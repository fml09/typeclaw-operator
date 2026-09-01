import { Buffer } from "node:buffer";

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
  screenshotMaxWidth: z.number().int().min(320).max(1600).default(1024),
  screenshotMaxBytes: z.number().int().min(50_000).max(190_000).default(180_000),
});

const CONTROL_PERMISSION = "security.bypass.personalDesktopControl";
const GATEWAY_STATUS_TIMEOUT_MS = 8_000;
const GATEWAY_SCREENSHOT_TIMEOUT_MS = 15_000;
const GATEWAY_ACTION_TIMEOUT_MS = 12_000;
const GATEWAY_BATCH_TIMEOUT_MS = 32_000;
const GATEWAY_POWER_TIMEOUT_MS = 20_000;
const SERIALIZED_OPERATION_TIMEOUT_MS = 45_000;
const MAX_ACTIONS_PER_BATCH = 16;
const MAX_IMAGE_RESULT_BASE64_BYTES = 262_144;
const MAX_BATCH_TEXT_CHARS = 4000;

const desktopActionSchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("click"),
    x: z.number().int().nonnegative(),
    y: z.number().int().nonnegative(),
    button: z.enum(["left", "right", "middle"]).default("left"),
    clicks: z.union([z.literal(1), z.literal(2)]).default(1),
  }),
  z.object({
    type: z.literal("type"),
    text: z.string().min(1).max(MAX_BATCH_TEXT_CHARS),
  }),
  z.object({
    type: z.literal("key"),
    key: z
      .string()
      .min(1)
      .max(80)
      .regex(/^[A-Za-z0-9+_-]+$/),
  }),
  z.object({
    type: z.literal("scroll"),
    x: z.number().int().nonnegative(),
    y: z.number().int().nonnegative(),
    deltaY: z.number().int().min(-4000).max(4000),
    deltaX: z.number().int().min(-4000).max(4000).default(0),
  }),
]);

const desktopActionBatchSchema = z
  .array(desktopActionSchema)
  .min(1)
  .max(MAX_ACTIONS_PER_BATCH)
  .superRefine((actions, validation) => {
    const textCharacters = actions.reduce(
      (total, action) => total + (action.type === "type" ? [...action.text].length : 0),
      0,
    );
    if (textCharacters > MAX_BATCH_TEXT_CHARS) {
      validation.addIssue({
        code: z.ZodIssueCode.custom,
        message: `action batch may type at most ${MAX_BATCH_TEXT_CHARS} characters`,
      });
    }
  });
type DesktopAction = z.infer<typeof desktopActionSchema>;

const DESKTOP_TOOL_NAMES: Record<string, true> = {
  desktop_status: true,
  desktop_acquire: true,
  desktop_observe: true,
  desktop_act: true,
  desktop_launch: true,
  desktop_windows: true,
  desktop_power: true,
  desktop_release: true,
};
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

function observationMatchesCurrent(
  observation: ObservationState,
  current: GatewayStatus,
  presentedObservationId: string,
): boolean {
  const frame = observation.frame;
  return Boolean(
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

export function observationIsFresh(
  observation: ObservationState,
  current: GatewayStatus,
  presentedObservationId: string,
): boolean {
  return observationMatchesCurrent(observation, current, presentedObservationId);
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
        observation = {};
        observations.set(sessionId, observation);
      }
      return observation;
    };

    const invalidateObservation = (observation: ObservationState) => {
      observation.frame = undefined;
      observation.observationId = undefined;
    };

    const invalidateAllObservations = () => {
      for (const observation of observations.values()) invalidateObservation(observation);
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
      return current;
    };

    const ensureControl = async (
      lease: LocalControlLease,
      signal?: AbortSignal,
    ): Promise<Record<string, unknown>> => {
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
        return current as unknown as Record<string, unknown>;
      }

      invalidateAllObservations();
      try {
        const granted = await gatewayJSON<Record<string, unknown>>(
          config,
          "/api/control/acquire",
          { method: "POST" },
          signal,
          GATEWAY_STATUS_TIMEOUT_MS,
        );
        const bootID = granted.gatewayBootID;
        const generation = granted.controlGeneration;
        if (
          typeof bootID !== "string" ||
          bootID.length === 0 ||
          typeof generation !== "number" ||
          !Number.isSafeInteger(generation) ||
          generation < 0
        ) {
          throw new Error("the Gateway omitted the controller generation after acquire");
        }
        lease.controlEstablished = true;
        lease.gatewayBootID = bootID;
        lease.controlGeneration = generation;
        return granted;
      } catch (error) {
        invalidateAllObservations();
        // A definitive rejection means no lease of ours exists; nothing to
        // clean up. An ambiguous dispatch (lost response) may have created a
        // Gateway lease that this local lease cannot adopt, so try to release
        // it and quarantine only if the cleanup is not confirmed.
        const ambiguous =
          error instanceof GatewayHTTPError && error.body.outcome === "UnknownOutcome";
        if (!ambiguous) throw error;
        const cleanupErrors: string[] = [];
        try {
          await releaseAgentRequest(config, AbortSignal.timeout(3_000));
        } catch (cleanupError) {
          cleanupErrors.push(`release: ${String(cleanupError)}`);
        }
        try {
          await waitForAgentRelease(AbortSignal.timeout(3_000));
        } catch (cleanupError) {
          cleanupErrors.push(`release confirmation: ${String(cleanupError)}`);
        }
        const cleanup = cleanupErrors.length === 0 ? "cleanup confirmed" : cleanupErrors.join("; ");
        if (cleanupErrors.length > 0) quarantineLocalControl(lease, cleanup);
        const cause = error instanceof Error ? error : new Error(String(error));
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
          const observation = observations.get(event.sessionId);
          if (observation) invalidateObservation(observation);
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
                  await releaseAgentRequest(config, operationSignal);
                } catch (error) {
                  cleanupErrors.push(`release: ${String(error)}`);
                }
                try {
                  await waitForAgentRelease(operationSignal);
                } catch (error) {
                  cleanupErrors.push(`release confirmation: ${String(error)}`);
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
          if (event.toolProvenance !== "plugin" || !(event.tool in DESKTOP_TOOL_NAMES)) return;
          if (disposing) return { block: true, reason: "Personal Desktop plugin is disposing" };
          if (ctx.permissions.has(event.origin, CONTROL_PERMISSION)) return;
          return { block: true, reason: `missing ${CONTROL_PERMISSION}` };
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
            "Acquire exclusive agent input control if it is free. This does not observe the screen; call desktop_observe before input.",
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
            "Capture the current desktop and return a bounded JPEG directly to the image-capable main model. This observation unlocks one ordered desktop_act batch. Coordinates use the framebuffer dimensions returned here.",
          parameters: z.object({}),
          async execute(_args, toolCtx) {
            return serialized(toolCtx.signal, async (operationSignal) => {
              const frame = await fetchFrame(config, operationSignal);
              const observationId = crypto.randomUUID();
              const imageData = Buffer.from(frame.bytes).toString("base64");
              if (imageData.length > MAX_IMAGE_RESULT_BASE64_BYTES) {
                throw new Error(
                  `encoded screenshot exceeded ${MAX_IMAGE_RESULT_BASE64_BYTES} base64 bytes`,
                );
              }
              const observation = observationFor(toolCtx.sessionId);
              observation.frame = {
                framebufferWidth: frame.framebufferWidth,
                framebufferHeight: frame.framebufferHeight,
                gatewayBootID: frame.gatewayBootID,
                vmiUID: frame.vmiUID,
                controlGeneration: frame.controlGeneration,
              };
              observation.observationId = observationId;
              const details = {
                framebufferWidth: frame.framebufferWidth,
                framebufferHeight: frame.framebufferHeight,
                encodedWidth: frame.encodedWidth,
                encodedHeight: frame.encodedHeight,
                encodedBytes: frame.bytes.byteLength,
                imageBase64Bytes: imageData.length,
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
                    text: [
                      `Observation ${observationId}; screen ${frame.framebufferWidth}×${frame.framebufferHeight}; encoded ${frame.encodedWidth}×${frame.encodedHeight}.`,
                      "The attached image is the current desktop. Interpret coordinates in the full framebuffer coordinate space.",
                      "Echo this observationId in exactly one desktop_act call.",
                    ].join(" "),
                  },
                  {
                    type: "image" as const,
                    mimeType: frame.mimeType,
                    data: imageData,
                  },
                ],
                details,
              };
            });
          },
        },
        desktop_act: {
          description:
            "Execute one bounded ordered action batch from the latest observed frame. Group deterministic click/type/key/scroll steps that do not require another visual decision; stop before any target whose position depends on a UI change. Never retry after an ambiguous or partial dispatch failure.",
          parameters: z.object({
            observationId: z.string().uuid(),
            actions: desktopActionBatchSchema,
          }),
          fileOperands: { nonFile: ["observationId", "actions"] },
          async execute({ observationId, actions }, toolCtx) {
            return serializedWithControl(toolCtx.sessionId, toolCtx.signal, async (signal) => {
              const observation = observationFor(toolCtx.sessionId);
              const observed = requireFreshObservation(
                observation,
                await requireExistingControl(signal),
                observationId,
              );
              const normalizedActions = actions.map((action: DesktopAction) => {
                if (action.type === "click") {
                  if (
                    action.x >= observed.framebufferWidth ||
                    action.y >= observed.framebufferHeight
                  ) {
                    throw new Error(
                      `coordinates (${action.x}, ${action.y}) exceed screen ${observed.framebufferWidth}×${observed.framebufferHeight}`,
                    );
                  }
                  return {
                    ...action,
                    button: action.button ?? "left",
                    clicks: action.clicks ?? 1,
                  };
                }
                if (action.type === "scroll") {
                  if (
                    action.x >= observed.framebufferWidth ||
                    action.y >= observed.framebufferHeight
                  ) {
                    throw new Error(
                      `coordinates (${action.x}, ${action.y}) exceed screen ${observed.framebufferWidth}×${observed.framebufferHeight}`,
                    );
                  }
                  return { ...action, deltaX: action.deltaX ?? 0 };
                }
                return action;
              });

              try {
                const result = await gatewayJSON<Record<string, unknown>>(
                  config,
                  "/api/agent/actions",
                  {
                    method: "POST",
                    body: JSON.stringify({ actions: normalizedActions }),
                  },
                  signal,
                  GATEWAY_BATCH_TIMEOUT_MS,
                );
                invalidateAllObservations();
                const applied = result.applied === true;
                const outcome = applied
                  ? "Applied"
                  : result.outcome === "Partial"
                    ? "Partial"
                    : "NotApplied";
                const completedActions =
                  typeof result.completedActions === "number" ? result.completedActions : 0;
                return {
                  content: [
                    {
                      type: "text",
                      text: applied
                        ? `${completedActions} ordered desktop action(s) applied. Observe before continuing.`
                        : `Desktop action batch ended with ${outcome} after ${completedActions} completed action(s). Do not retry automatically; observe first.`,
                    },
                  ],
                  details: {
                    ...result,
                    dispatched: true,
                    applied,
                    outcome,
                    retrySafe: false,
                    actionCount: normalizedActions.length,
                    completedActions,
                  },
                };
              } catch (error) {
                invalidateAllObservations();
                return handleActionFailure("action batch", error);
              }
            });
          },
        },
        desktop_launch: {
          description:
            "Launch an installed application on the desktop, such as firefox, terminal, or files. Requires an agent control lease but no observation; observe afterwards to see the result.",
          parameters: z.object({
            app: z.string().regex(/^[a-z0-9][a-z0-9._-]{0,63}$/),
          }),
          fileOperands: { nonFile: ["app"] },
          async execute({ app }, toolCtx) {
            return serializedWithControl(toolCtx.sessionId, toolCtx.signal, async (signal) => {
              await requireExistingControl(signal);
              try {
                const result = await gatewayJSON<Record<string, unknown>>(
                  config,
                  "/api/agent/launch",
                  { method: "POST", body: JSON.stringify({ app }) },
                  signal,
                  GATEWAY_ACTION_TIMEOUT_MS,
                );
                invalidateAllObservations();
                return {
                  content: [
                    {
                      type: "text",
                      text: `Launch request applied for ${app}. Observe before continuing.`,
                    },
                  ],
                  details: {
                    dispatched: true,
                    applied: result.applied === true,
                    outcome: "Applied",
                    retrySafe: false,
                    app,
                  },
                };
              } catch (error) {
                invalidateAllObservations();
                return handleActionFailure("launch", error);
              }
            });
          },
        },
        desktop_windows: {
          description:
            "List the titles of top-level application windows currently on the desktop. View-only; it does not require or grant input control.",
          parameters: z.object({}),
          async execute(_args, toolCtx) {
            const result = await gatewayJSON<{ windows?: Array<Record<string, unknown>> }>(
              config,
              "/api/agent/windows",
              undefined,
              toolCtx.signal,
              GATEWAY_ACTION_TIMEOUT_MS,
            );
            const windows = Array.isArray(result.windows) ? result.windows : [];
            return {
              content: [{ type: "text", text: JSON.stringify(windows, null, 2) }],
              details: { windows },
            };
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
                  await releaseAgentRequest(config, boundedOperationSignal);
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
                await releaseAgentRequest(config, operationSignal);
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
                await releaseAgentRequest(config, operationSignal);
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
        observations.clear();
        const cleanupSignal = AbortSignal.timeout(6_000);
        try {
          await releaseAgentRequest(config, cleanupSignal);
          await waitForAgentRelease(cleanupSignal);
        } catch (error) {
          ctx.logger.warn(`failed to release Personal Desktop agent control: ${String(error)}`);
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

function handleActionFailure(action: string, error: unknown): never | {
  content: Array<{ type: "text"; text: string }>;
  details: Record<string, unknown>;
} {
  // A Gateway-authored UnknownOutcome means the dispatch may or may not have
  // reached the guest agent. The lease is kept; the next observe resolves the
  // ambiguity. Every other failure is a definitive non-applied result.
  if (!(error instanceof GatewayHTTPError) || error.body.outcome !== "UnknownOutcome") {
    throw error;
  }
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
  timeoutMs?: number,
): Promise<T> {
  const headers = new Headers(init?.headers);
  for (const [name, value] of Object.entries(agentHeaders(config))) headers.set(name, value);
  const timeout = timeoutMs ?? (init?.method === "POST" ? GATEWAY_POWER_TIMEOUT_MS : GATEWAY_STATUS_TIMEOUT_MS);
  const response = await fetch(`${config.gatewayUrl}${path}`, {
    ...init,
    headers,
    signal: withDeadlineSignal(signal, timeout),
  });
  const body = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  if (!response.ok) throw gatewayFailure(response, body);
  return body as T;
}

async function releaseAgentRequest(config: PluginConfig, signal?: AbortSignal): Promise<void> {
  await gatewayJSON<Record<string, unknown>>(
    config,
    "/api/control/release",
    { method: "POST" },
    signal,
    GATEWAY_STATUS_TIMEOUT_MS,
  );
}


async function fetchFrame(config: PluginConfig, signal?: AbortSignal): Promise<Frame> {
  const query = new URLSearchParams({
    format: "jpeg",
    maxWidth: String(config.screenshotMaxWidth),
    maxBytes: String(config.screenshotMaxBytes),
    quality: "65",
  });
  const response = await fetch(`${config.gatewayUrl}/api/agent/screenshot?${query}`, {
    method: "POST",
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

function abortableDelay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted();
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  const timer = setTimeout(resolve, milliseconds);
  signal?.addEventListener(
    "abort",
    () => {
      clearTimeout(timer);
      reject(signal.reason ?? new DOMException("Aborted", "AbortError"));
    },
    { once: true },
  );
  return promise;
}

function waitWithSignal<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  signal.throwIfAborted();
  const { promise: tracked, resolve, reject } = Promise.withResolvers<T>();
  const aborted = () => reject(signal.reason ?? new DOMException("Aborted", "AbortError"));
  signal.addEventListener("abort", aborted, { once: true });
  promise.then(resolve, reject).finally(() => signal.removeEventListener("abort", aborted));
  return tracked;
}
