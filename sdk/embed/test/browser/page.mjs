// The page side of the browser job: it runs inside Chromium, starts the module
// in a Worker through createHarness, and runs one row against a fake model
// server. e2e/browser_test.go serves this file and asserts on the results.
import { createHarness, wasm } from "/embed/index.mjs";

const echo = {
  name: "js_echo",
  description: "Returns its text.",
  schema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
  run: async (input) => "echo:" + input.text,
};

function acpClient(h) {
  const writer = h.stream.writable.getWriter();
  const reader = h.stream.readable.getReader();
  const waiting = new Map();
  const chunkWaiters = [];
  const text = [];
  let nextId = 1;
  (async () => {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      if (value.id !== undefined && waiting.has(value.id)) {
        waiting.get(value.id)(value);
        waiting.delete(value.id);
      } else if (value.method === "session/update" && value.params.update.sessionUpdate === "agent_message_chunk") {
        text.push(value.params.update.content.text);
        chunkWaiters.splice(0).forEach((fn) => fn());
      }
    }
  })();
  return {
    text,
    call(method, params) {
      const id = nextId++;
      return new Promise((resolve) => {
        waiting.set(id, resolve);
        writer.write({ jsonrpc: "2.0", id, method, params });
      });
    },
    notify: (method, params) => writer.write({ jsonrpc: "2.0", method, params }),
    firstChunk: () => (text.length ? Promise.resolve() : new Promise((resolve) => chunkWaiters.push(resolve))),
  };
}

function config(row) {
  const anthropic = {};
  if (row.baseURL) anthropic.base_url = row.baseURL;
  if (row.extraHeaders) anthropic.extra_headers = row.extraHeaders;
  return { model: "anthropic/claude-fable-5", providers: { anthropic } };
}

// open starts the module in a Worker with the paths relative to this page, and
// opens one ACP session.
async function open(row, log = () => {}) {
  const logs = [];
  const started = performance.now();
  const h = await createHarness({
    transport: wasm({ module: "harness.wasm", wasmExec: "wasm_exec.js", worker: true }),
    config: config(row),
    proxy: row.proxy,
    tools: [echo],
    log: (line) => (logs.push(line), log(line)),
  });
  const bootMs = performance.now() - started;
  const c = acpClient(h);
  await c.call("initialize", { protocolVersion: 1, clientCapabilities: {} });
  const created = await c.call("session/new", { cwd: "/", mcpServers: [] });
  if (created.error) throw new Error("session/new: " + JSON.stringify(created.error) + "\n" + logs.join("\n"));
  return { h, c, bootMs, sessionId: created.result.sessionId };
}

const prompt = (c, sessionId, text) => c.call("session/prompt", { sessionId, prompt: [{ type: "text", text }] });

const rows = {
  async turn(row) {
    const { h, c, bootMs, sessionId } = await open(row);
    const done = await prompt(c, sessionId, "go");
    await h.close();
    return { bootMs, stopReason: done.result?.stopReason, error: done.error, text: c.text.join("") };
  },

  async cancel(row) {
    const { h, c, sessionId } = await open(row);
    const turn = prompt(c, sessionId, "go");
    await c.firstChunk();
    const canceled = performance.now();
    await c.notify("session/cancel", { sessionId });
    const done = await turn;
    const endedMs = performance.now() - canceled;
    await h.close();
    return { stopReason: done.result?.stopReason, error: done.error, endedMs };
  },

  // offline leaves the network after the module booted. The driver returns it
  // when the first model request fails, so the turn must retry to finish.
  async offline(row) {
    const { h, c, sessionId } = await open(row);
    await globalThis.goOffline();
    const done = await prompt(c, sessionId, "go");
    await h.close();
    return { stopReason: done.result?.stopReason, error: done.error, text: c.text.join("") };
  },
};

/** runRow runs the row named by row.kind and returns its result as JSON data. */
export async function runRow(row) {
  try {
    return await rows[row.kind](row);
  } catch (err) {
    return { failure: String(err?.stack ?? err) };
  }
}
