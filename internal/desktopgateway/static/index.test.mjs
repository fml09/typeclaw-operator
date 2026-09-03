// The Desktop Console ships as one self-contained HTML file, so its behaviour
// is tested by extracting the inline module and running it against a minimal
// DOM in a node:vm sandbox. That keeps the page dependency-free while still
// pinning the rules that must never regress: the power quarantine, the
// structured outcome contract, and what the top bar reports.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const moduleSource = html.match(/<script type="module">([\s\S]*?)<\/script>/)?.[1];
assert.ok(moduleSource, "inline UI module was not found");

function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// Elements the page markup declares as hidden. Seeding them keeps a "still
// hidden" assertion meaningful instead of just reading a fake's default.
const initiallyHidden = new Set(["#dev-warning", "#badge-recovery", "#watermark", "#toast"]);

// elementRegistry mimics just enough of an element for the console: every
// selector returns a stable object whose properties the module reads back.
function elementRegistry() {
  const elements = new Map();
  return (selector) => {
    if (!elements.has(selector)) {
      elements.set(selector, {
        dataset: {},
        disabled: false,
        hidden: initiallyHidden.has(selector),
        src: "",
        value: "",
        textContent: "",
        replaceChildren() {},
        removeAttribute() {},
      });
    }
    return elements.get(selector);
  };
}

function sandbox({ element, fetch, location, exports = "" }) {
  const context = vm.createContext({
    AbortController,
    DOMException,
    Response,
    URL,
    URLSearchParams,
    clearTimeout,
    console,
    document: { querySelector: element },
    fetch,
    location,
    setTimeout,
    addEventListener() {},
  });
  vm.runInContext(
    `${moduleSource}\nglobalThis.testUI = { power, refresh, dismissToast, stopViewing,` +
      ` getPowerUncertain: () => powerUncertain,` +
      ` getGatewayControlBlocked: () => gatewayControlBlocked${exports} };`,
    context,
  );
  return context;
}

const settled = () => new Promise((resolve) => setImmediate(resolve));

test("localhost dev auth forwards only the random token to API calls", async () => {
  const element = elementRegistry();
  const requested = [];
  const context = sandbox({
    element,
    fetch: async (input) => {
      requested.push(String(input));
      return jsonResponse({ desktopName: "inst-desktop", vmiPhase: "Running" });
    },
    location: {
      host: "localhost:8081",
      protocol: "http:",
      search: "?issuer=https%3A%2F%2Fissuer.example&subject=subject-123&devToken=random-secret",
    },
  });
  await settled();

  // The Gateway derives the identity itself; only the development token is a
  // client-supplied credential, so nothing else may be echoed back to it.
  assert.equal(requested[0], "/api/me?devToken=random-secret");
  assert.equal(element("#dev-warning").hidden, false);
  context.testUI.dismissToast();
});

test("power UnknownOutcome disables blind retry until explicit start succeeds", async () => {
  const element = elementRegistry();
  let stopCalls = 0;
  let startCalls = 0;
  const fetch = async (input) => {
    const path = String(input);
    if (path.startsWith("/api/me")) {
      return jsonResponse({ desktopName: "inst-desktop", vmiPhase: "Running" });
    }
    if (path.startsWith("/api/power/stop")) {
      stopCalls += 1;
      return jsonResponse(
        {
          detail: "KubeVirt power outcome is unknown",
          outcome: "UnknownOutcome",
          retrySafe: false,
          controlBlocked: true,
        },
        503,
      );
    }
    if (path.startsWith("/api/power/start")) {
      startCalls += 1;
      return jsonResponse({ action: "start", outcome: "Succeeded", idempotent: true }, 202);
    }
    throw new Error(`unexpected fetch ${path}`);
  };

  const context = sandbox({
    element,
    fetch,
    location: { host: "desktop.example", protocol: "https:", search: "" },
  });
  await settled();

  await context.testUI.power("stop");
  assert.equal(stopCalls, 1);
  assert.equal(context.testUI.getPowerUncertain().outcome, "UnknownOutcome");
  assert.equal(element("#stop").disabled, true);
  assert.equal(element("#control").disabled, true);
  assert.equal(element("#takeover").disabled, true);
  assert.equal(element("#start").disabled, false);
  assert.equal(element("#badge-recovery").hidden, false);
  assert.match(element("#status").textContent, /"retrySafe": false/);
  assert.match(element("#status").textContent, /"controlBlocked": true/);

  await context.testUI.power("stop");
  assert.equal(stopCalls, 1, "a quarantined stop must not be dispatched again");

  await context.testUI.power("start");
  assert.equal(startCalls, 1);
  assert.equal(context.testUI.getPowerUncertain(), undefined);
  assert.equal(element("#stop").disabled, false);
  assert.equal(element("#control").disabled, false);
  assert.equal(element("#takeover").disabled, false);
  assert.equal(element("#badge-recovery").hidden, true);
  context.testUI.dismissToast();
});

test("reload restores the Gateway power block and permits only explicit start", async () => {
  const element = elementRegistry();
  let gatewayBlocked = true;
  let stopCalls = 0;
  let startCalls = 0;
  const fetch = async (input) => {
    const path = String(input);
    if (path.startsWith("/api/me")) {
      return jsonResponse({
        desktopName: "inst-desktop",
        vmiPhase: gatewayBlocked ? "Unknown" : "Running",
        controlBlocked: gatewayBlocked,
        powerRecoveryRequired: gatewayBlocked,
      });
    }
    if (path.startsWith("/api/power/stop")) {
      stopCalls += 1;
      return jsonResponse({ action: "stop", outcome: "Succeeded" }, 202);
    }
    if (path.startsWith("/api/power/start")) {
      startCalls += 1;
      gatewayBlocked = false;
      return jsonResponse({ action: "start", outcome: "Succeeded", idempotent: true }, 202);
    }
    throw new Error(`unexpected fetch ${path}`);
  };

  const context = sandbox({
    element,
    fetch,
    location: { host: "desktop.example", protocol: "https:", search: "" },
  });
  await settled();

  assert.equal(context.testUI.getGatewayControlBlocked(), true);
  assert.equal(element("#stop").disabled, true);
  assert.equal(element("#control").disabled, true);
  assert.equal(element("#takeover").disabled, true);
  assert.match(element("#status").textContent, /"powerRecoveryRequired": true/);

  await context.testUI.power("stop");
  assert.equal(stopCalls, 0, "reload must not lose the server-side stop quarantine");

  await context.testUI.power("start");
  assert.equal(startCalls, 1);
  assert.equal(context.testUI.getGatewayControlBlocked(), false);
  assert.equal(element("#stop").disabled, false);
  assert.equal(element("#control").disabled, false);
  assert.equal(element("#takeover").disabled, false);
  context.testUI.dismissToast();
});

test("intermediary timeout and overload responses quarantine power retries", async (t) => {
  for (const responseStatus of [408, 425, 429]) {
    await t.test(String(responseStatus), async () => {
      const element = elementRegistry();
      let stopCalls = 0;
      const fetch = async (input) => {
        const path = String(input);
        if (path.startsWith("/api/me")) {
          return jsonResponse({ desktopName: "inst-desktop", controlBlocked: false });
        }
        if (path.startsWith("/api/power/stop")) {
          stopCalls += 1;
          return jsonResponse({ detail: `intermediary ${responseStatus}` }, responseStatus);
        }
        throw new Error(`unexpected fetch ${path}`);
      };
      const context = sandbox({
        element,
        fetch,
        location: { host: "desktop.example", protocol: "https:", search: "" },
      });
      await settled();

      await context.testUI.power("stop");
      await context.testUI.power("stop");
      assert.equal(stopCalls, 1);
      assert.equal(context.testUI.getPowerUncertain().outcome, "UnknownOutcome");
      assert.equal(element("#stop").disabled, true);
      context.testUI.dismissToast();
    });
  }
});

test("a structured Rejected outcome never quarantines control (ticket #20)", async () => {
  const element = elementRegistry();
  let stopCalls = 0;
  const fetch = async (input) => {
    const path = String(input);
    if (path.startsWith("/api/me")) {
      return jsonResponse({ desktopName: "inst-desktop", controlBlocked: false });
    }
    if (path.startsWith("/api/power/stop")) {
      stopCalls += 1;
      // A definite Forbidden from KubeVirt: the Gateway ServiceAccount lacks
      // the subresource, so the status is 502 but the desktop never moved.
      return jsonResponse(
        {
          error: "KubeVirt power operation failed",
          detail: "virtualmachines/stop is forbidden",
          action: "stop",
          outcome: "Rejected",
          retrySafe: true,
          controlBlocked: false,
        },
        502,
      );
    }
    throw new Error(`unexpected fetch ${path}`);
  };
  const context = sandbox({
    element,
    fetch,
    location: { host: "desktop.example", protocol: "https:", search: "" },
  });
  await settled();

  await context.testUI.power("stop");
  assert.equal(context.testUI.getPowerUncertain(), undefined);
  assert.equal(context.testUI.getGatewayControlBlocked(), false);
  assert.equal(element("#stop").disabled, false);
  assert.equal(element("#badge-recovery").hidden, true);
  assert.match(element("#status").textContent, /"outcome": "Rejected"/);

  await context.testUI.power("stop");
  assert.equal(stopCalls, 2, "a rejected stop stays retryable");
  context.testUI.dismissToast();
});

test("the top bar reports the Tailscale identity and desktop state", async () => {
  const element = elementRegistry();
  const context = sandbox({
    element,
    fetch: async (input) => {
      const path = String(input).split("?")[0];
      if (path === "/api/me") {
        return jsonResponse({
          desktopName: "inst-desktop",
          namespace: "desktops",
          os: "linux",
          actor: "human",
          authMode: "tailscale",
          login: "alice@example.com",
          vmExists: true,
          vmPrintableStatus: "Running",
          vmiExists: true,
          vmiPhase: "Running",
          controlActive: true,
          controlActor: "agent",
          controlBlocked: false,
        });
      }
      if (path === "/api/screenshot") return jsonResponse({});
      throw new Error(`unexpected fetch ${path}`);
    },
    location: { host: "desktop.example", protocol: "https:", search: "" },
  });
  await settled();

  assert.equal(element("#identity").textContent, "alice@example.com · tailscale");
  assert.equal(element("#desktop-name").textContent, "inst-desktop");
  assert.equal(element("#os-badge").textContent, "linux");
  assert.equal(element("#chip-power").textContent, "Running");
  assert.equal(element("#chip-controller").textContent, "Agent");
  // A running desktop starts its read-only live view unprompted, so the top
  // bar leaves "Disconnected" without a click.
  assert.equal(element("#chip-connection").textContent, "Viewing");
  assert.equal(element("#dev-warning").hidden, true);
  context.testUI.stopViewing();
  context.testUI.dismissToast();
});

test("a running desktop brings the live view up without being asked", async () => {
  const element = elementRegistry();
  const requested = [];
  const context = sandbox({
    element,
    fetch: async (input) => {
      const path = String(input).split("?")[0];
      requested.push(path);
      if (path === "/api/me") {
        return jsonResponse({
          desktopName: "inst-desktop",
          vmExists: true,
          vmPrintableStatus: "Running",
        });
      }
      if (path === "/api/screenshot") return jsonResponse({});
      throw new Error(`unexpected fetch ${path}`);
    },
    location: { host: "desktop.example", protocol: "https:", search: "" },
  });
  await settled();
  for (let i = 0; i < 20 && !requested.includes("/api/screenshot"); i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 10));
  }

  assert.ok(requested.includes("/api/screenshot"), "the live view must start without a click");
  assert.equal(element("#chip-connection").textContent, "Viewing");
  assert.equal(element("#empty").hidden, true);
  context.testUI.stopViewing();
  context.testUI.dismissToast();
});

test("a stopped desktop offers its start button instead of a doomed live view", async () => {
  const element = elementRegistry();
  const requested = [];
  let startCalls = 0;
  const context = sandbox({
    element,
    fetch: async (input) => {
      const path = String(input).split("?")[0];
      requested.push(path);
      if (path === "/api/me") {
        return jsonResponse({
          desktopName: "inst-desktop",
          vmExists: true,
          vmPrintableStatus: "Stopped",
        });
      }
      if (path === "/api/power/start") {
        startCalls += 1;
        return jsonResponse({ action: "start", outcome: "Succeeded" }, 202);
      }
      throw new Error(`unexpected fetch ${path}`);
    },
    location: { host: "desktop.example", protocol: "https:", search: "" },
  });
  await settled();
  await settled();

  assert.equal(requested.includes("/api/screenshot"), false, "a stopped desktop must not be polled");
  assert.equal(element("#empty-title").textContent, "The desktop is stopped");
  assert.equal(element("#empty-action").textContent, "Start desktop");
  assert.equal(element("#empty").hidden, false);

  element("#empty-action").onclick();
  await settled();

  assert.equal(startCalls, 1, "the card's start button dispatches the power start");
  context.testUI.stopViewing();
  context.testUI.dismissToast();
});

test("a desktop that goes dark under the live view stops polling and shows the start card", async () => {
  const element = elementRegistry();
  const requested = [];
  let running = true;
  const context = sandbox({
    element,
    fetch: async (input) => {
      const path = String(input).split("?")[0];
      requested.push(path);
      if (path === "/api/me") {
        return jsonResponse({
          desktopName: "inst-desktop",
          vmExists: true,
          vmPrintableStatus: running ? "Running" : "Stopped",
        });
      }
      if (path === "/api/screenshot") {
        if (!running) return jsonResponse({ detail: "not running" }, 409);
        return jsonResponse({});
      }
      throw new Error(`unexpected fetch ${path}`);
    },
    location: { host: "desktop.example", protocol: "https:", search: "" },
  });
  await settled();
  for (let i = 0; i < 20 && !requested.includes("/api/screenshot"); i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  assert.ok(requested.includes("/api/screenshot"), "precondition: the live view started");

  running = false;
  await context.testUI.refresh();

  assert.equal(element("#chip-connection").textContent, "Disconnected");
  assert.equal(element("#empty-title").textContent, "The desktop is stopped");
  assert.equal(element("#empty-action").textContent, "Start desktop");
  assert.equal(element("#empty").hidden, false);
  context.testUI.stopViewing();
  context.testUI.dismissToast();
});
