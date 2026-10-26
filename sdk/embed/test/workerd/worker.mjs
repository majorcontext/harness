// The Worker under test: one ACP turn on the module in the isolate of the Worker, through createHarness with
// worker: false. The bundle keeps ./harness.wasm and ./wasm_exec.js as modules of the Worker.
import { createHarness, wasm } from "../../src/index.mjs";
import harnessModule from "./harness.wasm";

const initialize = { protocolVersion: 1, clientCapabilities: {} };

// client sends requests on the stream and resolves each with its result. It collects the text chunks of the agent.
function client(h) {
  const writer = h.stream.writable.getWriter();
  const reader = h.stream.readable.getReader();
  const waiting = new Map();
  const chunks = [];
  let next = 1;
  const pump = (async () => {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      const update = value.params?.update;
      if (value.method === "session/update" && update.sessionUpdate === "agent_message_chunk") chunks.push(update.content.text);
      const entry = waiting.get(value.id);
      if (!entry) continue;
      waiting.delete(value.id);
      if (value.error) entry.reject(new Error(`${value.error.message}: ${JSON.stringify(value.error.data)}`));
      else entry.resolve(value.result);
    }
  })();
  return {
    chunks,
    pump,
    request(method, params) {
      const id = next++;
      return new Promise((resolve, reject) => {
        waiting.set(id, { resolve, reject });
        writer.write({ jsonrpc: "2.0", id, method, params });
      });
    },
  };
}

export default {
  async fetch(request, env) {
    // The Worker adds the credential. The module never holds it.
    const modelFetch = (url, init) => {
      const headers = new Headers(init?.headers);
      headers.set("authorization", `Bearer ${env.MODEL_KEY}`);
      return fetch(url, { ...init, headers });
    };
    const h = await createHarness({
      transport: wasm({ module: harnessModule, wasmExec: () => import("./wasm_exec.js"), worker: false }),
      config: { model: "mine/m", providers: { mine: { type: "openai-compat", base_url: env.MODEL_URL } } },
      fetch: modelFetch,
    });
    try {
      const c = client(h);
      await c.request("initialize", initialize);
      const { sessionId } = await c.request("session/new", { cwd: "/work", mcpServers: [] });
      const { stopReason } = await c.request("session/prompt", { sessionId, prompt: [{ type: "text", text: "say hello" }] });
      return Response.json({ stopReason, text: c.chunks.join("") });
    } catch (err) {
      return Response.json({ failure: String(err?.message ?? err) }, { status: 500 });
    } finally {
      await h.close();
    }
  },
};
