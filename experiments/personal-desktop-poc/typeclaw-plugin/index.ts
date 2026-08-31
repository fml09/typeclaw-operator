import { definePlugin } from "typeclaw/plugin";
import { z } from "zod";

const configSchema = z.object({
  gatewayUrl: z.string().url().describe("HTTPS URL of the Personal Desktop Gateway"),
  issuer: z.string().min(1).max(512).describe("Exact OIDC issuer linked to this TypeClaw runtime"),
  subject: z
    .string()
    .min(1)
    .max(512)
    .describe("Stable OIDC subject linked to this TypeClaw runtime"),
  agentTokenEnv: z
    .string()
    .regex(/^[A-Z][A-Z0-9_]*$/)
    .default("PERSONAL_DESKTOP_AGENT_TOKEN"),
  browserSession: z
    .string()
    .regex(/^[a-zA-Z0-9_-]+$/)
    .default("personal-desktop-computer-use"),
  screenshotMaxWidth: z.number().int().min(320).max(1600).default(1024),
  screenshotMaxBytes: z.number().int().min(50_000).max(190_000).default(180_000),
});

const CONTROL_PERMISSION = "security.bypass.personalDesktopControl";
const GATEWAY_STATUS_TIMEOUT_MS = 8_000;
const GATEWAY_SCREENSHOT_TIMEOUT_MS = 15_000;
const GATEWAY_POWER_TIMEOUT_MS = 20_000;
const SERIALIZED_OPERATION_TIMEOUT_MS = 30_000;
const AGENT_BROWSER_ABORT_GRACE_MS = 250;
const DESKTOP_TOOL_NAMES = new Set([
  "desktop_status",
  "desktop_acquire",
  "desktop_observe",
  "desktop_click",
  "desktop_type",
  "desktop_key",
  "desktop_scroll",
  "desktop_power",
  "desktop_release",
]);

type PluginConfig = z.infer<typeof configSchema>;
type GatewayStatus = {
  desktopName: string;
  gatewayBootID?: string;
  vmExists?: boolean;
  vmiExists?: boolean;
  vmiPhase?: string;
  vmiUID?: string;
  controlActive?: boolean;
  controlActor?: "human" | "agent";
  controlGeneration?: number;
  controlBlocked?: boolean;
  powerRecoveryRequired?: boolean;
};

type BrowserBox = { x: number; y: number; width: number; height: number };
type Frame = {
  bytes: Uint8Array;
  mimeType: string;
  framebufferWidth: number;
  framebufferHeight: number;
  encodedWidth: number;
  encodedHeight: number;
  gatewayBootID: string;
  vmiUID: string;
  controlGeneration: number;
};
type ObservedFrame = Pick<
  Frame,
  "framebufferWidth" | "framebufferHeight" | "gatewayBootID" | "vmiUID" | "controlGeneration"
>;
type ObservationState = {
  frame?: ObservedFrame;
  observationId?: string;
  mustObserve: boolean;
};
type LocalControlLease = {
  sessionId: string;
  operationAbortController: AbortController;
  lifecycleAbortController: AbortController;
  closing: boolean;
  controlEstablished: boolean;
  gatewayBootID?: string;
  controlGeneration?: number;
  orphaned: boolean;
  cleanupReason?: string;
};

export function withDeadlineSignal(
  signal: AbortSignal | undefined,
  timeoutMs: number,
): AbortSignal {
  const deadline = AbortSignal.timeout(timeoutMs);
  return signal ? AbortSignal.any([signal, deadline]) : deadline;
}

export function createSerializedExecutor(timeoutMs = SERIALIZED_OPERATION_TIMEOUT_MS) {
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs <= 0) {
    throw new Error("serialized operation timeout must be a positive integer");
  }
  let queue: Promise<void> = Promise.resolve();
  const run = async <T>(
    signal: AbortSignal | undefined,
    operation: (operationSignal: AbortSignal) => Promise<T>,
  ): Promise<T> => {
    signal?.throwIfAborted();
    const checkedOperation = () => {
      signal?.throwIfAborted();
      return operation(withDeadlineSignal(signal, timeoutMs));
    };
    const result = queue.then(checkedOperation, checkedOperation);
    queue = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  };
  return { run, idle: () => queue };
}

export function observationIsFresh(
  observation: ObservationState,
  current: GatewayStatus,
  presentedObservationId: string,
): boolean {
  const frame = observation.frame;
  return Boolean(
    !observation.mustObserve &&
    frame &&
    observation.observationId &&
    presentedObservationId === observation.observationId &&
    current.gatewayBootID &&
    current.gatewayBootID === frame.gatewayBootID &&
    current.vmiUID &&
    current.vmiUID === frame.vmiUID &&
    current.controlGeneration === frame.controlGeneration,
  );
}

export function mapFramebufferPoint(
  box: BrowserBox,
  framebufferWidth: number,
  framebufferHeight: number,
  x: number,
  y: number,
): { x: number; y: number } {
  if (box.width <= 0 || box.height <= 0) throw new Error("no positive noVNC canvas area");
  const mapPixelCenter = (pixel: number, pixels: number, start: number, size: number) => {
    const minimum = Math.ceil(start);
    const maximum = Math.ceil(start + size) - 1;
    if (maximum < minimum) throw new Error("no integer viewport coordinate inside the canvas");
    const center = Math.floor(start + ((pixel + 0.5) / pixels) * size);
    return Math.min(maximum, Math.max(minimum, center));
  };
  return {
    x: mapPixelCenter(x, framebufferWidth, box.x, box.width),
    y: mapPixelCenter(y, framebufferHeight, box.y, box.height),
  };
}

export default definePlugin({
  permissions: [CONTROL_PERMISSION],
  configSchema,
  async plugin(ctx) {
    const config = normalizeConfig(ctx.config);
    const serializedExecutor = createSerializedExecutor();
    const serialized = serializedExecutor.run;
    let powerUncertain: Record<string, unknown> | undefined;
    let localControl: LocalControlLease | undefined;
    let disposing = false;
    const observations = new Map<string, ObservationState>();

    const observationFor = (sessionId: string): ObservationState => {
      let observation = observations.get(sessionId);
      if (!observation) {
        observation = { mustObserve: true };
        observations.set(sessionId, observation);
      }
      return observation;
    };

    const invalidateAllObservations = () => {
      for (const observation of observations.values()) {
        observation.frame = undefined;
        observation.observationId = undefined;
        observation.mustObserve = true;
      }
    };

    const status = (signal?: AbortSignal) =>
      gatewayJSON<GatewayStatus>(config, "/api/me", undefined, signal);

    const requirePowerCertain = () => {
      if (!powerUncertain) return;
      throw new Error(
        "PowerRecoveryRequired: a power action has UnknownOutcome. Inspect desktop_status, then call desktop_power with action=start explicitly before acquiring or sending input.",
      );
    };

    const requireGatewayControlAvailable = (current: GatewayStatus) => {
      if (!current.controlBlocked) return;
      throw new Error(
        "PowerRecoveryRequired: the Gateway has blocked control. Inspect desktop_status and call desktop_power with action=start explicitly.",
      );
    };

    const reserveLocalControl = (sessionId: string): LocalControlLease => {
      if (disposing) throw new Error("PluginUnavailable: Personal Desktop plugin is disposing");
      requirePowerCertain();
      if (localControl) {
        if (localControl.orphaned) {
          throw new Error(
            "ControlCleanupRequired: a previous Agent controller was not confirmed released. Call desktop_release before acquiring control.",
          );
        }
        if (localControl.sessionId !== sessionId || localControl.closing) {
          throw new Error(
            "ControlBusy: another TypeClaw session owns or is releasing agent input control.",
          );
        }
        return localControl;
      }
      localControl = {
        sessionId,
        operationAbortController: new AbortController(),
        lifecycleAbortController: new AbortController(),
        closing: false,
        controlEstablished: false,
        orphaned: false,
      };
      return localControl;
    };

    const requireLocalControl = (
      sessionId: string,
      allowPowerUncertain = false,
    ): LocalControlLease => {
      if (disposing) throw new Error("PluginUnavailable: Personal Desktop plugin is disposing");
      if (!allowPowerUncertain) requirePowerCertain();
      if (!localControl || localControl.sessionId !== sessionId || localControl.closing) {
        throw new Error(
          "ControlRequired: this TypeClaw session must call desktop_acquire before sending input.",
        );
      }
      return localControl;
    };

    const releaseLocalControl = (lease: LocalControlLease) => {
      if (localControl === lease) localControl = undefined;
    };

    const quarantineLocalControl = (lease: LocalControlLease, reason: string) => {
      if (localControl !== lease) return;
      lease.closing = true;
      lease.orphaned = true;
      lease.cleanupReason = reason;
      lease.operationAbortController.abort(
        new DOMException("agent input control cleanup is required", "AbortError"),
      );
      invalidateAllObservations();
    };

    const signalForLease = (lease: LocalControlLease, signal?: AbortSignal): AbortSignal =>
      AbortSignal.any(
        [
          signal,
          lease.operationAbortController.signal,
          lease.lifecycleAbortController.signal,
        ].filter((candidate): candidate is AbortSignal => Boolean(candidate)),
      );

    const closingSignalForLease = (
      lease: LocalControlLease,
      signal?: AbortSignal,
    ): AbortSignal =>
      signal
        ? AbortSignal.any([signal, lease.lifecycleAbortController.signal])
        : lease.lifecycleAbortController.signal;

    const assertCurrentLease = (lease: LocalControlLease, closing: boolean) => {
      if (localControl !== lease || lease.closing !== closing) {
        throw new Error("ControlLeaseChanged: the local input lease changed before execution");
      }
    };

    const beginLocalControlClose = (
      sessionId: string,
      allowPowerUncertain = false,
    ): LocalControlLease => {
      const lease = requireLocalControl(sessionId, allowPowerUncertain);
      lease.closing = true;
      lease.operationAbortController.abort(
        new DOMException("agent input control is closing", "AbortError"),
      );
      return lease;
    };

    const renewLocalControlAfterFailedClose = (lease: LocalControlLease) => {
      if (localControl !== lease || lease.lifecycleAbortController.signal.aborted || disposing) return;
      localControl = {
        sessionId: lease.sessionId,
        operationAbortController: new AbortController(),
        lifecycleAbortController: lease.lifecycleAbortController,
        closing: false,
        controlEstablished: lease.controlEstablished,
        gatewayBootID: lease.gatewayBootID,
        controlGeneration: lease.controlGeneration,
        orphaned: false,
      };
    };

    const serializedWithControl = <T>(
      sessionId: string,
      signal: AbortSignal | undefined,
      operation: (controlSignal: AbortSignal, lease: LocalControlLease) => Promise<T>,
      allowPowerUncertain = false,
    ): Promise<T> => {
      const lease = requireLocalControl(sessionId, allowPowerUncertain);
      const controlSignal = signalForLease(lease, signal);
      return serialized(controlSignal, (operationSignal) => {
        assertCurrentLease(lease, false);
        return operation(operationSignal, lease);
      });
    };

    const waitForControlRelease = async (signal?: AbortSignal): Promise<void> => {
      const deadline = Date.now() + 3_000;
      while (true) {
        signal?.throwIfAborted();
        if (!(await status(signal)).controlActive) return;
        if (Date.now() >= deadline) {
          throw new Error("ControlBusy: the Gateway has not acknowledged agent release");
        }
        await abortableDelay(100, signal);
      }
    };

    const waitForAgentRelease = async (signal?: AbortSignal): Promise<void> => {
      const deadline = Date.now() + 3_000;
      while (true) {
        signal?.throwIfAborted();
        const current = await status(signal);
        if (!current.controlActive || current.controlActor !== "agent") return;
        if (Date.now() >= deadline) {
          throw new Error("ControlBusy: the Gateway has not acknowledged agent release");
        }
        await abortableDelay(100, signal);
      }
    };

    const containAmbiguousInputForLease = async (
      lease: LocalControlLease,
      button?: "left" | "right" | "middle",
    ): Promise<AmbiguousInputCleanup> => {
      const cleanup = await containAmbiguousInput(config, button);
      if (cleanup.connectionClosed) {
        try {
          await waitForAgentRelease(AbortSignal.timeout(3_000));
          cleanup.gatewayReleaseConfirmed = true;
        } catch (error) {
          cleanup.errors.push(`Gateway release confirmation: ${String(error)}`);
        }
      }
      if (cleanup.gatewayReleaseConfirmed) {
        releaseLocalControl(lease);
      } else {
        quarantineLocalControl(lease, cleanup.errors.join("; ") || "Agent release was not confirmed");
      }
      return cleanup;
    };

    const requireExistingControl = async (signal?: AbortSignal): Promise<GatewayStatus> => {
      requirePowerCertain();
      const current = await status(signal);
      requireGatewayControlAvailable(current);
      if (current.controlActive && current.controlActor === "human") {
        throw new Error(
          "ControlBusy: a human owns input. The agent cannot preempt a human controller.",
        );
      }
      if (!current.controlActive || current.controlActor !== "agent") {
        throw new Error(
          "ControlRequired: call desktop_acquire, then desktop_observe, before sending input.",
        );
      }
      try {
        await browserBox(config, signal);
        await runAgentBrowser(config, ["wait", '#screen[data-control-connected="true"]'], signal);
      } catch {
        throw new Error("ControlBusy: another agent connection owns input control.");
      }
      return current;
    };

    const ensureControl = async (
      lease: LocalControlLease,
      signal?: AbortSignal,
    ): Promise<GatewayStatus> => {
      requirePowerCertain();
      const current = await status(signal);
      requireGatewayControlAvailable(current);
      if (current.controlActive) {
        if (current.controlActor === "human") {
          throw new Error(
            "ControlBusy: a human owns input. The agent cannot preempt a human controller.",
          );
        }
        if (!lease.controlEstablished) {
          quarantineLocalControl(
            lease,
            "the Gateway already reported an Agent controller before this local lease established it",
          );
          throw new Error(
            "ControlCleanupRequired: refusing to adopt an Agent controller from an earlier plugin lifecycle. Call desktop_release first.",
          );
        }
        const existing = await requireExistingControl(signal);
        if (
          !existing.gatewayBootID ||
          existing.gatewayBootID !== lease.gatewayBootID ||
          existing.controlGeneration !== lease.controlGeneration
        ) {
          quarantineLocalControl(
            lease,
            "the Gateway controller generation no longer matches this local lease",
          );
          throw new Error(
            "ControlCleanupRequired: Agent control changed outside this TypeClaw session. Call desktop_release before reacquiring.",
          );
        }
        return existing;
      }

      // A newly opened RFB connection changes input authority. Invalidate
      // explicitly as well as comparing the wire generation so a Gateway
      // restart cannot create an ABA match with a retained observation.
      invalidateAllObservations();
      const headers = JSON.stringify(agentHeaders(config));
      try {
        await runAgentBrowser(config, ["--headers", headers, "open", config.gatewayUrl], signal);
        // `--headers` is origin-scoped Fetch interception and does not cover the
        // noVNC WebSocket handshake in agent-browser 0.33.0. This dedicated
        // session also needs CDP Network extra headers before opening control.
        await runAgentBrowser(config, ["set", "headers", headers], signal);
        await runAgentBrowser(config, ["set", "viewport", "1440", "900"], signal);
        await runAgentBrowser(config, ["click", "#control"], signal);
        await runAgentBrowser(config, ["wait", '#screen[data-control-connected="true"]'], signal);
        await runAgentBrowser(config, ["focus", "#screen canvas"], signal);

        const granted = await status(signal);
        if (!granted.controlActive || granted.controlActor !== "agent") {
          throw new Error("the Gateway did not grant the agent exclusive input control");
        }
        if (!granted.gatewayBootID || granted.controlGeneration === undefined) {
          throw new Error("the Gateway omitted the controller generation after acquire");
        }
        lease.controlEstablished = true;
        lease.gatewayBootID = granted.gatewayBootID;
        lease.controlGeneration = granted.controlGeneration;
        return granted;
      } catch (error) {
        invalidateAllObservations();
        const cleanupErrors: string[] = [];
        try {
          await closeBrowser(config, AbortSignal.timeout(2_000));
        } catch (cleanupError) {
          cleanupErrors.push(`close: ${String(cleanupError)}`);
        }
        try {
          await waitForAgentRelease(AbortSignal.timeout(3_000));
        } catch (cleanupError) {
          cleanupErrors.push(`release: ${String(cleanupError)}`);
        }
        const cause = error instanceof Error ? error : new Error(String(error));
        const cleanup = cleanupErrors.length === 0 ? "cleanup confirmed" : cleanupErrors.join("; ");
        if (cleanupErrors.length > 0) quarantineLocalControl(lease, cleanup);
        throw new Error(`ControlSetupFailed: ${cause.message}; ${cleanup}`, { cause });
      }
    };

    const requireFreshObservation = (
      observation: ObservationState,
      current: GatewayStatus,
      presentedObservationId: string,
    ): ObservedFrame => {
      if (!observationIsFresh(observation, current, presentedObservationId)) {
        invalidateAllObservations();
        throw new Error(
          "FreshObservationRequired: control ownership or the VM changed. Call desktop_observe before sending input.",
        );
      }
      return observation.frame!;
    };

    return {
      hooks: {
        "session.end": async (event) => {
          observations.delete(event.sessionId);
          const lease = localControl;
          if (!lease || lease.sessionId !== event.sessionId) return;

          quarantineLocalControl(lease, "the controlling TypeClaw session ended before release");
          lease.lifecycleAbortController.abort(
            new DOMException("controlling TypeClaw session ended", "AbortError"),
          );
          const cleanupSignal = AbortSignal.timeout(8_000);
          try {
            await waitWithSignal(
              serialized(cleanupSignal, async (operationSignal) => {
                if (localControl !== lease) return;
                const cleanupErrors: string[] = [];
                try {
                  await closeBrowser(config, operationSignal);
                } catch (error) {
                  cleanupErrors.push(`close: ${String(error)}`);
                }
                try {
                  await waitForAgentRelease(operationSignal);
                } catch (error) {
                  cleanupErrors.push(`release: ${String(error)}`);
                }
                if (cleanupErrors.length > 0) throw new Error(cleanupErrors.join("; "));
                releaseLocalControl(lease);
              }),
              cleanupSignal,
            );
          } catch (error) {
            ctx.logger.warn(
              `failed to release Personal Desktop control for ended session ${event.sessionId}: ${String(error)}`,
            );
          }
        },
        "tool.before": (event) => {
          if (event.toolProvenance !== "plugin" || !DESKTOP_TOOL_NAMES.has(event.tool)) return;
          if (disposing) return { block: true, reason: "Personal Desktop plugin is disposing" };
          if (ctx.permissions.has(event.origin, CONTROL_PERMISSION)) return;
          return { block: true, reason: `missing ${CONTROL_PERMISSION}` };
        },
        "tool.after": (event) => {
          if (event.tool !== "desktop_observe") return;
          const observation = observationFor(event.sessionId);
          const observationId = (event.result.details as { observationId?: unknown } | undefined)
            ?.observationId;
          if (typeof observationId !== "string" || observation.observationId !== observationId) {
            return;
          }
          const imageReachedModel = event.result.content.some((part) => part.type === "image");
          observation.mustObserve = !imageReachedModel;
          if (!imageReachedModel) {
            observation.frame = undefined;
            observation.observationId = undefined;
          }
        },
      },
      tools: {
        desktop_status: {
          description:
            "Inspect the authenticated user’s persistent desktop, VM power state, and current input owner.",
          parameters: z.object({}),
          async execute(_args, toolCtx) {
            const current = await status(toolCtx.signal);
            const details = {
              ...current,
              pluginPowerUncertain: powerUncertain ?? null,
              pluginControlCleanupRequired: localControl?.orphaned
                ? {
                    reason: localControl.cleanupReason,
                    recoveryTool: "desktop_release",
                  }
                : null,
            };
            return {
              content: [{ type: "text", text: JSON.stringify(details, null, 2) }],
              details,
            };
          },
        },
        desktop_acquire: {
          description:
            "Acquire exclusive agent input control if it is free. This does not observe the screen; call desktop_observe in a later tool round before input.",
          parameters: z.object({}),
          async execute(_args, toolCtx) {
            const lease = reserveLocalControl(toolCtx.sessionId);
            const controlSignal = signalForLease(lease, toolCtx.signal);
            try {
              return await serialized(controlSignal, async (operationSignal) => {
                assertCurrentLease(lease, false);
                const granted = await ensureControl(lease, operationSignal);
                invalidateAllObservations();
                return {
                  content: [
                    {
                      type: "text",
                      text: "Agent control acquired for this TypeClaw session. Call desktop_observe before sending input.",
                    },
                  ],
                  details: granted,
                };
              });
            } catch (error) {
              if (!lease.closing) releaseLocalControl(lease);
              throw error;
            }
          },
        },
        desktop_observe: {
          description:
            "Capture the current XFCE framebuffer. Coordinates returned in details are the coordinate space for click actions.",
          parameters: z.object({}),
          async execute(_args, toolCtx) {
            return serialized(toolCtx.signal, async (operationSignal) => {
              const frame = await fetchFrame(config, operationSignal);
              const observationId = crypto.randomUUID();
              const observation = observationFor(toolCtx.sessionId);
              observation.frame = {
                framebufferWidth: frame.framebufferWidth,
                framebufferHeight: frame.framebufferHeight,
                gatewayBootID: frame.gatewayBootID,
                vmiUID: frame.vmiUID,
                controlGeneration: frame.controlGeneration,
              };
              observation.observationId = observationId;
              // tool-result-cap runs before this plugin's tool.after hook. The
              // hook clears this only if the image survives result capping.
              observation.mustObserve = true;
              const details = {
                framebufferWidth: frame.framebufferWidth,
                framebufferHeight: frame.framebufferHeight,
                encodedWidth: frame.encodedWidth,
                encodedHeight: frame.encodedHeight,
                encodedBytes: frame.bytes.byteLength,
                observationId,
                gatewayBootID: frame.gatewayBootID,
                vmiUID: frame.vmiUID,
                controlGeneration: frame.controlGeneration,
                warning: "A frame is an observation, not proof that a previous input took effect.",
              };
              return {
                content: [
                  {
                    type: "text" as const,
                    text: `Observation ${observationId}; framebuffer ${frame.framebufferWidth}×${frame.framebufferHeight}; encoded ${frame.encodedWidth}×${frame.encodedHeight}. Echo this observationId in exactly one input tool call.`,
                  },
                  {
                    type: "image" as const,
                    mimeType: frame.mimeType,
                    data: Buffer.from(frame.bytes).toString("base64"),
                  },
                ],
                details,
              };
            });
          },
        },
        desktop_click: {
          description:
            "Click absolute framebuffer coordinates using an existing agent control lease and the latest observationId. Never retry after an ambiguous connection failure.",
          parameters: z.object({
            observationId: z.string().uuid(),
            x: z.number().int().nonnegative(),
            y: z.number().int().nonnegative(),
            button: z.enum(["left", "right", "middle"]).default("left"),
            clicks: z.union([z.literal(1), z.literal(2)]).default(1),
          }),
          fileOperands: { nonFile: ["observationId", "button"] },
          async execute(args, toolCtx) {
            return serializedWithControl(toolCtx.sessionId, toolCtx.signal, async (signal, lease) => {
              const observation = observationFor(toolCtx.sessionId);
              const current = await requireExistingControl(signal);
              const observed = requireFreshObservation(observation, current, args.observationId);
              const [frame, box] = await Promise.all([
                fetchFrame(config, signal),
                browserBox(config, signal),
              ]);
              if (
                frame.vmiUID !== observed.vmiUID ||
                frame.controlGeneration !== observed.controlGeneration ||
                frame.gatewayBootID !== observed.gatewayBootID ||
                frame.framebufferWidth !== observed.framebufferWidth ||
                frame.framebufferHeight !== observed.framebufferHeight
              ) {
                invalidateAllObservations();
                throw new Error(
                  "FreshObservationRequired: the framebuffer changed after the last observation.",
                );
              }
              if (args.x >= observed.framebufferWidth || args.y >= observed.framebufferHeight) {
                throw new Error(
                  `coordinates (${args.x}, ${args.y}) exceed framebuffer ${observed.framebufferWidth}×${observed.framebufferHeight}`,
                );
              }
              const viewport = mapFramebufferPoint(
                box,
                observed.framebufferWidth,
                observed.framebufferHeight,
                args.x,
                args.y,
              );
              let buttonIsDown = false;
              try {
                await runAgentBrowser(
                  config,
                  ["mouse", "move", String(viewport.x), String(viewport.y)],
                  signal,
                );
                for (let index = 0; index < args.clicks; index += 1) {
                  buttonIsDown = true;
                  await runAgentBrowser(config, ["mouse", "down", args.button], signal);
                  await runAgentBrowser(config, ["mouse", "up", args.button], signal);
                  buttonIsDown = false;
                }
              } catch (error) {
                invalidateAllObservations();
                const cleanup = await containAmbiguousInputForLease(
                  lease,
                  buttonIsDown ? args.button : undefined,
                );
                return unknownOutcome("click", error, cleanup);
              }
              invalidateAllObservations();
              return {
                content: [
                  {
                    type: "text",
                    text: `Input dispatched at (${args.x}, ${args.y}). Observe before deciding whether it succeeded.`,
                  },
                ],
                details: {
                  dispatched: true,
                  x: args.x,
                  y: args.y,
                  outcome: "Unconfirmed",
                  retrySafe: false,
                },
              };
            });
          },
        },
        desktop_type: {
          description:
            "Type text with real key events into the currently focused guest application. Input effects remain unconfirmed until observe.",
          parameters: z.object({
            observationId: z.string().uuid(),
            text: z.string().min(1).max(4000),
          }),
          fileOperands: { nonFile: ["observationId", "text"] },
          async execute({ observationId, text }, toolCtx) {
            return serializedWithControl(toolCtx.sessionId, toolCtx.signal, async (signal, lease) => {
              const observation = observationFor(toolCtx.sessionId);
              requireFreshObservation(
                observation,
                await requireExistingControl(signal),
                observationId,
              );
              try {
                await runAgentBrowser(config, ["focus", "#screen canvas"], signal);
                await runAgentBrowser(config, ["keyboard", "type", text], signal);
              } catch (error) {
                invalidateAllObservations();
                const cleanup = await containAmbiguousInputForLease(lease);
                return unknownOutcome("type", error, cleanup);
              }
              invalidateAllObservations();
              return {
                content: [
                  {
                    type: "text",
                    text: `Typed ${[...text].length} character(s). Observe before continuing.`,
                  },
                ],
                details: { dispatched: true, outcome: "Unconfirmed", retrySafe: false },
              };
            });
          },
        },
        desktop_key: {
          description:
            "Press one guest key or key combination, such as Enter, Tab, Escape, Control+a, or Alt+F4.",
          parameters: z.object({
            observationId: z.string().uuid(),
            key: z
              .string()
              .min(1)
              .max(80)
              .regex(/^[A-Za-z0-9+_-]+$/),
          }),
          fileOperands: { nonFile: ["observationId", "key"] },
          async execute({ observationId, key }, toolCtx) {
            return serializedWithControl(toolCtx.sessionId, toolCtx.signal, async (signal, lease) => {
              const observation = observationFor(toolCtx.sessionId);
              requireFreshObservation(
                observation,
                await requireExistingControl(signal),
                observationId,
              );
              try {
                await runAgentBrowser(config, ["focus", "#screen canvas"], signal);
                await runAgentBrowser(config, ["press", key], signal);
              } catch (error) {
                invalidateAllObservations();
                const cleanup = await containAmbiguousInputForLease(lease);
                return unknownOutcome("key", error, cleanup);
              }
              invalidateAllObservations();
              return {
                content: [
                  { type: "text", text: `Key ${key} dispatched. Observe before continuing.` },
                ],
                details: { dispatched: true, outcome: "Unconfirmed", retrySafe: false },
              };
            });
          },
        },
        desktop_scroll: {
          description:
            "Move to absolute framebuffer coordinates and send a relative mouse-wheel event using the latest observationId.",
          parameters: z.object({
            observationId: z.string().uuid(),
            x: z.number().int().nonnegative(),
            y: z.number().int().nonnegative(),
            deltaY: z.number().int().min(-4000).max(4000),
            deltaX: z.number().int().min(-4000).max(4000).default(0),
          }),
          fileOperands: { nonFile: ["observationId"] },
          async execute({ observationId, x, y, deltaY, deltaX }, toolCtx) {
            return serializedWithControl(toolCtx.sessionId, toolCtx.signal, async (signal, lease) => {
              const observation = observationFor(toolCtx.sessionId);
              const observed = requireFreshObservation(
                observation,
                await requireExistingControl(signal),
                observationId,
              );
              const [frame, box] = await Promise.all([
                fetchFrame(config, signal),
                browserBox(config, signal),
              ]);
              if (
                frame.vmiUID !== observed.vmiUID ||
                frame.controlGeneration !== observed.controlGeneration ||
                frame.gatewayBootID !== observed.gatewayBootID ||
                frame.framebufferWidth !== observed.framebufferWidth ||
                frame.framebufferHeight !== observed.framebufferHeight
              ) {
                invalidateAllObservations();
                throw new Error(
                  "FreshObservationRequired: the framebuffer changed after the last observation.",
                );
              }
              if (x >= observed.framebufferWidth || y >= observed.framebufferHeight) {
                throw new Error(
                  `coordinates (${x}, ${y}) exceed framebuffer ${observed.framebufferWidth}×${observed.framebufferHeight}`,
                );
              }
              const viewport = mapFramebufferPoint(
                box,
                observed.framebufferWidth,
                observed.framebufferHeight,
                x,
                y,
              );
              try {
                await runAgentBrowser(
                  config,
                  ["mouse", "move", String(viewport.x), String(viewport.y)],
                  signal,
                );
                await runAgentBrowser(
                  config,
                  ["mouse", "wheel", String(deltaY), String(deltaX)],
                  signal,
                );
              } catch (error) {
                invalidateAllObservations();
                const cleanup = await containAmbiguousInputForLease(lease);
                return unknownOutcome("scroll", error, cleanup);
              }
              invalidateAllObservations();
              return {
                content: [
                  {
                    type: "text",
                    text: `Wheel input dispatched at (${x}, ${y}). Observe before continuing.`,
                  },
                ],
                details: { dispatched: true, x, y, outcome: "Unconfirmed", retrySafe: false },
              };
            });
          },
        },
        desktop_power: {
          description:
            "Start or gracefully stop the persistent desktop VM. Stop preserves the whole-root PVC but not RAM, processes, or unsaved buffers.",
          parameters: z.object({ action: z.enum(["start", "stop"]) }),
          fileOperands: { nonFile: ["action"] },
          async execute({ action }, toolCtx) {
            if (disposing) throw new Error("PluginUnavailable: Personal Desktop plugin is disposing");
            if (powerUncertain && action !== "start") {
              throw new Error(
                "PowerRecoveryRequired: only an explicit desktop_power start may clear an UnknownOutcome quarantine.",
              );
            }

            const leaseAtInvocation = localControl;
            let stopLease: LocalControlLease | undefined;
            let startLease: LocalControlLease | undefined;
            if (action === "stop" && leaseAtInvocation) {
              stopLease = beginLocalControlClose(toolCtx.sessionId);
            } else if (action === "start" && leaseAtInvocation) {
              if (
                leaseAtInvocation.sessionId !== toolCtx.sessionId ||
                leaseAtInvocation.closing
              ) {
                throw new Error(
                  "ControlBusy: another TypeClaw session owns or is releasing agent input control.",
                );
              }
              startLease = leaseAtInvocation;
            }

            const operationSignal = stopLease
              ? closingSignalForLease(stopLease, toolCtx.signal)
              : startLease
                ? signalForLease(startLease, toolCtx.signal)
                : toolCtx.signal;
            let controlReleaseConfirmed = false;
            try {
              return await serialized(operationSignal, async (boundedOperationSignal) => {
                if (disposing) {
                  throw new Error("PluginUnavailable: Personal Desktop plugin is disposing");
                }
                if (stopLease) {
                  assertCurrentLease(stopLease, true);
                } else if (startLease) {
                  assertCurrentLease(startLease, false);
                } else if (localControl) {
                  throw new Error(
                    "ControlBusy: a TypeClaw session acquired input control before the power operation executed.",
                  );
                }
                if (powerUncertain && action !== "start") {
                  throw new Error(
                    "PowerRecoveryRequired: only an explicit desktop_power start may clear an UnknownOutcome quarantine.",
                  );
                }
                if (action === "stop") {
                  await closeBrowser(config, boundedOperationSignal);
                  await waitForControlRelease(boundedOperationSignal);
                  controlReleaseConfirmed = true;
                }
                boundedOperationSignal.throwIfAborted();
                agentHeaders(config); // Validate local credentials before POST dispatch.
                try {
                  const result = await gatewayJSON<Record<string, unknown>>(
                    config,
                    `/api/power/${action}`,
                    { method: "POST" },
                    boundedOperationSignal,
                  );
                  invalidateAllObservations();
                  if (action === "start") powerUncertain = undefined;
                  return {
                    content: [{ type: "text", text: JSON.stringify(result) }],
                    details: result,
                  };
                } catch (error) {
                  invalidateAllObservations();
                  if (
                    error instanceof GatewayHTTPError &&
                    error.body.outcome === "UnknownOutcome"
                  ) {
                    const unknown = powerUnknownOutcome(action, error, error.body);
                    powerUncertain = {
                      ...unknown.details,
                      recordedAt: new Date().toISOString(),
                    };
                    return unknown;
                  }
                  if (
                    !(error instanceof GatewayHTTPError) ||
                    isAmbiguousPowerHTTPStatus(error.status)
                  ) {
                    const unknown = powerUnknownOutcome(action, error, {
                      controlBlocked: "unknown",
                      transportError: error instanceof Error ? error.message : String(error),
                    });
                    powerUncertain = {
                      ...unknown.details,
                      recordedAt: new Date().toISOString(),
                    };
                    return unknown;
                  }
                  throw error;
                }
              });
            } catch (error) {
              if (stopLease && !controlReleaseConfirmed) {
                renewLocalControlAfterFailedClose(stopLease);
              }
              throw error;
            } finally {
              if (stopLease && controlReleaseConfirmed) releaseLocalControl(stopLease);
            }
          },
        },
        desktop_release: {
          description:
            "Release agent input control so a human can take over. Any input whose acknowledgement was lost has UnknownOutcome and must not be replayed.",
          parameters: z.object({}),
          async execute(_args, toolCtx) {
            const orphanedLease = localControl?.orphaned ? localControl : undefined;
            if (orphanedLease) {
              const timeoutSignal = AbortSignal.timeout(8_000);
              const signal = toolCtx.signal
                ? AbortSignal.any([toolCtx.signal, timeoutSignal])
                : timeoutSignal;
              return serialized(signal, async (operationSignal) => {
                if (localControl !== orphanedLease || !orphanedLease.orphaned) {
                  throw new Error(
                    "ControlLeaseChanged: the quarantined controller changed before cleanup",
                  );
                }
                await closeBrowser(config, operationSignal);
                invalidateAllObservations();
                await waitForAgentRelease(operationSignal);
                releaseLocalControl(orphanedLease);
                return {
                  content: [
                    {
                      type: "text",
                      text: "Orphaned Agent control cleanup confirmed by the Gateway. A later session may now acquire a fresh controller.",
                    },
                  ],
                  details: { releaseConfirmed: true, orphanedControlRecovered: true },
                };
              });
            }
            const lease = beginLocalControlClose(toolCtx.sessionId, true);
            const signal = closingSignalForLease(lease, toolCtx.signal);
            try {
              return await serialized(signal, async (operationSignal) => {
                assertCurrentLease(lease, true);
                await closeBrowser(config, operationSignal);
                invalidateAllObservations();
                await waitForAgentRelease(operationSignal);
                releaseLocalControl(lease);
                return {
                  content: [
                    {
                      type: "text",
                      text: "Agent control release confirmed by the Gateway. In-flight input, if any, has UnknownOutcome.",
                    },
                  ],
                  details: { releaseConfirmed: true },
                };
              });
            } catch (error) {
              renewLocalControlAfterFailedClose(lease);
              throw error;
            }
          },
        },
      },
      onDispose: async () => {
        disposing = true;
        observations.clear();
        const lease = localControl;
        if (lease) {
          lease.closing = true;
          lease.operationAbortController.abort(new DOMException("plugin disposed", "AbortError"));
          lease.lifecycleAbortController.abort(new DOMException("plugin disposed", "AbortError"));
        }
        const drainSignal = AbortSignal.timeout(8_000);
        try {
          await waitWithSignal(serializedExecutor.idle(), drainSignal);
        } catch (error) {
          ctx.logger.warn(`timed out draining Personal Desktop tool queue: ${String(error)}`);
        }
        const cleanupSignal = AbortSignal.timeout(6_000);
        try {
          await closeBrowser(config, cleanupSignal);
          await waitForAgentRelease(cleanupSignal);
        } catch (error) {
          ctx.logger.warn(`failed to release Personal Desktop browser session: ${String(error)}`);
        } finally {
          if (lease) releaseLocalControl(lease);
        }
      },
    };
  },
});

function normalizeConfig(config: PluginConfig): PluginConfig {
  const gateway = new URL(config.gatewayUrl);
  if (!["https:", "http:"].includes(gateway.protocol))
    throw new Error("gatewayUrl must use HTTP or HTTPS");
  if (gateway.pathname !== "/" || gateway.search || gateway.hash)
    throw new Error("gatewayUrl must be an origin without a path, query, or fragment");
  gateway.search = "";
  gateway.hash = "";
  return { ...config, gatewayUrl: gateway.toString().replace(/\/$/, "") };
}

function token(config: PluginConfig): string {
  const value = process.env[config.agentTokenEnv];
  if (!value || value.length < 24)
    throw new Error(`${config.agentTokenEnv} must contain the Gateway PoC agent token`);
  return value;
}

function agentHeaders(config: PluginConfig): Record<string, string> {
  return {
    Authorization: `Bearer ${token(config)}`,
    "X-Personal-Desktop-Issuer": config.issuer,
    "X-Personal-Desktop-Subject": config.subject,
  };
}

type AmbiguousInputCleanup = {
  mouseUpAttempted: boolean;
  mouseUpConfirmed: boolean;
  connectionClosed: boolean;
  gatewayReleaseConfirmed: boolean;
  errors: string[];
};

async function containAmbiguousInput(
  config: PluginConfig,
  button?: "left" | "right" | "middle",
): Promise<AmbiguousInputCleanup> {
  const cleanup: AmbiguousInputCleanup = {
    mouseUpAttempted: Boolean(button),
    mouseUpConfirmed: false,
    connectionClosed: false,
    gatewayReleaseConfirmed: false,
    errors: [],
  };
  if (button) {
    try {
      await runAgentBrowser(config, ["mouse", "up", button], AbortSignal.timeout(1_500));
      cleanup.mouseUpConfirmed = true;
    } catch (error) {
      cleanup.errors.push(`mouse-up cleanup: ${String(error)}`);
    }
  }
  try {
    await closeBrowser(config, AbortSignal.timeout(2_000));
    cleanup.connectionClosed = true;
  } catch (error) {
    cleanup.errors.push(`connection cleanup: ${String(error)}`);
  }
  return cleanup;
}

function unknownOutcome(action: string, error: unknown, cleanup: AmbiguousInputCleanup) {
  const message = error instanceof Error ? error.message : String(error);
  return {
    content: [
      {
        type: "text" as const,
        text: `UnknownOutcome: ${action} may have reached the guest. Do not retry it automatically; observe first. ${message}`,
      },
    ],
    details: {
      dispatched: "possibly",
      outcome: "UnknownOutcome",
      retrySafe: false,
      error: message,
      cleanup,
    },
  };
}

function isAmbiguousPowerHTTPStatus(status: number): boolean {
  return status >= 500 || status === 408 || status === 425 || status === 429;
}

export function powerUnknownOutcome(
  action: "start" | "stop",
  error: unknown,
  sourceDetails: Record<string, unknown> = {},
) {
  const message = error instanceof Error ? error.message : String(error);
  const details = {
    ...sourceDetails,
    action,
    outcome: "UnknownOutcome",
    retrySafe: false,
    controlBlocked: sourceDetails.controlBlocked ?? "unknown",
  };
  return {
    content: [
      {
        type: "text" as const,
        text: `UnknownOutcome: ${action} may have been accepted. Do not retry automatically; inspect VM state and recover explicitly. ${message}`,
      },
    ],
    details,
  };
}

async function gatewayJSON<T>(
  config: PluginConfig,
  path: string,
  init?: RequestInit,
  signal?: AbortSignal,
): Promise<T> {
  const headers = new Headers(init?.headers);
  for (const [name, value] of Object.entries(agentHeaders(config))) headers.set(name, value);
  const timeoutMs = init?.method === "POST" ? GATEWAY_POWER_TIMEOUT_MS : GATEWAY_STATUS_TIMEOUT_MS;
  const response = await fetch(`${config.gatewayUrl}${path}`, {
    ...init,
    headers,
    signal: withDeadlineSignal(signal, timeoutMs),
  });
  const body = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  if (!response.ok) throw gatewayFailure(response, body);
  return body as T;
}

async function fetchFrame(config: PluginConfig, signal?: AbortSignal): Promise<Frame> {
  const query = new URLSearchParams({
    format: "jpeg",
    maxWidth: String(config.screenshotMaxWidth),
    maxBytes: String(config.screenshotMaxBytes),
    quality: "65",
  });
  const response = await fetch(`${config.gatewayUrl}/api/screenshot?${query}`, {
    headers: agentHeaders(config),
    signal: withDeadlineSignal(signal, GATEWAY_SCREENSHOT_TIMEOUT_MS),
  });
  if (!response.ok) {
    const body = (await response.json().catch(() => ({}))) as Record<string, unknown>;
    throw gatewayFailure(response, body);
  }
  const bytes = new Uint8Array(await response.arrayBuffer());
  if (bytes.byteLength > config.screenshotMaxBytes)
    throw new Error(`Gateway screenshot exceeded ${config.screenshotMaxBytes} bytes`);
  return {
    bytes,
    mimeType: response.headers.get("content-type") || "image/jpeg",
    framebufferWidth: requiredHeaderInt(response, "X-Framebuffer-Width"),
    framebufferHeight: requiredHeaderInt(response, "X-Framebuffer-Height"),
    encodedWidth: requiredHeaderInt(response, "X-Encoded-Width"),
    encodedHeight: requiredHeaderInt(response, "X-Encoded-Height"),
    gatewayBootID: requiredHeader(response, "X-Gateway-Boot-ID"),
    vmiUID: requiredHeader(response, "X-VMI-UID"),
    controlGeneration: requiredHeaderNonNegativeInt(response, "X-Control-Generation"),
  };
}

function requiredHeader(response: Response, name: string): string {
  const value = response.headers.get(name);
  if (!value) throw new Error(`Gateway response omitted ${name}`);
  return value;
}

class GatewayHTTPError extends Error {
  constructor(
    readonly status: number,
    readonly body: Record<string, unknown>,
    message: string,
  ) {
    super(message);
    this.name = "GatewayHTTPError";
  }
}

function gatewayFailure(response: Response, body: Record<string, unknown>): Error {
  const detail = String(body.detail || body.error || response.statusText);
  return new GatewayHTTPError(response.status, body, `Gateway HTTP ${response.status}: ${detail}`);
}

function requiredHeaderInt(response: Response, name: string): number {
  const value = Number(response.headers.get(name));
  if (!Number.isSafeInteger(value) || value <= 0)
    throw new Error(`Gateway response omitted ${name}`);
  return value;
}

function requiredHeaderNonNegativeInt(response: Response, name: string): number {
  const raw = response.headers.get(name);
  const value = raw === null || raw === "" ? Number.NaN : Number(raw);
  if (!Number.isSafeInteger(value) || value < 0)
    throw new Error(`Gateway response omitted ${name}`);
  return value;
}

async function browserBox(config: PluginConfig, signal?: AbortSignal): Promise<BrowserBox> {
  const output = await runAgentBrowser(config, ["--json", "get", "box", "#screen canvas"], signal);
  const result = JSON.parse(output) as {
    success?: boolean;
    data?: Partial<BrowserBox>;
    error?: string;
  };
  const box = result.data;
  const x = box?.x;
  const y = box?.y;
  const width = box?.width;
  const height = box?.height;
  if (
    !result.success ||
    ![x, y, width, height].every((value) => typeof value === "number" && Number.isFinite(value)) ||
    typeof width !== "number" ||
    typeof height !== "number" ||
    width <= 0 ||
    height <= 0
  ) {
    throw new Error(result.error || "no active noVNC canvas");
  }
  return { x: x as number, y: y as number, width, height };
}

function abortableDelay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted();
  return new Promise((resolve, reject) => {
    const timer = setTimeout(done, milliseconds);
    signal?.addEventListener("abort", aborted, { once: true });
    function done() {
      signal?.removeEventListener("abort", aborted);
      resolve();
    }
    function aborted() {
      clearTimeout(timer);
      reject(signal?.reason ?? new DOMException("Aborted", "AbortError"));
    }
  });
}

function waitWithSignal<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  signal.throwIfAborted();
  return new Promise<T>((resolve, reject) => {
    const aborted = () => reject(signal.reason ?? new DOMException("Aborted", "AbortError"));
    signal.addEventListener("abort", aborted, { once: true });
    promise.then(resolve, reject).finally(() => signal.removeEventListener("abort", aborted));
  });
}

async function runAgentBrowser(
  config: PluginConfig,
  args: string[],
  signal?: AbortSignal,
): Promise<string> {
  signal?.throwIfAborted();
  const processHandle = Bun.spawn({
    cmd: ["agent-browser", "--session", config.browserSession, ...args],
    stdout: "pipe",
    stderr: "pipe",
    env: { ...process.env },
  });
  const abort = () => processHandle.kill();
  signal?.addEventListener("abort", abort, { once: true });
  if (signal?.aborted) abort();
  const completion = Promise.all([
    processHandle.exited,
    new Response(processHandle.stdout).text(),
    new Response(processHandle.stderr).text(),
  ]);
  try {
    const [exitCode, output, errorOutput] = signal
      ? await waitWithSignal(completion, signal)
      : await completion;
    if (exitCode !== 0)
      throw new Error(errorOutput.trim() || output.trim() || `agent-browser exited ${exitCode}`);
    return output.trim();
  } catch (error) {
    if (signal?.aborted) {
      processHandle.kill();
      try {
        await waitWithSignal(completion, AbortSignal.timeout(AGENT_BROWSER_ABORT_GRACE_MS));
      } catch {
        try {
          processHandle.kill(9);
        } catch {
          // The process may already have exited between the grace timeout and SIGKILL.
        }
        try {
          await waitWithSignal(completion, AbortSignal.timeout(AGENT_BROWSER_ABORT_GRACE_MS));
        } catch {
          // The queue is bounded even if the OS never reports exit for a killed child.
        }
      }
    }
    throw error;
  } finally {
    signal?.removeEventListener("abort", abort);
  }
}

async function closeBrowser(config: PluginConfig, signal?: AbortSignal): Promise<void> {
  try {
    await runAgentBrowser(config, ["close"], signal);
  } catch (error) {
    if (!String(error).includes("No active")) throw error;
  }
}
