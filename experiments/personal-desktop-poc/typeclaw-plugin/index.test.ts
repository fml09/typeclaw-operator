import { describe, expect, test } from "bun:test";

import pluginDefinition, {
  createSerializedExecutor,
  mapFramebufferPoint,
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

describe("framebuffer coordinate mapping", () => {
  test("keeps the last 4K pixel inside the canvas exclusive boundary", () => {
    const mapped = mapFramebufferPoint(
      { x: 10, y: 20, width: 1000, height: 562.5 },
      3840,
      2160,
      3839,
      2159,
    );
    expect(mapped.x).toBeGreaterThanOrEqual(10);
    expect(mapped.x).toBeLessThan(1010);
    expect(mapped.y).toBeGreaterThanOrEqual(20);
    expect(mapped.y).toBeLessThan(583);
  });

  test("rejects a zero-sized canvas", () => {
    expect(() =>
      mapFramebufferPoint({ x: 0, y: 0, width: 0, height: 10 }, 1280, 720, 0, 0),
    ).toThrow("positive noVNC canvas area");
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
    const signal = withDeadlineSignal(undefined, 10);
    expect(signal.aborted).toBe(false);
    await Bun.sleep(30);
    expect(signal.aborted).toBe(true);
  });
});

type PluginHarness = Awaited<ReturnType<typeof createPluginHarness>>;

async function createPluginHarness(options?: {
  initiallyControlled?: boolean;
  closeFailures?: number;
  connectedWaitFailures?: number;
  typeFailures?: number;
  statusFailuresAfterClose?: number;
  hangingCommand?: string;
  powerResponse?: (action: "start" | "stop") => Response | Promise<Response>;
}) {
  const originalFetch = globalThis.fetch;
  const originalSpawn = Bun.spawn;
  const originalToken = process.env.PERSONAL_DESKTOP_AGENT_TOKEN;
  process.env.PERSONAL_DESKTOP_AGENT_TOKEN = "t".repeat(32);

  let agentActive = options?.initiallyControlled ?? false;
  let generation = agentActive ? 1 : 0;
  let closeFailures = options?.closeFailures ?? 0;
  let connectedWaitFailures = options?.connectedWaitFailures ?? 0;
  let typeFailures = options?.typeFailures ?? 0;
  let statusFailures = 0;
  let killCount = 0;
  const commands: string[][] = [];
  const warnings: string[] = [];
  const fetchSignals: Array<AbortSignal | null | undefined> = [];

  Bun.spawn = ((spawnOptions: { cmd: string[] }) => {
    const command = [...spawnOptions.cmd];
    commands.push(command);
    const args = command.slice(3);
    let exitCode = 0;
    let errorOutput = "";
    if (args[0] === "close") {
      if (closeFailures > 0) {
        closeFailures -= 1;
        statusFailures += options?.statusFailuresAfterClose ?? 0;
        exitCode = 1;
        errorOutput = "simulated close failure";
      } else {
        agentActive = false;
      }
    }
    if (
      args[0] === "wait" &&
      args[1] === '#screen[data-control-connected="true"]' &&
      connectedWaitFailures > 0
    ) {
      connectedWaitFailures -= 1;
      exitCode = 1;
      errorOutput = "simulated connected wait failure";
    }
    if (args[0] === "keyboard" && args[1] === "type" && typeFailures > 0) {
      typeFailures -= 1;
      exitCode = 1;
      errorOutput = "simulated type failure";
    }
    if (args[0] === "click" && args[1] === "#control") {
      agentActive = true;
      generation += 1;
    }
    const output = args.includes("--json") || (args[0] === "--json" && args[1] === "get")
      ? JSON.stringify({
          success: true,
          data: { x: 0, y: 0, width: 1280, height: 720 },
        })
      : "";
    const hangs = options?.hangingCommand ? args.includes(options.hangingCommand) : false;
    return {
      stdout: new Response(output).body!,
      stderr: new Response(errorOutput).body!,
      exited: hangs ? new Promise<number>(() => {}) : Promise.resolve(exitCode),
      kill() {
        killCount += 1;
      },
    } as ReturnType<typeof Bun.spawn>;
  }) as typeof Bun.spawn;

  globalThis.fetch = (async (input, init) => {
    fetchSignals.push(init?.signal);
    const url = String(input);
    if (url.endsWith("/api/me")) {
      if (statusFailures > 0) {
        statusFailures -= 1;
        return new Response(JSON.stringify({ detail: "simulated status failure" }), {
          status: 503,
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response(
        JSON.stringify({
          desktopName: "pd-test",
          gatewayBootID: "gateway-test",
          vmiUID: "vmi-test",
          controlGeneration: generation,
          controlActive: agentActive,
          controlActor: agentActive ? "agent" : undefined,
        }),
        { headers: { "Content-Type": "application/json" } },
      );
    }
    if (url.includes("/api/screenshot?")) {
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
    const power = url.match(/\/api\/power\/(start|stop)$/)?.[1] as
      | "start"
      | "stop"
      | undefined;
    if (power) {
      if (options?.powerResponse) return options.powerResponse(power);
      return new Response(JSON.stringify({ action: power }), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    throw new Error(`unexpected fetch: ${url}`);
  }) as typeof fetch;

  const runtime = (await pluginDefinition.plugin({
    config: {
      gatewayUrl: "https://desktop.example",
      issuer: "https://issuer.example",
      subject: "subject-a",
      agentTokenEnv: "PERSONAL_DESKTOP_AGENT_TOKEN",
      browserSession: "personal-desktop-test",
      screenshotMaxWidth: 1024,
      screenshotMaxBytes: 180_000,
    },
    permissions: { has: () => true },
    logger: { warn: (message: string) => warnings.push(message) },
  } as never)) as any;

  return {
    runtime,
    commands,
    warnings,
    fetchSignals,
    get agentActive() {
      return agentActive;
    },
    get killCount() {
      return killCount;
    },
    async restore() {
      try {
        await runtime.onDispose?.();
      } finally {
        globalThis.fetch = originalFetch;
        Bun.spawn = originalSpawn;
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

  test("an unresponsive agent-browser process is killed and does not pin the queue", async () => {
    const harness = await createPluginHarness({ hangingCommand: "open" });
    try {
      const stalledAcquire = harness.runtime.tools.desktop_acquire.execute(
        {},
        { sessionId: "stalled-session", signal: AbortSignal.timeout(20) } as never,
      );
      const queuedObserve = harness.runtime.tools.desktop_observe.execute(
        {},
        toolContext("next-session"),
      );
      const [acquireResult, observeResult] = await Promise.allSettled([
        stalledAcquire,
        queuedObserve,
      ]);
      expect(acquireResult.status).toBe("rejected");
      expect(observeResult.status).toBe("fulfilled");
      expect(harness.killCount).toBeGreaterThanOrEqual(2);
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
      expect(harness.commands.filter((command) => command.includes("#control"))).toHaveLength(0);
      await harness.runtime.tools.desktop_release.execute(
        {},
        toolContext("new-runtime-session"),
      );
      expect(harness.agentActive).toBe(false);
    } finally {
      await harness.restore();
    }
  });

  test("release invalidates a queued same-session acquire instead of creating an untracked controller", async () => {
    const harness = await createPluginHarness({ initiallyControlled: false });
    try {
      await acquire(harness);
      const queuedAcquire = acquire(harness);
      const release = harness.runtime.tools.desktop_release.execute({}, toolContext("session-a"));
      const [acquireResult, releaseResult] = await Promise.allSettled([queuedAcquire, release]);

      expect(acquireResult.status).toBe("rejected");
      expect(releaseResult.status).toBe("fulfilled");
      expect(harness.agentActive).toBe(false);
      expect(harness.commands.filter((command) => command.includes("#control"))).toHaveLength(1);
    } finally {
      await harness.restore();
    }
  });

  test("a view-only session end does not close the writer, but the owner session end does", async () => {
    const harness = await createPluginHarness({ initiallyControlled: false });
    try {
      await acquire(harness, "writer");
      const closesBefore = harness.commands.filter((command) => command.at(3) === "close").length;
      await harness.runtime.hooks?.["session.end"]?.({ sessionId: "viewer" }, {} as never);
      expect(harness.agentActive).toBe(true);
      expect(harness.commands.filter((command) => command.at(3) === "close")).toHaveLength(
        closesBefore,
      );

      await harness.runtime.hooks?.["session.end"]?.({ sessionId: "writer" }, {} as never);
      expect(harness.agentActive).toBe(false);
      expect(harness.commands.filter((command) => command.at(3) === "close")).toHaveLength(
        closesBefore + 1,
      );
    } finally {
      await harness.restore();
    }
  });

  test("stop rejection cannot hand a stale lease to a queued acquire", async () => {
    const harness = await createPluginHarness({
      initiallyControlled: false,
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
      expect(harness.commands.filter((command) => command.includes("#control"))).toHaveLength(1);
    } finally {
      await harness.restore();
    }
  });

  test("failed session-end cleanup quarantines the old controller instead of adopting it", async () => {
    const harness = await createPluginHarness({
      initiallyControlled: false,
      closeFailures: 1,
      statusFailuresAfterClose: 1,
    });
    try {
      await acquire(harness, "writer");
      await harness.runtime.hooks?.["session.end"]?.({ sessionId: "writer" }, {} as never);
      expect(harness.agentActive).toBe(true);
      expect(harness.warnings.some((warning) => warning.includes("failed to release"))).toBe(true);

      await expect(acquire(harness, "next-session")).rejects.toThrow(
        "ControlCleanupRequired",
      );
      const quarantined = await harness.runtime.tools.desktop_status.execute(
        {},
        toolContext("next-session"),
      );
      expect(quarantined.details.pluginControlCleanupRequired.recoveryTool).toBe(
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
      expect(harness.commands.filter((command) => command.includes("#control"))).toHaveLength(2);
    } finally {
      await harness.restore();
    }
  });

  test("failed acquire setup cleanup leaves an orphan quarantine", async () => {
    const harness = await createPluginHarness({
      initiallyControlled: false,
      connectedWaitFailures: 1,
      closeFailures: 1,
      statusFailuresAfterClose: 1,
    });
    try {
      await expect(acquire(harness, "setup-session")).rejects.toThrow("ControlSetupFailed");
      expect(harness.agentActive).toBe(true);
      await expect(acquire(harness, "next-session")).rejects.toThrow(
        "ControlCleanupRequired",
      );

      await harness.runtime.tools.desktop_release.execute({}, toolContext("next-session"));
      expect(harness.agentActive).toBe(false);
    } finally {
      await harness.restore();
    }
  });

  test("ambiguous input cleanup failure blocks further control until release is confirmed", async () => {
    const harness = await createPluginHarness({
      initiallyControlled: false,
      typeFailures: 1,
      closeFailures: 1,
    });
    try {
      await acquire(harness, "writer");
      const observed = await harness.runtime.tools.desktop_observe.execute(
        {},
        toolContext("writer"),
      );
      await harness.runtime.hooks?.["tool.after"]?.(
        { tool: "desktop_observe", sessionId: "writer", result: observed },
        {} as never,
      );
      const result = await harness.runtime.tools.desktop_type.execute(
        { observationId: observed.details.observationId, text: "hello" },
        toolContext("writer"),
      );
      expect(result.details.outcome).toBe("UnknownOutcome");
      expect(result.details.cleanup.connectionClosed).toBe(false);
      expect(result.details.cleanup.gatewayReleaseConfirmed).toBe(false);
      expect(harness.agentActive).toBe(true);
      await expect(acquire(harness, "writer")).rejects.toThrow("ControlCleanupRequired");

      await harness.runtime.tools.desktop_release.execute({}, toolContext("recovery-session"));
      expect(harness.agentActive).toBe(false);
    } finally {
      await harness.restore();
    }
  });
});

describe("plugin power quarantine", () => {
  test("transport loss blocks acquire and stop until an explicit start succeeds", async () => {
    let loseStopResponse = true;
    const harness = await createPluginHarness({
      initiallyControlled: false,
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
      expect(quarantined.details.pluginPowerUncertain.outcome).toBe("UnknownOutcome");

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
