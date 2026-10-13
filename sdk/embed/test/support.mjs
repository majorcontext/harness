// Test support: a scripted Anthropic Messages server and a minimal ACP client
// over the stream of a Harness.
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";

/** Paths of the module and of its wasm_exec.js, which the Node job builds. */
export function moduleFiles() {
  const module = process.env.HARNESS_WASM;
  const wasmExec = process.env.HARNESS_WASM_EXEC;
  if (!module || !wasmExec) {
    throw new Error("set HARNESS_WASM and HARNESS_WASM_EXEC to the built module and its wasm_exec.js");
  }
  return { module, wasmExec };
}

export function tempDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "harness-embed-"));
}

function sse(res, events) {
  res.writeHead(200, { "content-type": "text/event-stream" });
  for (const e of events) res.write(`event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`);
  res.end();
}

function reply(res, { text = "", toolCalls = [] }) {
  const events = [{ type: "message_start", message: { id: "msg_fake", usage: { input_tokens: 5 } } }];
  let index = 0;
  if (text !== "" || toolCalls.length === 0) {
    events.push({ type: "content_block_start", index, content_block: { type: "text", text: "" } });
    events.push({ type: "content_block_delta", index, delta: { type: "text_delta", text } });
    events.push({ type: "content_block_stop", index });
    index++;
  }
  for (const tc of toolCalls) {
    events.push({ type: "content_block_start", index, content_block: { type: "tool_use", id: tc.id, name: tc.name, input: {} } });
    events.push({ type: "content_block_delta", index, delta: { type: "input_json_delta", partial_json: JSON.stringify(tc.input) } });
    events.push({ type: "content_block_stop", index });
    index++;
  }
  events.push({ type: "message_delta", delta: { stop_reason: toolCalls.length ? "tool_use" : "end_turn" }, usage: { output_tokens: 3 } });
  events.push({ type: "message_stop" });
  sse(res, events);
}

/**
 * Starts a model server that answers its requests in order with the replies.
 * requests holds each request as {path, headers, body}.
 */
export async function fakeModel(replies) {
  const requests = [];
  const waiters = [];
  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const body = JSON.parse(Buffer.concat(chunks).toString() || "{}");
      requests.push({ path: req.url, headers: req.headers, body });
      for (const w of waiters) if (requests.length >= w.n) w.resolve();
      reply(res, replies[Math.min(requests.length, replies.length) - 1]);
    });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    requests,
    /** Resolves when the server has had n requests. */
    reached: (n) => (requests.length >= n ? Promise.resolve() : new Promise((resolve) => waiters.push({ n, resolve }))),
    close: () => server.close(),
  };
}

export function userConfig(baseURL) {
  const anthropic = {};
  if (baseURL) anthropic.base_url = baseURL;
  return { model: "anthropic/claude-fable-5", providers: { anthropic } };
}

/** An ACP client over h.stream. request() resolves with the result of the answer. */
export function acpClient(h) {
  const writer = h.stream.writable.getWriter();
  const reader = h.stream.readable.getReader();
  const waiting = new Map();
  const updates = [];
  let nextId = 1;
  const done = (async () => {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      if (value.id !== undefined && waiting.has(value.id)) {
        const { resolve, reject } = waiting.get(value.id);
        waiting.delete(value.id);
        if (value.error) reject(Object.assign(new Error(value.error.message), { rpc: value.error }));
        else resolve(value.result);
      } else if (value.method === "session/update") {
        updates.push(value.params.update);
      }
    }
  })();
  return {
    updates,
    done,
    cancelReader: () => reader.cancel(),
    request(method, params) {
      const id = nextId++;
      return new Promise((resolve, reject) => {
        waiting.set(id, { resolve, reject });
        writer.write({ jsonrpc: "2.0", id, method, params });
      });
    },
    notify(method, params) {
      return writer.write({ jsonrpc: "2.0", method, params });
    },
    async session() {
      await this.request("initialize", { protocolVersion: 1, clientCapabilities: {} });
      const { sessionId } = await this.request("session/new", { cwd: tempDir(), mcpServers: [] });
      return sessionId;
    },
    prompt(sessionId, text) {
      return this.request("session/prompt", { sessionId, prompt: [{ type: "text", text }] });
    },
  };
}

/** The text of a request body, to look for a tool result in it. */
export function bodyText(request) {
  return JSON.stringify(request.body);
}
