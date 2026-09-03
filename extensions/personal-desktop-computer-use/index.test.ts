import { describe, expect, test } from "bun:test";

import pluginDefinition, {
  createSerializedExecutor,
  observationIsFresh,
  powerUnknownOutcome,
  resolvePluginConfig,
  withDeadlineSignal,
} from "./index";

const frame = {
  framebufferWidth: 1280,
  framebufferHeight: 720,
  gatewayBootID: "gateway-boot-a",
  vmiUID: "vmi-a",
  controlGeneration: 3,
};

const current = {
  desktopName: "pd-test",
  gatewayBootID: frame.gatewayBootID,
  vmiUID: frame.vmiUID,
  controlGeneration: frame.controlGeneration,
};

describe("observation freshness", () => {
  test("requires the unpredictable ID returned by a completed observe", () => {
    const observation = {
      frame,
      observationId: "f2a7c4ca-5197-4eaa-a2c8-4850f5123560",
    };

    expect(observationIsFresh(observation, current, "same-batch-does-not-know-new-id")).toBe(false);
    expect(observationIsFresh(observation, current, observation.observationId)).toBe(true);
  });

  test("rejects a Gateway restart ABA", () => {
    const observation = {
      frame,
      observationId: "f2a7c4ca-5197-4eaa-a2c8-4850f5123560",
    };
    expect(
      observationIsFresh(
        observation,
        { ...current, gatewayBootID: "gateway-boot-after-restart" },
        observation.observationId,
      ),
    ).toBe(false);
  });

  test("defines both possible old-ID orderings without claiming batch isolation", () => {
    const oldObservation = {
      frame,
      observationId: "a9042f98-6cd3-48b7-b79a-f9e64e4227c3",
    };
    const newObservation = {
      frame,
      observationId: "ea658f02-c788-46a7-be92-946e544cf71f",
    };

    // If input enters the serialized queue first, it may legitimately use the
    // frame the model saw in the previous inference.
    expect(observationIsFresh(oldObservation, current, oldObservation.observationId)).toBe(true);
    // If observe enters first, replacing A with B, input carrying A is stale.
    expect(observationIsFresh(newObservation, current, oldObservation.observationId)).toBe(false);
  });
});

describe("power lost-ACK semantics", () => {
  test("transport loss after POST is explicit and never retry-safe", () => {
    const result = powerUnknownOutcome("stop", new TypeError("connection reset"), {
      controlBlocked: "unknown",
    });
    expect(result.details.outcome).toBe("UnknownOutcome");
    expect(result.details.retrySafe).toBe(false);
    expect(result.details.controlBlocked).toBe("unknown");
    expect(result.content[0].text).toContain("Do not retry automatically");
  });
});

describe("bounded desktop operations", () => {
  test("an internal deadline advances the serialized queue", async () => {
    const executor = createSerializedExecutor(20);
    let secondStarted = false;
    const first = executor.run(
      undefined,
      (signal) =>
        new Promise<never>((_resolve, reject) => {
          signal.throwIfAborted();
          signal.addEventListener("abort", () => reject(signal.reason), { once: true });
        }),
    );
    const second = executor.run(undefined, async (signal) => {
      signal.throwIfAborted();
      secondStarted = true;
      return "continued";
    });

    const [firstResult, secondResult] = await Promise.allSettled([first, second]);
    expect(firstResult.status).toBe("rejected");
    expect(secondResult).toEqual({ status: "fulfilled", value: "continued" });
    expect(secondStarted).toBe(true);
  });

  test("a caller-less operation still receives an aborting deadline signal", async () => {
    // Real-timer behavior on purpose: this asserts AbortSignal.timeout against
    // the platform clock, which fake timers cannot exercise.
    const signal = withDeadlineSignal(undefined, 10);
    expect(signal.aborted).toBe(false);
    await Bun.sleep(30);
    expect(signal.aborted).toBe(true);
  });
});

describe("configuration resolution", () => {
  test("falls back to the operator-injected environment when typeclaw.json is absent", () => {
    const resolution = resolvePluginConfig(undefined, {
      PERSONAL_DESKTOP_GATEWAY_URL: "http://pd-desktop-gateway.desktops.svc:8080",
      PERSONAL_DESKTOP_CONSOLE_URL: "https://pd.tailnet.ts.net",
      PERSONAL_DESKTOP_SCREENSHOT_MAX_WIDTH: "1280",
      PERSONAL_DESKTOP_SCREENSHOT_MAX_BYTES: "150000",
    });
    expect(resolution).toEqual({
      config: {
        gatewayUrl: "http://pd-desktop-gateway.desktops.svc:8080",
        agentTokenEnv: "PERSONAL_DESKTOP_AGENT_TOKEN",
        screenshotMaxWidth: 1280,
        screenshotMaxBytes: 150_000,
        consoleUrl: "https://pd.tailnet.ts.net",
      },
    });
  });

  test("applies defaults when neither config nor environment sets a value", () => {
    const resolution = resolvePluginConfig(undefined, {
      PERSONAL_DESKTOP_GATEWAY_URL: "https://desktop.example/",
    });
    expect(resolution).toEqual({
      config: {
        gatewayUrl: "https://desktop.example",
        agentTokenEnv: "PERSONAL_DESKTOP_AGENT_TOKEN",
        screenshotMaxWidth: 1024,
        screenshotMaxBytes: 180_000,
        consoleUrl: undefined,
      },
    });
  });

  // The Agent Folder is the agent's own writable state. A gatewayUrl read from
  // there would let the model choose which machine it drives and who sees the
  // frames, so the platform's value wins whenever it set one.
  test("the platform Gateway URL wins over one in typeclaw.json", () => {
    const warnings: string[] = [];
    const resolution = resolvePluginConfig(
      {
        gatewayUrl: "http://personal-desktop-gateway.typeclaw.svc.cluster.local",
        agentTokenEnv: "DESKTOP_TOKEN",
        screenshotMaxWidth: 800,
        screenshotMaxBytes: 90_000,
        consoleUrl: "https://configured-console.example",
      },
      {
        PERSONAL_DESKTOP_GATEWAY_URL: "https://from-env.example",
        PERSONAL_DESKTOP_CONSOLE_URL: "https://console-from-env.example",
        PERSONAL_DESKTOP_SCREENSHOT_MAX_WIDTH: "1600",
        PERSONAL_DESKTOP_SCREENSHOT_MAX_BYTES: "190000",
      },
      { warn: (message) => warnings.push(message) },
    );
    expect(resolution).toEqual({
      config: {
        gatewayUrl: "https://from-env.example",
        agentTokenEnv: "DESKTOP_TOKEN",
        screenshotMaxWidth: 800,
        screenshotMaxBytes: 90_000,
        consoleUrl: "https://configured-console.example",
      },
    });
    expect(warnings).toHaveLength(1);
    expect(warnings[0]).toContain("personal-desktop-gateway.typeclaw.svc.cluster.local");
  });

  // A host-mode runtime has no operator injecting anything, so typeclaw.json is
  // the only way to configure the extension at all.
  test("typeclaw.json still supplies the Gateway URL when the platform set none", () => {
    expect(
      resolvePluginConfig({ gatewayUrl: "https://configured.example" }, {}),
    ).toEqual({
      config: {
        gatewayUrl: "https://configured.example",
        agentTokenEnv: "PERSONAL_DESKTOP_AGENT_TOKEN",
        screenshotMaxWidth: 1024,
        screenshotMaxBytes: 180_000,
        consoleUrl: undefined,
      },
    });
  });

  test("a missing Gateway URL names the environment variable instead of failing to load", () => {
    expect(resolvePluginConfig(undefined, {})).toEqual({
      unavailable: "PERSONAL_DESKTOP_GATEWAY_URL is not set",
    });
    expect(resolvePluginConfig({}, { PERSONAL_DESKTOP_GATEWAY_URL: "   " })).toEqual({
      unavailable: "PERSONAL_DESKTOP_GATEWAY_URL is not set",
    });
  });

  test("a malformed value is reported per source rather than crashing the runtime", () => {
    expect(resolvePluginConfig(undefined, { PERSONAL_DESKTOP_GATEWAY_URL: "not a url" })).toEqual({
      unavailable: "PERSONAL_DESKTOP_GATEWAY_URL is not a valid URL",
    });
    expect(
      resolvePluginConfig(undefined, { PERSONAL_DESKTOP_GATEWAY_URL: "https://desktop.example/api" }),
    ).toEqual({
      unavailable:
        "PERSONAL_DESKTOP_GATEWAY_URL must be an origin without a path, query, or fragment",
    });
    expect(
      resolvePluginConfig({ gatewayUrl: "ftp://desktop.example" }, {}),
    ).toEqual({ unavailable: "gatewayUrl in typeclaw.json must use HTTP or HTTPS" });
    expect(
      resolvePluginConfig(undefined, {
        PERSONAL_DESKTOP_GATEWAY_URL: "https://desktop.example",
        PERSONAL_DESKTOP_SCREENSHOT_MAX_BYTES: "12",
      }),
    ).toEqual({
      unavailable: "PERSONAL_DESKTOP_SCREENSHOT_MAX_BYTES must be an integer between 50000 and 190000",
    });
  });
});

type ToolResult = {
  content: Array<{ type: string; text?: string; mimeType?: string; data?: string }>;
  details: Record<string, unknown>;
};
type DesktopTool = {
  execute(args: Record<string, unknown>, toolCtx: { sessionId: string }): Promise<ToolResult>;
};
type ChannelCommand = {
  description: string;
  aliases?: readonly string[];
  permission?: string;
  run(commandCtx: never): Promise<string | void>;
};
// Unchecked cast target: pluginDefinition.plugin() returns an untyped runtime;
// this shape pins exactly what these tests assert.
type PluginRuntime = {
  hooks: Record<
    "session.end" | "tool.before",
    (event: unknown, ctx: never) => Promise<unknown>
  >;
  tools: Record<string, DesktopTool>;
  channelCommands: Record<string, ChannelCommand>;
  onDispose?: () => Promise<void>;
};
type RecordedRequest = { path: string; body: string };

interface PluginHarness {
  runtime: PluginRuntime;
  requests: RecordedRequest[];
  warnings: string[];
  fetchSignals: Array<AbortSignal | null | undefined>;
  readonly agentActive: boolean;
  readonly generation: number;
  releaseCallCount(): number;
  acquireCallCount(): number;
  restore(): Promise<void>;
}

interface HarnessOptions {
  initiallyControlled?: boolean;
  acquireFailures?: number;
  releaseFailures?: number;
  statusFailuresAfterRelease?: number;
  batchFailures?: number;
  partialBatch?: boolean;
  powerResponse?: (action: "start" | "stop") => Response | Promise<Response>;
  config?: Record<string, unknown>;
  env?: Record<string, string | undefined>;
  statusExtras?: Record<string, unknown>;
}

// Runtime narrowing helper: tool details are Record<string, unknown>, and
// these assertions only need the object-typed entries.
function detailObject(details: Record<string, unknown>, key: string): Record<string, unknown> {
  const value = details[key];
  return value && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

async function createPluginHarness(options?: HarnessOptions): Promise<PluginHarness> {
  const originalFetch = globalThis.fetch;
  const environment: Record<string, string | undefined> = {
    PERSONAL_DESKTOP_AGENT_TOKEN: "t".repeat(32),
    PERSONAL_DESKTOP_GATEWAY_URL: "https://desktop.example",
    PERSONAL_DESKTOP_CONSOLE_URL: undefined,
    PERSONAL_DESKTOP_SCREENSHOT_MAX_WIDTH: undefined,
    PERSONAL_DESKTOP_SCREENSHOT_MAX_BYTES: undefined,
    ...options?.env,
  };
  const originalEnv = new Map<string, string | undefined>();
  for (const [name, value] of Object.entries(environment)) {
    originalEnv.set(name, process.env[name]);
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }

  let agentActive = options?.initiallyControlled ?? false;
  let generation = agentActive ? 1 : 0;
  let acquireFailures = options?.acquireFailures ?? 0;
  let releaseFailures = options?.releaseFailures ?? 0;
  let statusFailures = 0;
  let batchFailures = options?.batchFailures ?? 0;
  const requests: RecordedRequest[] = [];
  const warnings: string[] = [];
  const fetchSignals: Array<AbortSignal | null | undefined> = [];

  const unknownOutcome = (status: number) =>
    new Response(JSON.stringify({ error: "simulated transport loss", outcome: "UnknownOutcome" }), {
      status,
      headers: { "Content-Type": "application/json" },
    });

  globalThis.fetch = (async (input, init) => {
    fetchSignals.push(init?.signal);
    const url = new URL(String(input));
    const path = url.pathname;
    const body = init?.body ? String(init.body) : "";
    requests.push({ path, body });

    const respond = (payload: unknown, status = 200) =>
      new Response(JSON.stringify(payload), {
        status,
        headers: { "Content-Type": "application/json" },
      });

    if (path === "/api/agent/actions") {
      if (batchFailures > 0) {
        batchFailures -= 1;
        return unknownOutcome(502);
      }
      const parsed = JSON.parse(body) as { actions: Array<Record<string, unknown>> };
      if (options?.partialBatch) {
        return respond({
          applied: false,
          outcome: "Partial",
          retrySafe: false,
          actionCount: parsed.actions.length,
          completedActions: 1,
          failedActionIndex: 1,
        });
      }
      return respond({ applied: true, completedActions: parsed.actions.length });
    }
    if (path === "/api/me") {
      if (statusFailures > 0) {
        statusFailures -= 1;
        return respond({ detail: "simulated status failure" }, 503);
      }
      return respond({
        desktopName: "pd-test",
        os: "linux",
        gatewayBootID: "gateway-test",
        vmExists: true,
        vmPrintableStatus: "Running",
        vmiExists: true,
        vmiPhase: "Running",
        vmiUID: "vmi-test",
        controlGeneration: generation,
        controlActive: agentActive,
        controlActor: agentActive ? "agent" : undefined,
        ...options?.statusExtras,
      });
    }
    if (path === "/api/control/acquire") {
      if (acquireFailures > 0) {
        acquireFailures -= 1;
        return unknownOutcome(502);
      }
      agentActive = true;
      generation += 1;
      return respond({
        desktopName: "pd-test",
        gatewayBootID: "gateway-test",
        controlGeneration: generation,
        actor: "agent",
        leaseTtlSeconds: 120,
      });
    }
    if (path === "/api/control/release") {
      if (releaseFailures > 0) {
        releaseFailures -= 1;
        statusFailures += options?.statusFailuresAfterRelease ?? 0;
        return respond({ detail: "simulated release failure" }, 503);
      }
      agentActive = false;
      return respond({ desktopName: "pd-test", released: true });
    }
    if (path === "/api/agent/screenshot") {
      return new Response(new Uint8Array([0xff, 0xd8, 0xff, 0xd9]), {
        headers: {
          "Content-Type": "image/jpeg",
          "X-Framebuffer-Width": "1280",
          "X-Framebuffer-Height": "720",
          "X-Encoded-Width": "1024",
          "X-Encoded-Height": "576",
          "X-Gateway-Boot-ID": "gateway-test",
          "X-VMI-UID": "vmi-test",
          "X-Control-Generation": String(generation),
        },
      });
    }
    if (path === "/api/agent/launch") {
      return respond({ applied: true });
    }
    if (path === "/api/agent/windows") {
      return respond({ windows: [{ id: "0x03c00002", desktop: 0, title: "Firefox" }] });
    }
    const power = path.match(/^\/api\/power\/(start|stop)$/)?.[1] as "start" | "stop" | undefined;
    if (power) {
      if (options?.powerResponse) return options.powerResponse(power);
      return respond({ desktopName: "pd-test", action: power, idempotent: false }, 202);
    }
    throw new Error(`unexpected fetch: ${url.href}`);
  }) as typeof fetch;

  // Unchecked casts: pluginDefinition.plugin() returns an untyped runtime, and
  // the test config/hook shapes cannot satisfy its parameter type exactly.
  const runtime = (await pluginDefinition.plugin({
    config: options?.config,
    permissions: { has: () => true },
    logger: {
      info: () => undefined,
      warn: (message: string) => warnings.push(message),
      error: () => undefined,
    },
  } as never)) as unknown as PluginRuntime;

  return {
    runtime,
    requests,
    warnings,
    fetchSignals,
    get agentActive() {
      return agentActive;
    },
    get generation() {
      return generation;
    },
    releaseCallCount() {
      return requests.filter((request) => request.path === "/api/control/release").length;
    },
    acquireCallCount() {
      return requests.filter((request) => request.path === "/api/control/acquire").length;
    },
    async restore() {
      try {
        await runtime.onDispose?.();
      } finally {
        globalThis.fetch = originalFetch;
        for (const [name, value] of originalEnv) {
          if (value === undefined) delete process.env[name];
          else process.env[name] = value;
        }
      }
    },
  };
}

const toolContext = (sessionId: string) => ({ sessionId }) as never;

const channelContext = (
  args: string,
  overrides?: { sessionId?: string | null; permitted?: boolean },
) =>
  ({
    args,
    sessionId: overrides?.sessionId ?? null,
    invokerId: "U0DESKTOP",
    origin: { kind: "channel", adapter: "slack", workspace: "w", chat: "c", thread: null },
    adapter: "slack",
    permissions: { has: () => overrides?.permitted ?? true },
    logger: { info: () => undefined, warn: () => undefined, error: () => undefined },
    signal: new AbortController().signal,
  }) as never;

async function acquire(harness: PluginHarness, sessionId = "session-a") {
  return harness.runtime.tools.desktop_acquire.execute({}, toolContext(sessionId));
}

async function observe(harness: PluginHarness, sessionId = "session-a") {
  return harness.runtime.tools.desktop_observe.execute({}, toolContext(sessionId));
}

async function runDesktopCommand(
  harness: PluginHarness,
  args: string,
  overrides?: { sessionId?: string | null; permitted?: boolean },
) {
  return harness.runtime.channelCommands.desktop.run(channelContext(args, overrides));
}

describe("session-bound local control lease", () => {
  test("Gateway reads carry a plugin-owned deadline without a caller signal", async () => {
    const harness = await createPluginHarness();
    try {
      await harness.runtime.tools.desktop_status.execute({}, toolContext("status-session"));
      await harness.runtime.tools.desktop_observe.execute({}, toolContext("observe-session"));
      expect(harness.fetchSignals).toHaveLength(2);
      expect(harness.fetchSignals.every((signal) => signal instanceof AbortSignal)).toBe(true);
    } finally {
      await harness.restore();
    }
  });

  test("a lost acquire response triggers cleanup and a later fresh acquire", async () => {
    const harness = await createPluginHarness({ acquireFailures: 1 });
    try {
      await expect(acquire(harness, "lost-response")).rejects.toThrow("ControlSetupFailed");
      // The ambiguous acquire attempted to release the Gateway lease it may
      // have created before failing.
      expect(harness.releaseCallCount()).toBe(1);
      expect(harness.agentActive).toBe(false);

      await acquire(harness, "lost-response");
      expect(harness.agentActive).toBe(true);
      expect(harness.acquireCallCount()).toBe(2);
    } finally {
      await harness.restore();
    }
  });

  test("a fresh plugin lifecycle refuses to adopt an existing Agent controller", async () => {
    const harness = await createPluginHarness({ initiallyControlled: true });
    try {
      await expect(acquire(harness, "new-runtime-session")).rejects.toThrow(
        "ControlCleanupRequired",
      );
      expect(harness.acquireCallCount()).toBe(0);

      await harness.runtime.tools.desktop_release.execute({}, toolContext("new-runtime-session"));
      expect(harness.agentActive).toBe(false);
    } finally {
      await harness.restore();
    }
  });

  test("release invalidates a queued same-session acquire instead of creating an untracked controller", async () => {
    const harness = await createPluginHarness();
    try {
      await acquire(harness);
      const queuedAcquire = acquire(harness);
      const release = harness.runtime.tools.desktop_release.execute({}, toolContext("session-a"));
      const [acquireResult, releaseResult] = await Promise.allSettled([queuedAcquire, release]);

      expect(acquireResult.status).toBe("rejected");
      expect(releaseResult.status).toBe("fulfilled");
      expect(harness.agentActive).toBe(false);
    } finally {
      await harness.restore();
    }
  });

  test("a view-only session end does not release the writer, but the owner session end does", async () => {
    const harness = await createPluginHarness();
    try {
      await acquire(harness, "writer");
      const releasesBefore = harness.releaseCallCount();

      await harness.runtime.hooks["session.end"]({ sessionId: "viewer" }, {} as never);
      expect(harness.agentActive).toBe(true);
      expect(harness.releaseCallCount()).toBe(releasesBefore);

      await harness.runtime.hooks["session.end"]({ sessionId: "writer" }, {} as never);
      expect(harness.agentActive).toBe(false);
      expect(harness.releaseCallCount()).toBe(releasesBefore + 1);
    } finally {
      await harness.restore();
    }
  });

  test("stop rejection cannot hand a stale lease to a queued acquire", async () => {
    const harness = await createPluginHarness({
      powerResponse() {
        return new Response(JSON.stringify({ detail: "power denied" }), {
          status: 403,
          headers: { "Content-Type": "application/json" },
        });
      },
    });
    try {
      await acquire(harness);
      const queuedAcquire = acquire(harness);
      const stop = harness.runtime.tools.desktop_power.execute(
        { action: "stop" },
        toolContext("session-a"),
      );
      const [acquireResult, stopResult] = await Promise.allSettled([queuedAcquire, stop]);

      expect(acquireResult.status).toBe("rejected");
      expect(stopResult.status).toBe("rejected");
      expect(harness.agentActive).toBe(false);
      expect(harness.releaseCallCount()).toBeGreaterThanOrEqual(1);
    } finally {
      await harness.restore();
    }
  });

  test("failed session-end cleanup quarantines the old controller instead of adopting it", async () => {
    const harness = await createPluginHarness({
      releaseFailures: 1,
      statusFailuresAfterRelease: 1,
    });
    try {
      await acquire(harness, "writer");
      await harness.runtime.hooks["session.end"]({ sessionId: "writer" }, {} as never);
      expect(harness.agentActive).toBe(true);
      expect(harness.warnings.some((warning) => warning.includes("failed to release"))).toBe(true);

      await expect(acquire(harness, "next-session")).rejects.toThrow("ControlCleanupRequired");
      const quarantined = await harness.runtime.tools.desktop_status.execute(
        {},
        toolContext("next-session"),
      );
      expect(detailObject(quarantined.details, "pluginControlCleanupRequired").recoveryTool).toBe(
        "desktop_release",
      );

      const recovered = await harness.runtime.tools.desktop_release.execute(
        {},
        toolContext("next-session"),
      );
      expect(recovered.details.orphanedControlRecovered).toBe(true);
      expect(harness.agentActive).toBe(false);

      await acquire(harness, "next-session");
      expect(harness.agentActive).toBe(true);
    } finally {
      await harness.restore();
    }
  });
});

describe("typed guest agent actions", () => {
  test("an observation returns the image and directly unlocks one action batch", async () => {
    const harness = await createPluginHarness();
    try {
      await acquire(harness);
      const observed = await harness.runtime.tools.desktop_observe.execute(
        {},
        toolContext("session-a"),
      );
      const image = observed.content.find((part) => part.type === "image");
      expect(image).toEqual({
        type: "image",
        mimeType: "image/jpeg",
        data: "/9j/2Q==",
      });
      expect(observed.details.imagePath).toBeUndefined();

      const result = await harness.runtime.tools.desktop_act.execute(
        {
          observationId: observed.details.observationId,
          actions: [{ type: "click", x: 10, y: 20 }],
        },
        toolContext("session-a"),
      );
      expect(result.details.outcome).toBe("Applied");
    } finally {
      await harness.restore();
    }
  });

  test("input requires acquire, a fresh observation, and in-bounds coordinates", async () => {
    const harness = await createPluginHarness();
    try {
      await expect(
        harness.runtime.tools.desktop_act.execute(
          {
            observationId: "f2a7c4ca-5197-4eaa-a2c8-4850f5123560",
            actions: [{ type: "click", x: 10, y: 10 }],
          },
          toolContext("session-a"),
        ),
      ).rejects.toThrow("ControlRequired");

      await acquire(harness);
      await expect(
        harness.runtime.tools.desktop_act.execute(
          {
            observationId: "f2a7c4ca-5197-4eaa-a2c8-4850f5123560",
            actions: [{ type: "click", x: 10, y: 10 }],
          },
          toolContext("session-a"),
        ),
      ).rejects.toThrow("FreshObservationRequired");

      const observed = await observe(harness);
      await expect(
        harness.runtime.tools.desktop_act.execute(
          {
            observationId: observed.details.observationId,
            actions: [{ type: "click", x: 1280, y: 10 }],
          },
          toolContext("session-a"),
        ),
      ).rejects.toThrow("exceed screen 1280×720");

      const result = await harness.runtime.tools.desktop_act.execute(
        {
          observationId: observed.details.observationId,
          actions: [{ type: "click", x: 10, y: 20 }],
        },
        toolContext("session-a"),
      );
      expect(result.details.outcome).toBe("Applied");
      expect(result.details.applied).toBe(true);
      expect(harness.requests.at(-1)).toEqual({
        path: "/api/agent/actions",
        body: JSON.stringify({
          actions: [{ type: "click", x: 10, y: 20, button: "left", clicks: 1 }],
        }),
      });
    } finally {
      await harness.restore();
    }
  });

  test("one observed frame unlocks one ordered action batch", async () => {
    const harness = await createPluginHarness();
    try {
      await acquire(harness);
      const observed = await observe(harness);
      const actions = [
        { type: "click", x: 10, y: 20, button: "left", clicks: 1 },
        { type: "type", text: "hello" },
        { type: "key", key: "Enter" },
      ];

      const result = await harness.runtime.tools.desktop_act.execute(
        { observationId: observed.details.observationId, actions },
        toolContext("session-a"),
      );

      expect(result.details.outcome).toBe("Applied");
      expect(result.details.completedActions).toBe(3);
      expect(harness.requests.slice(-2)).toEqual([
        { path: "/api/me", body: "" },
        { path: "/api/agent/actions", body: JSON.stringify({ actions }) },
      ]);
    } finally {
      await harness.restore();
    }
  });

  test("an ambiguous dispatch returns UnknownOutcome and keeps the lease observable", async () => {
    const harness = await createPluginHarness({ batchFailures: 1 });
    try {
      await acquire(harness, "writer");
      const observed = await observe(harness, "writer");
      const result = await harness.runtime.tools.desktop_act.execute(
        {
          observationId: observed.details.observationId,
          actions: [{ type: "type", text: "hello" }],
        },
        toolContext("writer"),
      );
      expect(result.details.outcome).toBe("UnknownOutcome");
      expect(result.details.retrySafe).toBe(false);
      expect(harness.agentActive).toBe(true);

      const refreshed = await observe(harness, "writer");
      const retried = await harness.runtime.tools.desktop_act.execute(
        {
          observationId: refreshed.details.observationId,
          actions: [{ type: "type", text: "hello" }],
        },
        toolContext("writer"),
      );
      expect(retried.details.outcome).toBe("Applied");
    } finally {
      await harness.restore();
    }
  });

  test("a partial guest batch is not retryable and consumes the observation", async () => {
    const harness = await createPluginHarness({ partialBatch: true });
    try {
      await acquire(harness);
      const observed = await observe(harness);
      const args = {
        observationId: observed.details.observationId,
        actions: [
          { type: "type", text: "hello" },
          { type: "key", key: "Enter" },
        ],
      };

      const result = await harness.runtime.tools.desktop_act.execute(
        args,
        toolContext("session-a"),
      );
      expect(result.details.outcome).toBe("Partial");
      expect(result.details.completedActions).toBe(1);
      expect(result.details.retrySafe).toBe(false);
      await expect(
        harness.runtime.tools.desktop_act.execute(args, toolContext("session-a")),
      ).rejects.toThrow("FreshObservationRequired");
    } finally {
      await harness.restore();
    }
  });

  test("a launch requires the lease and a windows listing stays view-only", async () => {
    const harness = await createPluginHarness();
    try {
      await expect(
        harness.runtime.tools.desktop_launch.execute({ app: "firefox" }, toolContext("session-a")),
      ).rejects.toThrow("ControlRequired");

      await acquire(harness);
      const launched = await harness.runtime.tools.desktop_launch.execute(
        { app: "firefox" },
        toolContext("session-a"),
      );
      expect(launched.details.outcome).toBe("Applied");
      expect(harness.requests.at(-1)).toEqual({
        path: "/api/agent/launch",
        body: JSON.stringify({ app: "firefox" }),
      });

      const windows = await harness.runtime.tools.desktop_windows.execute(
        {},
        toolContext("viewer-session"),
      );
      expect(windows.details.windows).toEqual([{ id: "0x03c00002", desktop: 0, title: "Firefox" }]);
    } finally {
      await harness.restore();
    }
  });
});

describe("plugin power quarantine", () => {
  test("transport loss blocks acquire and stop until an explicit start succeeds", async () => {
    let loseStopResponse = true;
    const harness = await createPluginHarness({
      powerResponse(action) {
        if (action === "stop" && loseStopResponse) {
          loseStopResponse = false;
          throw new TypeError("connection reset after POST");
        }
        return new Response(JSON.stringify({ action, idempotent: action === "start" }), {
          status: 202,
          headers: { "Content-Type": "application/json" },
        });
      },
    });
    try {
      const unknown = await harness.runtime.tools.desktop_power.execute(
        { action: "stop" },
        toolContext("session-a"),
      );
      expect(unknown.details.outcome).toBe("UnknownOutcome");
      expect(unknown.details.retrySafe).toBe(false);

      await expect(acquire(harness)).rejects.toThrow("PowerRecoveryRequired");
      await expect(
        harness.runtime.tools.desktop_power.execute(
          { action: "stop" },
          toolContext("session-a"),
        ),
      ).rejects.toThrow("PowerRecoveryRequired");

      const quarantined = await harness.runtime.tools.desktop_status.execute(
        {},
        toolContext("session-a"),
      );
      expect(detailObject(quarantined.details, "pluginPowerUncertain").outcome).toBe(
        "UnknownOutcome",
      );

      await harness.runtime.tools.desktop_power.execute(
        { action: "start" },
        toolContext("session-a"),
      );
      const recovered = await harness.runtime.tools.desktop_status.execute(
        {},
        toolContext("session-a"),
      );
      expect(recovered.details.pluginPowerUncertain).toBeNull();
    } finally {
      await harness.restore();
    }
  });
});

describe("desktop_status passthrough", () => {
  test("reports the guest OS and the Gateway's console URL", async () => {
    const harness = await createPluginHarness({
      statusExtras: { consoleURL: "https://from-gateway.ts.net" },
      env: { PERSONAL_DESKTOP_CONSOLE_URL: "https://from-env.ts.net" },
    });
    try {
      const status = await harness.runtime.tools.desktop_status.execute(
        {},
        toolContext("session-a"),
      );
      expect(status.details.os).toBe("linux");
      expect(status.details.consoleURL).toBe("https://from-gateway.ts.net");
    } finally {
      await harness.restore();
    }
  });

  test("falls back to the projected console URL when the Gateway omits one", async () => {
    const harness = await createPluginHarness({
      env: { PERSONAL_DESKTOP_CONSOLE_URL: "https://from-env.ts.net" },
    });
    try {
      const status = await harness.runtime.tools.desktop_status.execute(
        {},
        toolContext("session-a"),
      );
      expect(status.details.consoleURL).toBe("https://from-env.ts.net");
    } finally {
      await harness.restore();
    }
  });
});

describe("unavailable Desktop Gateway", () => {
  test("the extension loads and every tool fails with a message naming the environment variable", async () => {
    const harness = await createPluginHarness({ env: { PERSONAL_DESKTOP_GATEWAY_URL: undefined } });
    try {
      expect(
        harness.warnings.some((warning) =>
          warning.includes("PERSONAL_DESKTOP_GATEWAY_URL is not set"),
        ),
      ).toBe(true);

      const invocations: Array<[string, Record<string, unknown>]> = [
        ["desktop_status", {}],
        ["desktop_acquire", {}],
        ["desktop_observe", {}],
        [
          "desktop_act",
          {
            observationId: "f2a7c4ca-5197-4eaa-a2c8-4850f5123560",
            actions: [{ type: "click", x: 1, y: 1 }],
          },
        ],
        ["desktop_launch", { app: "firefox" }],
        ["desktop_windows", {}],
        ["desktop_power", { action: "start" }],
        ["desktop_release", {}],
      ];
      for (const [tool, args] of invocations) {
        await expect(
          harness.runtime.tools[tool].execute(args, toolContext("session-a")),
        ).rejects.toThrow("PluginUnavailable: PERSONAL_DESKTOP_GATEWAY_URL is not set");
      }
      expect(harness.requests).toEqual([]);
    } finally {
      await harness.restore();
    }
  });

  test("the channel command reports the same reason instead of throwing", async () => {
    const harness = await createPluginHarness({ env: { PERSONAL_DESKTOP_GATEWAY_URL: undefined } });
    try {
      expect(await runDesktopCommand(harness, "status")).toBe(
        [
          "Personal Desktop is unavailable.",
          "PluginUnavailable: PERSONAL_DESKTOP_GATEWAY_URL is not set",
        ].join("\n"),
      );
      expect(harness.requests).toEqual([]);
    } finally {
      await harness.restore();
    }
  });
});

describe("desktop channel command", () => {
  test("declares the admin tier and its aliases", async () => {
    const harness = await createPluginHarness();
    try {
      const command = harness.runtime.channelCommands.desktop;
      expect(command.permission).toBe("session.admin");
      expect(command.aliases).toEqual(["vnc", "pc"]);
      expect(command.description.length).toBeGreaterThan(0);
    } finally {
      await harness.restore();
    }
  });

  test("the admin tier alone does not grant desktop control", async () => {
    const harness = await createPluginHarness();
    try {
      expect(await runDesktopCommand(harness, "start", { permitted: false })).toBe(
        [
          "Not permitted.",
          "This channel identity is missing security.bypass.personalDesktopControl.",
        ].join("\n"),
      );
      // The gate runs before any Gateway call, so a denied invoker cannot even
      // learn whether the desktop exists.
      expect(harness.requests).toEqual([]);
    } finally {
      await harness.restore();
    }
  });

  test("status is the default subcommand and reports one fact per line", async () => {
    const harness = await createPluginHarness();
    try {
      expect(await runDesktopCommand(harness, "")).toBe(
        [
          "Desktop: pd-test (linux)",
          "Console not published",
          "VM: Running",
          "Controller: nobody",
          "Recovery required: no",
        ].join("\n"),
      );
      expect(harness.requests).toEqual([{ path: "/api/me", body: "" }]);
    } finally {
      await harness.restore();
    }
  });

  test("status shows the console URL, the agent controller, and a quarantine flag", async () => {
    const harness = await createPluginHarness({
      env: { PERSONAL_DESKTOP_CONSOLE_URL: "https://pd.tailnet.ts.net" },
      statusExtras: { controlBlocked: true },
      initiallyControlled: true,
    });
    try {
      expect(await runDesktopCommand(harness, "status")).toBe(
        [
          "Desktop: pd-test (linux)",
          "Console: https://pd.tailnet.ts.net",
          "VM: Running",
          "Controller: agent",
          "Recovery required: yes (the Gateway has blocked control)",
        ].join("\n"),
      );
    } finally {
      await harness.restore();
    }
  });

  test("start dispatches the power request through the serialized executor", async () => {
    const harness = await createPluginHarness();
    try {
      expect(await runDesktopCommand(harness, "start")).toBe(
        ['Desktop start requested.', 'Run "desktop status" to see the new VM state.'].join("\n"),
      );
      expect(harness.requests).toEqual([{ path: "/api/power/start", body: "" }]);
    } finally {
      await harness.restore();
    }
  });

  test("stop releases the agent lease at the Gateway before powering off", async () => {
    const harness = await createPluginHarness({ initiallyControlled: true });
    try {
      expect(await runDesktopCommand(harness, "stop")).toBe(
        ['Desktop stop requested.', 'Run "desktop status" to see the new VM state.'].join("\n"),
      );
      expect(harness.requests.map((request) => request.path)).toEqual([
        "/api/control/release",
        "/api/me",
        "/api/power/stop",
      ]);
      expect(harness.agentActive).toBe(false);
    } finally {
      await harness.restore();
    }
  });

  test("stop refuses to take a lease away from a live TypeClaw session", async () => {
    const harness = await createPluginHarness();
    try {
      await acquire(harness, "writer");
      const reply = await runDesktopCommand(harness, "stop");
      expect(reply).toContain("Desktop stop failed.");
      expect(reply).toContain("ControlRequired");
      expect(harness.agentActive).toBe(true);
    } finally {
      await harness.restore();
    }
  });

  test("an unknown power outcome is reported without inviting a retry", async () => {
    let loseStopResponse = true;
    const harness = await createPluginHarness({
      powerResponse(action) {
        if (action === "stop" && loseStopResponse) {
          loseStopResponse = false;
          throw new TypeError("connection reset after POST");
        }
        return new Response(JSON.stringify({ action, idempotent: false }), {
          status: 202,
          headers: { "Content-Type": "application/json" },
        });
      },
    });
    try {
      expect(await runDesktopCommand(harness, "stop")).toBe(
        [
          "Desktop stop has an unknown outcome.",
          "Do not retry it.",
          'Run "desktop status", then "desktop start" to recover.',
        ].join("\n"),
      );
      const quarantined = await runDesktopCommand(harness, "status");
      expect(quarantined).toContain(
        "Recovery required: yes (a power action has an unknown outcome)",
      );
      expect(await runDesktopCommand(harness, "stop")).toContain("PowerRecoveryRequired");

      expect(await runDesktopCommand(harness, "start")).toContain("Desktop start requested.");
      expect(await runDesktopCommand(harness, "status")).toContain("Recovery required: no");
    } finally {
      await harness.restore();
    }
  });

  test("release confirms the Gateway release for the invoking session", async () => {
    const harness = await createPluginHarness();
    try {
      await acquire(harness, "writer");
      expect(await runDesktopCommand(harness, "release", { sessionId: "writer" })).toBe(
        ["Agent input control released.", "A human can take control of the desktop now."].join("\n"),
      );
      expect(harness.agentActive).toBe(false);
    } finally {
      await harness.restore();
    }
  });

  test("release cleans up a quarantined orphan controller", async () => {
    const harness = await createPluginHarness({ initiallyControlled: true });
    try {
      await expect(acquire(harness, "next-session")).rejects.toThrow("ControlCleanupRequired");
      expect(await runDesktopCommand(harness, "release")).toBe(
        [
          "Orphaned agent input control cleaned up.",
          "A human can take control of the desktop now.",
        ].join("\n"),
      );
      expect(harness.agentActive).toBe(false);
    } finally {
      await harness.restore();
    }
  });

  test("an unrecognized subcommand answers with usage and touches nothing", async () => {
    const harness = await createPluginHarness();
    try {
      expect(await runDesktopCommand(harness, "reboot")).toBe(
        ["Unknown desktop subcommand: reboot", "Usage: desktop [status|start|stop|release]"].join(
          "\n",
        ),
      );
      expect(await runDesktopCommand(harness, "start now")).toBe(
        ["Unknown desktop subcommand: start now", "Usage: desktop [status|start|stop|release]"].join(
          "\n",
        ),
      );
      expect(harness.requests).toEqual([]);
    } finally {
      await harness.restore();
    }
  });
});
