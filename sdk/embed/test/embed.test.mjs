// Tests of the client API over the Worker transport. They run the real wasm
// module in a worker_threads Worker against a scripted model server. Run with
// `node --test sdk/embed/test/*.test.mjs` after setting HARNESS_WASM and
// HARNESS_WASM_EXEC (see support.mjs).
import { test } from "node:test";
import { Worker } from "node:worker_threads";
import assert from "node:assert/strict";
import { createHarness, defaultBuiltinTools, wasm } from "../src/index.mjs";
import { workerBridge } from "../src/workerbridge.mjs";
import { acpClient, bodyText, fakeModel, moduleFiles, tempDir, userConfig } from "./support.mjs";

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

test("close ends the stream and a second close settles", async () => {
  const h = await harness({ config: userConfig("http://127.0.0.1:1") });
  const c = acpClient(h);
  await c.request("initialize", { protocolVersion: 1, clientCapabilities: {} });
  await h.close();
  await c.done;
  await h.close();
});

test("a transport that is not a Worker is refused", async () => {
  await assert.rejects(createHarness({ transport: wasm({ ...moduleFiles(), worker: false }) }), /Worker/);
});

test("a frame after the page cancels the readable does not throw, and the connection closes", async () => {
  const model = await fakeModel([{ text: "late" }]);
  const h = await harness({ config: userConfig(model.url) });
  try {
    const c = acpClient(h);
    const sessionId = await c.session();
    c.prompt(sessionId, "go").catch(() => {});
    await c.cancelReader();
    await h.close();
  } finally {
    model.close();
  }
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

test("a write after close rejects", async () => {
  const h = await harness({ config: userConfig("http://127.0.0.1:1") });
  const writer = h.stream.writable.getWriter();
  await h.close();
  await assert.rejects(writer.write({ jsonrpc: "2.0", id: 1, method: "initialize", params: {} }));
});

test("aborting the writable closes the connection", async () => {
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
