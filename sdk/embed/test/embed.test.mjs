// Tests of the client API over the Worker transport. The shared suite of
// suite.mjs runs here too. They run the real wasm
// module in a worker_threads Worker against a scripted model server. Run with
// `node --test sdk/embed/test/*.test.mjs` after setting HARNESS_WASM and
// HARNESS_WASM_EXEC (see support.mjs).
import { pathToFileURL } from "node:url";
import { test } from "node:test";
import { Worker } from "node:worker_threads";
import assert from "node:assert/strict";
import { webmcpTools } from "../src/webmcp.mjs";
import { createHarness, defaultBuiltinTools, wasm } from "../src/index.mjs";
import { guardedFetch } from "../src/inprocess.mjs";
import { workerBridge } from "../src/workerbridge.mjs";
import { clientSuite } from "./suite.mjs";
import { acpClient, bodyText, fakeModel, fakeServer, moduleFiles, tempDir, userConfig } from "./support.mjs";

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
    const requests = await model.modelRequests();
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
    assert.match(bodyText((await model.modelRequests())[1]), /disk on fire/);
  } finally {
    await h.close();
    model.close();
  }
});

test("a WebMCP tool of the page is a page tool that the model calls", async () => {
  const model = await fakeModel([{ toolCalls: [{ id: "c1", name: "webmcp__lookup", input: { q: "cats" } }] }, { text: "ok" }]);
  const wm = { name: "lookup", description: "Looks a word up.", inputSchema: { type: "object", properties: { q: { type: "string" } } }, annotations: { readOnlyHint: true }, origin: "https://page.test" };
  const seen = [];
  Object.defineProperty(globalThis, "document", {
    configurable: true,
    value: {
      location: { origin: "https://page.test" },
      modelContext: {
        getTools: async () => [wm],
        executeTool: async (tool, input) => {
          seen.push([tool, input]);
          return JSON.stringify({ content: [{ type: "text", text: "webmcp found " + input.q }] });
        },
      },
    },
  });
  let h;
  try {
    h = await harness({ config: userConfig(model.url), tools: await webmcpTools() });
    const c = acpClient(h);
    assert.equal((await c.prompt(await c.session(), "go")).stopReason, "end_turn");
    const requests = await model.modelRequests();
    const spec = requests[0].body.tools.find((t) => t.name === "webmcp__lookup");
    assert.equal(spec.description, "Looks a word up.\nRead-only.");
    assert.deepEqual(spec.input_schema, wm.inputSchema);
    assert.deepEqual(seen, [[wm, { q: "cats" }]]);
    assert.match(bodyText(requests[1]), /webmcp found cats/);
  } finally {
    delete globalThis.document;
    await h?.close();
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

test("proxy.baseURL routes an injected native provider and proxy.headers reach the request", async () => {
  const model = await fakeModel([{ text: "hello" }]);
  const h = await harness({
    config: { model: "anthropic/claude-fable-5", providers: { anthropic: { auth: "injected" } } },
    proxy: { baseURL: model.url, headers: { "x-proxy-token": "t1" } },
  });
  try {
    const c = acpClient(h);
    assert.equal((await c.prompt(await c.session(), "go")).stopReason, "end_turn");
    assert.equal((await model.modelRequests())[0].headers["x-proxy-token"], "t1");
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
    assert.equal((await model.modelRequests()).length, 1);
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
    assert.deepEqual(seen.map((r) => [r.method, r.url]), [["GET", model.url + "/v1/models"], ["POST", model.url + "/v1/messages"]]);
    assert.deepEqual(JSON.parse(new TextDecoder().decode(seen[1].body)), (await model.modelRequests())[0].body);
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
    if (init.method !== "POST") return fetch(url, init);
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

test("the in-process mode is refused under Node and leaves the globals", async () => {
  const before = { fs: globalThis.fs, process: globalThis.process };
  await assert.rejects(createHarness({ transport: wasm({ ...moduleFiles(), worker: false }) }), /Worker mode under Node/);
  assert.deepEqual({ fs: globalThis.fs, process: globalThis.process }, before);
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
    const [request] = await direct.modelRequests();
    assert.equal(request.headers["x-proxy-token"], undefined);
  } finally {
    await h.close();
    direct.close();
  }
});

test("proxy.providers makes the native codex entry injected, and the request carries no credential", async () => {
  const model = await fakeModel([{ text: "unused" }]);
  const h = await harness({
    config: { model: "codex/gpt-5.6-sol" },
    proxy: { providers: { codex: { baseURL: model.url + "/codex", loginURL: "https://console.example/settings/codex" } } },
  });
  try {
    const c = acpClient(h);
    await c.prompt(await c.session(), "go").catch(() => {});
    const request = (await model.modelRequests()).find((r) => r.path === "/codex/responses");
    assert.ok(request, "the codex request did not reach the server path");
    for (const name of ["authorization", "chatgpt-account-id", "originator", "x-api-key"]) assert.equal(request.headers[name], undefined, name);
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

function pageHarness(providers, extra = {}) {
  const files = moduleFiles();
  const transport = wasm({ module: pathToFileURL(files.module).href, wasmExec: pathToFileURL(files.wasmExec).href, worker: true });
  return createHarness({ transport, config: { model: "codex/gpt-5.6-sol" }, proxy: { providers }, ...extra });
}

test("proxy.providers resolves a relative baseURL and loginURL against the page", async () => {
  const server = await fakeServer({ status: 401 });
  try {
    await withPageOrigin(async () => {
      const h = await pageHarness({ codex: { baseURL: "/codex", loginURL: "/settings/codex" } });
      try {
        await assert.rejects(
          acpClient(h).session(),
          (err) => err.rpc?.data?.code === "login_required" && err.message.includes(server.url + "/settings/codex"),
        );
      } finally {
        await h.close();
      }
    }, { href: server.url + "/", origin: server.url });
    assert.ok(server.requests.some((r) => r.path.startsWith("/codex/models")), "the status probe did not reach the base URL");
  } finally {
    server.close();
  }
});

test("proxy.providers refuses a name that is no provider and a value that is no object", async () => {
  const h = await harness({ proxy: { providers: { nope: { baseURL: "/x" } } } });
  try {
    await assert.rejects(acpClient(h).session(), /proxy\.providers: "nope" is not a provider/);
  } finally {
    await h.close();
  }
  await assert.rejects(harness({ proxy: { providers: { codex: "/codex" } } }), /proxy\.providers\.codex must be an object/);
});

test("proxy.providers needs a baseURL for a native provider", async () => {
  const h = await harness({ proxy: { providers: { codex: { loginURL: "https://console.example/settings/codex" } } } });
  try {
    await assert.rejects(acpClient(h).session(), /proxy\.providers\.codex: baseURL is required/);
  } finally {
    await h.close();
  }
});

test("a redirect of a keyed Anthropic request is not followed and the key does not reach the target", async () => {
  const sink = await fakeServer();
  const redirecting = await fakeServer({ redirectTo: sink.url });
  const h = await harness({
    config: { model: "anthropic/claude-fable-5", request_max_retries: 0, stream_max_retries: 0, providers: { anthropic: { base_url: redirecting.url, extra_headers: { "x-api-key": "sk-test-key" } } } },
  });
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

test("a redirect of an injected model endpoint is not followed", async () => {
  const sink = await fakeServer();
  const redirecting = await fakeServer({ redirectTo: sink.url });
  const h = await pageHarness({ codex: { baseURL: redirecting.url + "/codex" } }, { config: { model: "codex/gpt-5.6-sol", request_max_retries: 0, stream_max_retries: 0 } });
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

test("a redirect of an injected model endpoint does not get past the page fetch callback", async () => {
  const sink = await fakeServer();
  const redirecting = await fakeServer({ redirectTo: sink.url });
  const seen = [];
  const pageFetch = async (url, init) => {
    seen.push(init.redirect);
    if (String(url).startsWith("https://synthetic.test")) return new Response(null, { status: 302, headers: { location: sink.url } });
    return fetch(url, { method: init.method, headers: init.headers, body: init.body, redirect: init.redirect });
  };
  try {
    for (const base of [redirecting.url + "/codex", "https://synthetic.test/codex"]) {
      const h = await pageHarness({ codex: { baseURL: base } }, { config: { model: "codex/gpt-5.6-sol", request_max_retries: 0, stream_max_retries: 0 }, fetch: pageFetch });
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
  const sink = await fakeServer();
  const redirecting = await fakeServer({ redirectTo: sink.url });
  // A real fetch that ignores init.redirect follows the 307 and returns the final answer.
  const pageFetch = (url, init) => fetch(url, { method: init.method, headers: init.headers, body: init.body });
  const h = await pageHarness({ codex: { baseURL: redirecting.url + "/codex" } }, { config: { model: "codex/gpt-5.6-sol", request_max_retries: 0, stream_max_retries: 0 }, fetch: pageFetch });
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
    return (await model.modelRequests())[0].body.tools?.map((t) => t.name).sort() ?? [];
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

test("the in-process fetch refuses a redirected response to a request that follows no redirect", async () => {
  const followed = Object.defineProperty(new Response("ok"), "redirected", { value: true });
  const fetch = guardedFetch({}, async () => followed);
  await assert.rejects(fetch("https://model.example/v1", { redirect: "manual" }), /redirect/);
  assert.equal((await fetch("https://model.example/v1", {})).status, 200);
});
