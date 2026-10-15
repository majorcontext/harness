// Tests of the client API over the Worker transport. The shared suite of
// suite.mjs runs here too. They run the real wasm
// module in a worker_threads Worker against a scripted model server. Run with
// `node --test sdk/embed/test/*.test.mjs` after setting HARNESS_WASM and
// HARNESS_WASM_EXEC (see support.mjs).
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { test } from "node:test";
import { Worker } from "node:worker_threads";
import assert from "node:assert/strict";
import { createHarness, defaultBuiltinTools, wasm } from "../src/index.mjs";
import { workerBridge } from "../src/workerbridge.mjs";
import { clientSuite } from "./suite.mjs";
import { acpClient, bodyText, chatgptToken, fakeIssuer, fakeModel, moduleFiles, seedLogin, tempDir, userConfig } from "./support.mjs";

clientSuite((options) => harness(options));

const echo = {
  name: "js_echo",
  description: "Returns its text.",
  schema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
  run: async (input) => "echo:" + input.text,
};
const call = { id: "c1", name: "js_echo", input: { text: "hi" } };

function harness(options) {
  return createHarness({ transport: wasm({ ...moduleFiles(), worker: true }), ...options });
}

test("a tool call of the module reaches the run function of the page and returns", async () => {
  const model = await fakeModel([{ toolCalls: [call] }, { text: "ok" }]);
  const h = await harness({ config: userConfig(model.url), tools: [echo] });
  try {
    const c = acpClient(h);
    const result = await c.prompt(await c.session(), "go");
    assert.equal(result.stopReason, "end_turn");
    const requests = await model.requests();
    assert.ok(requests[0].body.tools.some((t) => t.name === "js_echo"));
    assert.match(bodyText(requests[1]), /echo:hi/);
  } finally {
    await h.close();
    model.close();
  }
});

test("a tool failure on the page is an error result for the model", async () => {
  const model = await fakeModel([{ toolCalls: [call] }, { text: "ok" }]);
  const tool = { ...echo, run: async () => { throw new Error("disk on fire"); } };
  const h = await harness({ config: userConfig(model.url), tools: [tool] });
  try {
    const c = acpClient(h);
    const result = await c.prompt(await c.session(), "go");
    assert.equal(result.stopReason, "end_turn");
    assert.match(bodyText((await model.requests())[1]), /disk on fire/);
  } finally {
    await h.close();
    model.close();
  }
});

test("a cancel aborts the signal of a running page tool", async () => {
  const model = await fakeModel([{ toolCalls: [call] }, { text: "ok" }]);
  let started;
  const running = new Promise((resolve) => (started = resolve));
  let aborted;
  const sawAbort = new Promise((resolve) => (aborted = resolve));
  const tool = {
    ...echo,
    run: (_input, { signal }) => {
      started();
      return new Promise((_, reject) => signal.addEventListener("abort", () => { aborted(); reject(signal.reason); }));
    },
  };
  const h = await harness({ config: userConfig(model.url), tools: [tool] });
  try {
    const c = acpClient(h);
    const sessionId = await c.session();
    const turn = c.prompt(sessionId, "go");
    await running;
    await c.notify("session/cancel", { sessionId });
    await sawAbort;
    assert.equal((await turn).stopReason, "cancelled");
  } finally {
    await h.close();
    model.close();
  }
});

test("proxy.baseURL carries the model request of a config with no provider and proxy.headers reach the request", async () => {
  const model = await fakeModel([{ text: "hello" }]);
  const h = await harness({
    config: { model: "anthropic/claude-fable-5" },
    proxy: { baseURL: model.url, headers: { "x-proxy-token": "t1" } },
  });
  try {
    const c = acpClient(h);
    assert.equal((await c.prompt(await c.session(), "go")).stopReason, "end_turn");
    assert.equal((await model.requests())[0].headers["x-proxy-token"], "t1");
  } finally {
    await h.close();
    model.close();
  }
});

test("a base_url that the config sets wins over proxy.baseURL", async () => {
  const model = await fakeModel([{ text: "hello" }]);
  const h = await harness({ config: userConfig(model.url), proxy: { baseURL: "http://127.0.0.1:1" } });
  try {
    const c = acpClient(h);
    assert.equal((await c.prompt(await c.session(), "go")).stopReason, "end_turn");
    assert.equal((await model.requests()).length, 1);
  } finally {
    await h.close();
    model.close();
  }
});

test("a page fetch carries the model request and streams the response back", async () => {
  const model = await fakeModel([{ text: "hello" }]);
  const seen = [];
  const pageFetch = async (url, init) => {
    seen.push({ url: String(url), method: init.method, body: init.body });
    return fetch(url, init);
  };
  const h = await harness({ config: userConfig(model.url), fetch: pageFetch });
  try {
    const c = acpClient(h);
    assert.equal((await c.prompt(await c.session(), "go")).stopReason, "end_turn");
    assert.equal(seen.length, 1);
    assert.equal(seen[0].method, "POST");
    assert.equal(seen[0].url, model.url + "/v1/messages");
    assert.deepEqual(JSON.parse(new TextDecoder().decode(seen[0].body)), (await model.requests())[0].body);
    assert.ok(c.updates.some((u) => u.sessionUpdate === "agent_message_chunk" && u.content.text === "hello"));
  } finally {
    await h.close();
    model.close();
  }
});

test("a cancel aborts the signal of a page fetch that has begun to stream", async () => {
  const model = await fakeModel([{ text: "partial", block: true }]);
  let started;
  const running = new Promise((resolve) => (started = resolve));
  let aborted;
  const sawAbort = new Promise((resolve) => (aborted = resolve));
  const pageFetch = async (url, init) => {
    init.signal.addEventListener("abort", aborted);
    const res = await fetch(url, init);
    started();
    return res;
  };
  const h = await harness({ config: userConfig(model.url), fetch: pageFetch });
  try {
    const c = acpClient(h);
    const sessionId = await c.session();
    const turn = c.prompt(sessionId, "go");
    await running;
    await c.notify("session/cancel", { sessionId });
    await sawAbort;
    assert.equal((await turn).stopReason, "cancelled");
  } finally {
    await h.close();
    model.close();
  }
});

test("a page fetch that rejects ends the turn with the error of the page", async () => {
  const pageFetch = async () => { throw new TypeError("Failed to fetch"); };
  const config = { ...userConfig("http://127.0.0.1:1"), request_max_retries: 0, stream_max_retries: 0 };
  const h = await harness({ config, fetch: pageFetch });
  try {
    const c = acpClient(h);
    const sessionId = await c.session();
    await assert.rejects(c.prompt(sessionId, "go"), (err) => JSON.stringify(err.rpc).includes("TypeError: Failed to fetch"));
  } finally {
    await h.close();
  }
});

test("a store directory is exclusive to one harness and keeps the session", async () => {
  const dir = tempDir();
  const model = await fakeModel([{ text: "hello" }]);
  const h = await harness({ config: userConfig(model.url), store: { persist: true, dir } });
  try {
    await assert.rejects(harness({ config: userConfig(model.url), store: { persist: true, dir } }), /in use/);
    const c = acpClient(h);
    assert.equal((await c.prompt(await c.session(), "go")).stopReason, "end_turn");
  } finally {
    await h.close();
    model.close();
  }
  const h2 = await harness({ config: userConfig(model.url), store: { persist: true, dir } });
  await h2.close();
});

test("a transport that is not a Worker is refused", async () => {
  await assert.rejects(createHarness({ transport: wasm({ ...moduleFiles(), worker: false }) }), /Worker/);
});

// failWorker ends the Worker that the next createHarness starts, the way a crash does.
function captureWorker() {
  const original = Worker.prototype.postMessage;
  const captured = {};
  Worker.prototype.postMessage = function (...args) {
    captured.worker = this;
    return original.apply(this, args);
  };
  return { captured, restore: () => (Worker.prototype.postMessage = original) };
}

test("close settles after a Worker failure and releases the store directory", async () => {
  const dir = tempDir();
  const { captured, restore } = captureWorker();
  const h = await harness({ config: userConfig("http://127.0.0.1:1"), store: { persist: true, dir } });
  restore();
  const c = acpClient(h);
  c.done.catch(() => {});
  await captured.worker.terminate();
  await Promise.race([h.close(), new Promise((_, reject) => setTimeout(() => reject(new Error("close hangs")), 5000).unref())]);
  const again = await harness({ config: userConfig("http://127.0.0.1:1"), store: { persist: true, dir } });
  await again.close();
});

test("aborting the writable releases the store directory", async () => {
  const dir = tempDir();
  const h = await harness({ config: userConfig("http://127.0.0.1:1"), store: { persist: true, dir } });
  await h.stream.writable.getWriter().abort(new Error("done"));
  const again = await harness({ config: userConfig("http://127.0.0.1:1"), store: { persist: true, dir } });
  await again.close();
});

test("proxy.headers go to the proxy origin and to no other provider origin", async () => {
  const direct = await fakeModel([{ text: "hello" }]);
  const h = await harness({
    config: userConfig(direct.url),
    proxy: { baseURL: "http://127.0.0.1:1", headers: { "x-proxy-token": "secret" } },
  });
  try {
    const c = acpClient(h);
    assert.equal((await c.prompt(await c.session(), "go")).stopReason, "end_turn");
    const [request] = await direct.requests();
    assert.equal(request.headers["x-proxy-token"], undefined);
  } finally {
    await h.close();
    direct.close();
  }
});

test("proxy.providerBaseURLs sends the native codex requests to the pass-through path with the stored login", async () => {
  const model = await fakeModel([{ text: "unused" }]);
  const dir = tempDir();
  const segment = (value) => Buffer.from(JSON.stringify(value)).toString("base64url");
  const token = [segment({ alg: "none" }), segment({ exp: Math.floor(Date.now() / 1000) + 3600, "https://api.openai.com/auth": { chatgpt_account_id: "acct-1" } }), "sig"].join(".");
  fs.mkdirSync(path.join(dir, "auth"), { mode: 0o700 });
  fs.writeFileSync(path.join(dir, "auth", "codex.json"), JSON.stringify({ access_token: token, refresh_token: "refresh-1", account_id: "acct-1" }), { mode: 0o600 });
  const h = await harness({
    config: { model: "codex/gpt-5.6-sol" },
    proxy: { providerBaseURLs: { codex: model.url + "/codex" } },
    store: { persist: true, dir },
  });
  try {
    const c = acpClient(h);
    await c.prompt(await c.session(), "go").catch(() => {});
    const request = (await model.requests()).find((r) => r.path === "/codex/responses");
    assert.ok(request, "the codex request did not reach the pass-through path");
    assert.equal(request.headers.authorization, "Bearer " + token);
    assert.equal(request.headers["chatgpt-account-id"], "acct-1");
  } finally {
    await h.close();
    model.close();
  }
});

// withPageOrigin runs fn while globalThis.location names a page. The module paths are file URLs, which a base URL leaves alone.
async function withPageOrigin(fn, location = { href: "http://127.0.0.1:8080/", origin: "http://127.0.0.1:8080" }) {
  const saved = Object.getOwnPropertyDescriptor(globalThis, "location");
  Object.defineProperty(globalThis, "location", { value: location, configurable: true });
  try {
    return await fn();
  } finally {
    if (saved) Object.defineProperty(globalThis, "location", saved);
    else delete globalThis.location;
  }
}

function pageHarness(providerBaseURLs, extra = {}) {
  const files = moduleFiles();
  const transport = wasm({ module: pathToFileURL(files.module).href, wasmExec: pathToFileURL(files.wasmExec).href, worker: true });
  return createHarness({ transport, config: { model: "codex/gpt-5.6-sol" }, proxy: { providerBaseURLs }, store: { persist: true, dir: tempDir() }, ...extra });
}

const chatgptEntry = (extra) => ({ type: "openai", auth: "chatgpt", responses_path: "/responses", ...extra });

test("a request of a ChatGPT-login provider to another origin than the page fails before it is sent", async () => {
  const model = await fakeModel([{ text: "unused" }]);
  const foreign = model.url + "/codex";
  const rows = {
    "providerBaseURLs": { proxy: { providerBaseURLs: { codex: foreign } } },
    "a base_url of the user config": { config: { model: "codex/gpt-5.6-sol", providers: { codex: chatgptEntry({ base_url: foreign }) } } },
    "a base_url that proxy.baseURL fills": { config: { model: "gpt/x", providers: { gpt: chatgptEntry() } }, proxy: { baseURL: foreign } },
  };
  try {
    for (const [name, row] of Object.entries(rows)) {
      await withPageOrigin(async () => {
        const dir = tempDir();
        seedLogin(dir, chatgptToken(3600));
        const files = moduleFiles();
        const transport = wasm({ module: pathToFileURL(files.module).href, wasmExec: pathToFileURL(files.wasmExec).href, worker: true });
        const h = await createHarness({ transport, config: { model: "codex/gpt-5.6-sol" }, store: { persist: true, dir }, ...row });
        try {
          const c = acpClient(h);
          await assert.rejects(c.prompt(await c.session(), "go"), /page origin/, name);
        } finally {
          await h.close();
        }
      });
    }
    assert.deepEqual((await model.requests()).filter((r) => r.path.startsWith("/codex") || r.path === "/responses"), [], "a request reached the foreign origin");
  } finally {
    model.close();
  }
});

test("a page with an opaque origin sends the ChatGPT login nowhere", async () => {
  const model = await fakeModel([{ text: "unused" }]);
  try {
    await withPageOrigin(async () => {
      const dir = tempDir();
      seedLogin(dir, chatgptToken(3600));
      const h = await pageHarness({ codex: model.url + "/codex" }, { store: { persist: true, dir } });
      try {
        const c = acpClient(h);
        await assert.rejects(c.prompt(await c.session(), "go"), /page origin/);
      } finally {
        await h.close();
      }
    }, { href: "file:///page.html", origin: "null" });
    assert.equal((await model.requests()).length, 0);
  } finally {
    model.close();
  }
});

test("proxy.providerBaseURLs of the ChatGPT provider accepts a path and a URL of the page origin", async () => {
  for (const base of ["/codex", "http://127.0.0.1:8080/codex"]) {
    await withPageOrigin(async () => {
      const h = await pageHarness({ codex: base });
      try {
        // The config is valid, so the session fails only because no login exists yet.
        await assert.rejects(acpClient(h).session(), (err) => err.rpc?.data?.code === "login_required");
      } finally {
        await h.close();
      }
    });
  }
});

test("a page fetch callback carries model traffic only: the refresh goes to the issuer directly", async () => {
  const model = await fakeModel([{ text: "unused" }]);
  const issuer = await fakeIssuer();
  const dir = tempDir();
  seedLogin(dir, chatgptToken(-3600));
  const seen = [];
  const pageFetch = async (url, init) => {
    seen.push(String(url));
    return fetch(url, { method: init.method, headers: init.headers, body: init.body });
  };
  const h = await harness({ config: { model: "codex/gpt-5.6-sol", codex_issuer: issuer.url }, proxy: { providerBaseURLs: { codex: model.url + "/codex" } }, fetch: pageFetch, store: { persist: true, dir } });
  try {
    const c = acpClient(h);
    await c.prompt(await c.session(), "go").catch(() => {});
    assert.ok(issuer.requests.some((r) => r.path === "/oauth/token"), "no refresh reached the issuer");
    assert.deepEqual(seen.filter((u) => u.startsWith(issuer.url)), [], "the page callback saw an issuer request");
    assert.ok(seen.some((u) => u === model.url + "/codex/responses"), "the page callback did not carry the model request");
  } finally {
    await h.close();
    model.close();
    issuer.close();
  }
});

test("the device login goes to the issuer directly, not through the page fetch callback", async () => {
  const issuer = await fakeIssuer();
  const seen = [];
  const pageFetch = async (url) => {
    seen.push(String(url));
    throw new Error("the page callback must not carry the login");
  };
  const h = await harness({ config: { codex_issuer: issuer.url }, fetch: pageFetch, store: { persist: true, dir: tempDir() } });
  try {
    const c = acpClient(h);
    await c.request("initialize", { protocolVersion: 1, clientCapabilities: { elicitation: { url: {} } } });
    await c.request("authenticate", { methodId: "chatgpt" });
    assert.ok(issuer.requests.some((r) => r.path === "/oauth/token"), "the login did not reach the issuer");
    assert.deepEqual(seen, []);
  } finally {
    await h.close();
    issuer.close();
  }
});

test("a redirect of the issuer is not followed", async () => {
  const sink = await fakeIssuer();
  const issuer = await fakeIssuer({ redirectTo: sink.url });
  const dir = tempDir();
  seedLogin(dir, chatgptToken(-3600));
  const h = await harness({ config: { model: "codex/gpt-5.6-sol", codex_issuer: issuer.url }, store: { persist: true, dir } });
  try {
    const c = acpClient(h);
    await c.prompt(await c.session(), "go").catch(() => {});
    assert.ok(issuer.requests.length > 0, "the refresh did not reach the issuer");
    assert.deepEqual(sink.requests, [], "the redirect was followed");
  } finally {
    await h.close();
    sink.close();
    issuer.close();
  }
});

test("a redirect of the ChatGPT model endpoint is not followed", async () => {
  const sink = await fakeIssuer();
  const redirecting = await fakeIssuer({ redirectTo: sink.url });
  const dir = tempDir();
  seedLogin(dir, chatgptToken(3600));
  const h = await harness({ config: { model: "codex/gpt-5.6-sol" }, proxy: { providerBaseURLs: { codex: redirecting.url + "/codex" } }, store: { persist: true, dir } });
  try {
    const c = acpClient(h);
    await c.prompt(await c.session(), "go").catch(() => {});
    assert.ok(redirecting.requests.length > 0, "the model request did not reach the endpoint");
    assert.deepEqual(sink.requests, [], "the redirect was followed");
  } finally {
    await h.close();
    sink.close();
    redirecting.close();
  }
});

test("a redirect of the ChatGPT model endpoint does not get past the page fetch callback", async () => {
  const sink = await fakeIssuer();
  const redirecting = await fakeIssuer({ redirectTo: sink.url });
  const dir = tempDir();
  seedLogin(dir, chatgptToken(3600));
  const seen = [];
  const pageFetch = async (url, init) => {
    seen.push(init.redirect);
    if (String(url).startsWith("https://synthetic.test")) return new Response(null, { status: 302, headers: { location: sink.url } });
    return fetch(url, { method: init.method, headers: init.headers, body: init.body, redirect: init.redirect });
  };
  try {
    for (const base of [redirecting.url + "/codex", "https://synthetic.test/codex"]) {
      const h = await harness({ config: { model: "codex/gpt-5.6-sol", request_max_retries: 0, stream_max_retries: 0 }, proxy: { providerBaseURLs: { codex: base } }, fetch: pageFetch, store: { persist: true, dir } });
      try {
        const c = acpClient(h);
        await assert.rejects(c.prompt(await c.session(), "go"), /redirect/, base);
      } finally {
        await h.close();
      }
    }
    assert.ok(seen.length >= 2 && seen.every((mode) => mode === "manual"), "the page callback did not get redirect: manual");
    assert.deepEqual(sink.requests, [], "the redirect was followed");
  } finally {
    sink.close();
    redirecting.close();
  }
});

test("a redirect that the page fetch followed itself fails the request", async () => {
  const sink = await fakeIssuer();
  const redirecting = await fakeIssuer({ redirectTo: sink.url });
  const dir = tempDir();
  seedLogin(dir, chatgptToken(3600));
  // A real fetch that ignores init.redirect follows the 302 and returns the final answer.
  const pageFetch = (url, init) => fetch(url, { method: init.method, headers: init.headers, body: init.body });
  const h = await harness({
    config: { model: "codex/gpt-5.6-sol", request_max_retries: 0, stream_max_retries: 0 },
    proxy: { providerBaseURLs: { codex: redirecting.url + "/codex" } },
    fetch: pageFetch,
    store: { persist: true, dir },
  });
  try {
    const c = acpClient(h);
    await assert.rejects(c.prompt(await c.session(), "go"), /redirect/);
  } finally {
    await h.close();
    sink.close();
    redirecting.close();
  }
});

test("a malformed proxy URL leaves no Worker and no held store", async () => {
  const dir = tempDir();
  const files = moduleFiles();
  const transport = wasm({ module: files.module, wasmExec: files.wasmExec, worker: true });
  let workers = 0;
  const original = Worker.prototype.postMessage;
  Worker.prototype.postMessage = function (...args) {
    workers++;
    return original.apply(this, args);
  };
  try {
    await withPageOrigin(async () => {
      await assert.rejects(createHarness({ transport, proxy: { baseURL: "http://[bad" }, store: { persist: true, dir } }));
    });
  } finally {
    Worker.prototype.postMessage = original;
  }
  assert.equal(workers, 0, "a Worker started for options that cannot be built");
  const again = await harness({ config: userConfig("http://127.0.0.1:1"), store: { persist: true, dir } });
  await again.close();
});

test("an aborted signal does not run the page tool", async () => {
  const posted = [];
  const bridge = workerBridge((m) => posted.push(m));
  const ac = new AbortController();
  ac.abort();
  await assert.rejects(bridge.callTool("js_echo", {}, ac.signal));
  assert.deepEqual(posted, []);
});

test("a failed Worker releases the store directory before close", async () => {
  const dir = tempDir();
  const { captured, restore } = captureWorker();
  const h = await harness({ config: userConfig("http://127.0.0.1:1"), store: { persist: true, dir } });
  restore();
  const c = acpClient(h);
  const ended = c.done.catch(() => {});
  await captured.worker.terminate();
  await ended;
  const again = await harness({ config: userConfig("http://127.0.0.1:1"), store: { persist: true, dir } });
  await again.close();
  await h.close();
});

async function firstTools(options) {
  const model = await fakeModel([{ text: "ok" }]);
  const h = await harness({ config: userConfig(model.url), ...options });
  try {
    const c = acpClient(h);
    await c.prompt(await c.session(), "go");
    return (await model.requests())[0].body.tools?.map((t) => t.name).sort() ?? [];
  } finally {
    await h.close();
    model.close();
  }
}

test("the first request of a host lists the default tools, the page tools, and no bash or process", async () => {
  const names = await firstTools({ tools: [echo] });
  assert.deepEqual(names, [...defaultBuiltinTools, "js_echo"].sort());
  assert.ok(!names.includes("bash") && !names.includes("process"));
});

test("an empty builtinTools gives the model no tools", async () => {
  assert.deepEqual(await firstTools({ tools: [echo], builtinTools: [] }), []);
});

test("builtinTools replaces the default set", async () => {
  assert.deepEqual(await firstTools({ builtinTools: ["read_file", "task"] }), ["read_file", "task"]);
});
