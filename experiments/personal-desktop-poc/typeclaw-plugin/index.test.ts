import { describe, expect, test } from "bun:test";

import pluginDefinition, {
  createSerializedExecutor,
  observationIsFresh,
  powerUnknownOutcome,
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
      mustObserve: false,
    };

    expect(observationIsFresh(observation, current, "same-batch-does-not-know-new-id")).toBe(false);
    expect(observationIsFresh(observation, current, observation.observationId)).toBe(true);
  });

  test("rejects a capped image and a Gateway restart ABA", () => {
    const observation = {
      frame,
      observationId: "f2a7c4ca-5197-4eaa-a2c8-4850f5123560",
      mustObserve: true,
    };
    expect(observationIsFresh(observation, current, observation.observationId)).toBe(false);

    observation.mustObserve = false;
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
      mustObserve: false,
    };
    const newObservation = {
      frame,
      observationId: "ea658f02-c788-46a7-be92-946e544cf71f",
      mustObserve: false,
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

type ToolResult = {
  content: Array<{ type: string; text?: string }>;
  details: Record<string, unknown>;
};
type DesktopTool = {
  execute(args: Record<string, unknown>, toolCtx: { sessionId: string }): Promise<ToolResult>;
};
// Unchecked cast target: pluginDefinition.plugin() returns an untyped runtime;
// this shape pins exactly what these tests assert.
type PluginRuntime = {
  hooks: Record<
    "session.end" | "tool.before" | "tool.after",
    (event: unknown, ctx: never) => Promise<unknown>
  >;
  tools: Record<string, DesktopTool>;
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
  clickFailures?: number;
  typeFailures?: number;
  powerResponse?: (action: "start" | "stop") => Response | Promise<Response>;
}

// Runtime narrowing helper: tool details are Record<string, unknown>, and
// these assertions only need the object-typed entries.
function detailObject(details: Record<string, unknown>, key: string): Record<string, unknown> {
  const value = details[key];
  return value && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

async function createPluginHarness(options?: HarnessOptions): Promise<PluginHarness> {
  const originalFetch = globalThis.fetch;
  const originalToken = process.env.PERSONAL_DESKTOP_AGENT_TOKEN;
  process.env.PERSONAL_DESKTOP_AGENT_TOKEN = "t".repeat(32);

  let agentActive = options?.initiallyControlled ?? false;
  let generation = agentActive ? 1 : 0;
  let acquireFailures = options?.acquireFailures ?? 0;
  let releaseFailures = options?.releaseFailures ?? 0;
  let statusFailures = 0;
  let clickFailures = options?.clickFailures ?? 0;
  let typeFailures = options?.typeFailures ?? 0;
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

    if (path === "/api/agent/type") {
      if (typeFailures > 0) {
        typeFailures -= 1;
        return unknownOutcome(502);
      }
      const parsed: unknown = JSON.parse(body);
      let characters = 0;
      if (parsed && typeof parsed === "object" && "text" in parsed && typeof parsed.text === "string") {
        characters = parsed.text.length;
      }
      return respond({ applied: true, characters });
    }
    if (path === "/api/me") {
      if (statusFailures > 0) {
        statusFailures -= 1;
        return respond({ detail: "simulated status failure" }, 503);
      }
      return respond({
        desktopName: "pd-test",
        gatewayBootID: "gateway-test",
        vmiUID: "vmi-test",
        controlGeneration: generation,
        controlActive: agentActive,
        controlActor: agentActive ? "agent" : undefined,
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
    if (
      path === "/api/agent/click" ||
      path === "/api/agent/key" ||
      path === "/api/agent/scroll" ||
      path === "/api/agent/launch"
    ) {
      if (clickFailures > 0) {
        clickFailures -= 1;
        return unknownOutcome(502);
      }
      return respond({ applied: true });
    }
    if (path === "/api/agent/type") {
      if (typeFailures > 0) {
        typeFailures -= 1;
        return unknownOutcome(502);
      }
      return respond({ applied: true, characters: (JSON.parse(body) as { text: string }).text.length });
    }
    if (path === "/api/agent/windows") {
      return respond({ windows: [{ id: "0x03c00002", desktop: 0, title: "Firefox" }] });
    }
    const power = path.match(/^\/api\/power\/(start|stop)$/)?.[1] as "start" | "stop" | undefined;
    if (power) {
      if (options?.powerResponse) return options.powerResponse(power);
      return respond({ action: power }, 202);
    }
    throw new Error(`unexpected fetch: ${url.href}`);
  }) as typeof fetch;

  // Unchecked casts: pluginDefinition.plugin() returns an untyped runtime, and
  // the test config/hook shapes cannot satisfy its parameter type exactly.
  const runtime = (await pluginDefinition.plugin({
    config: {
      gatewayUrl: "https://desktop.example",
      issuer: "https://issuer.example",
      subject: "subject-a",
      agentTokenEnv: "PERSONAL_DESKTOP_AGENT_TOKEN",
      screenshotMaxWidth: 1024,
      screenshotMaxBytes: 180_000,
    },
    permissions: { has: () => true },
    logger: { warn: (message: string) => warnings.push(message) },
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
        if (originalToken === undefined) delete process.env.PERSONAL_DESKTOP_AGENT_TOKEN;
        else process.env.PERSONAL_DESKTOP_AGENT_TOKEN = originalToken;
      }
    },
  };
}

const toolContext = (sessionId: string) => ({ sessionId }) as never;

async function acquire(harness: PluginHarness, sessionId = "session-a") {
  return harness.runtime.tools.desktop_acquire.execute({}, toolContext(sessionId));
}

async function observe(harness: PluginHarness, sessionId = "session-a") {
  const observed = await harness.runtime.tools.desktop_observe.execute({}, toolContext(sessionId));
  await harness.runtime.hooks["tool.after"](
    { tool: "desktop_observe", sessionId, result: observed },
    {} as never,
  );
  return observed;
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
  test("input requires acquire, a fresh observation, and in-bounds coordinates", async () => {
    const harness = await createPluginHarness();
    try {
      await expect(
        harness.runtime.tools.desktop_click.execute(
          { observationId: "f2a7c4ca-5197-4eaa-a2c8-4850f5123560", x: 10, y: 10 },
          toolContext("session-a"),
        ),
      ).rejects.toThrow("ControlRequired");

      await acquire(harness);
      await expect(
        harness.runtime.tools.desktop_click.execute(
          { observationId: "f2a7c4ca-5197-4eaa-a2c8-4850f5123560", x: 10, y: 10 },
          toolContext("session-a"),
        ),
      ).rejects.toThrow("FreshObservationRequired");

      const observed = await observe(harness);
      await expect(
        harness.runtime.tools.desktop_click.execute(
          { observationId: observed.details.observationId, x: 1280, y: 10 },
          toolContext("session-a"),
        ),
      ).rejects.toThrow("exceed screen 1280×720");

      const result = await harness.runtime.tools.desktop_click.execute(
        { observationId: observed.details.observationId, x: 10, y: 20 },
        toolContext("session-a"),
      );
      expect(result.details.outcome).toBe("Applied");
      expect(result.details.applied).toBe(true);
      expect(harness.requests.at(-1)).toEqual({
        path: "/api/agent/click",
        body: JSON.stringify({ x: 10, y: 20, button: "left", clicks: 1 }),
      });
    } finally {
      await harness.restore();
    }
  });

  test("an ambiguous dispatch returns UnknownOutcome and keeps the lease observable", async () => {
    const harness = await createPluginHarness({ typeFailures: 1 });
    try {
      await acquire(harness, "writer");
      const observed = await observe(harness, "writer");
      const result = await harness.runtime.tools.desktop_type.execute(
        { observationId: observed.details.observationId, text: "hello" },
        toolContext("writer"),
      );
      expect(result.details.outcome).toBe("UnknownOutcome");
      expect(result.details.retrySafe).toBe(false);
      expect(harness.agentActive).toBe(true);

      const refreshed = await observe(harness, "writer");
      const retried = await harness.runtime.tools.desktop_type.execute(
        { observationId: refreshed.details.observationId, text: "hello" },
        toolContext("writer"),
      );
      expect(retried.details.outcome).toBe("Applied");
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
