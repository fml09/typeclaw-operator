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

test("localhost dev auth forwards the random token to API calls", async () => {
  const elements = new Map();
  const element = (selector) => {
    if (!elements.has(selector)) {
      elements.set(selector, {
        dataset: {},
        disabled: false,
        hidden: false,
        src: "",
        textContent: "",
        replaceChildren() {},
      });
    }
    return elements.get(selector);
  };

  let requestedURL;
  const context = vm.createContext({
    AbortController,
    DOMException,
    Response,
    URL,
    URLSearchParams,
    clearTimeout,
    console,
    document: { querySelector: element },
    fetch: async (input) => {
      requestedURL = String(input);
      return jsonResponse({ desktopName: "pd-test", vmiPhase: "Running" });
    },
    location: {
      host: "localhost:8080",
      protocol: "http:",
      search: "?issuer=https%3A%2F%2Fissuer.example&subject=subject-123&devToken=random-secret",
    },
    setTimeout,
    addEventListener() {},
  });
  vm.runInContext(moduleSource, context);
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(
    requestedURL,
    "/api/me?issuer=https%3A%2F%2Fissuer.example&subject=subject-123&devToken=random-secret",
  );
  assert.equal(element("#dev-warning").hidden, false);
});

test("power UnknownOutcome disables blind retry until explicit start succeeds", async () => {
  const elements = new Map();
  const element = (selector) => {
    if (!elements.has(selector)) {
      elements.set(selector, {
        dataset: {},
        disabled: false,
        hidden: false,
        src: "",
        textContent: "",
        replaceChildren() {},
      });
    }
    return elements.get(selector);
  };

  let stopCalls = 0;
  let startCalls = 0;
  const fetch = async (input) => {
    const path = String(input);
    if (path.startsWith("/api/me")) {
      return jsonResponse({ desktopName: "pd-test", vmiPhase: "Running" });
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
      return jsonResponse({ action: "start", idempotent: true }, 202);
    }
    throw new Error(`unexpected fetch ${path}`);
  };

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
    location: { host: "desktop.example", protocol: "https:", search: "" },
    setTimeout,
    addEventListener() {},
  });
  vm.runInContext(
    `${moduleSource}\nglobalThis.testUI = { power, refresh, getPowerUncertain: () => powerUncertain, getGatewayControlBlocked: () => gatewayControlBlocked };`,
    context,
  );
  await new Promise((resolve) => setImmediate(resolve));

  await context.testUI.power("stop");
  assert.equal(stopCalls, 1);
  assert.equal(context.testUI.getPowerUncertain().outcome, "UnknownOutcome");
  assert.equal(element("#stop").disabled, true);
  assert.equal(element("#control").disabled, true);
  assert.equal(element("#takeover").disabled, true);
  assert.equal(element("#start").disabled, false);
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
});

test("reload restores the Gateway power block and permits only explicit start", async () => {
  const elements = new Map();
  const element = (selector) => {
    if (!elements.has(selector)) {
      elements.set(selector, {
        dataset: {},
        disabled: false,
        hidden: false,
        src: "",
        textContent: "",
        replaceChildren() {},
      });
    }
    return elements.get(selector);
  };

  let gatewayBlocked = true;
  let stopCalls = 0;
  let startCalls = 0;
  const fetch = async (input) => {
    const path = String(input);
    if (path.startsWith("/api/me")) {
      return jsonResponse({
        desktopName: "pd-test",
        vmiPhase: gatewayBlocked ? "Unknown" : "Running",
        controlBlocked: gatewayBlocked,
        powerRecoveryRequired: gatewayBlocked,
      });
    }
    if (path.startsWith("/api/power/stop")) {
      stopCalls += 1;
      return jsonResponse({ action: "stop" }, 202);
    }
    if (path.startsWith("/api/power/start")) {
      startCalls += 1;
      gatewayBlocked = false;
      return jsonResponse({ action: "start", idempotent: true }, 202);
    }
    throw new Error(`unexpected fetch ${path}`);
  };

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
    location: { host: "desktop.example", protocol: "https:", search: "" },
    setTimeout,
    addEventListener() {},
  });
  vm.runInContext(
    `${moduleSource}\nglobalThis.testUI = { power, refresh, getGatewayControlBlocked: () => gatewayControlBlocked };`,
    context,
  );
  await new Promise((resolve) => setImmediate(resolve));

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
});

test("intermediary timeout and overload responses quarantine power retries", async (t) => {
  for (const responseStatus of [408, 425, 429]) {
    await t.test(String(responseStatus), async () => {
      const elements = new Map();
      const element = (selector) => {
        if (!elements.has(selector)) {
          elements.set(selector, {
            dataset: {},
            disabled: false,
            hidden: false,
            src: "",
            textContent: "",
            replaceChildren() {},
          });
        }
        return elements.get(selector);
      };
      let stopCalls = 0;
      const fetch = async (input) => {
        const path = String(input);
        if (path.startsWith("/api/me")) {
          return jsonResponse({ desktopName: "pd-test", controlBlocked: false });
        }
        if (path.startsWith("/api/power/stop")) {
          stopCalls += 1;
          return jsonResponse({ detail: `intermediary ${responseStatus}` }, responseStatus);
        }
        throw new Error(`unexpected fetch ${path}`);
      };
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
        location: { host: "desktop.example", protocol: "https:", search: "" },
        setTimeout,
        addEventListener() {},
      });
      vm.runInContext(
        `${moduleSource}\nglobalThis.testUI = { power, getPowerUncertain: () => powerUncertain };`,
        context,
      );
      await new Promise((resolve) => setImmediate(resolve));

      await context.testUI.power("stop");
      await context.testUI.power("stop");
      assert.equal(stopCalls, 1);
      assert.equal(context.testUI.getPowerUncertain().outcome, "UnknownOutcome");
      assert.equal(element("#stop").disabled, true);
    });
  }
});
