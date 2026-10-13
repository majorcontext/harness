// Test support: a scripted Anthropic Messages server and a minimal ACP client
// over the stream of a Harness.
import fs from "node:fs";
import { spawn } from "node:child_process";
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

/**
 * Starts the scripted Anthropic server of harnesstest (internal/embedfake) with
 * replies [{text, toolCalls: [{id, name, input}], block}], answered in order.
 * requests() resolves with each request as {path, headers, body}.
 */
export async function fakeModel(replies) {
  const bin = process.env.HARNESS_EMBEDFAKE;
  if (!bin) throw new Error("set HARNESS_EMBEDFAKE to the built internal/embedfake");
  const child = spawn(bin, [JSON.stringify(replies)], { stdio: ["ignore", "pipe", "inherit"] });
  const url = await new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (code) => reject(new Error(`embedfake exited with code ${code}`)));
    child.stdout.once("data", (chunk) => resolve(chunk.toString().trim()));
  });
  return {
    url,
    async requests() {
      const list = await (await fetch(url + "/__requests")).json();
      return list.map((r) => ({ ...r, headers: Object.fromEntries(Object.entries(r.headers).map(([k, v]) => [k.toLowerCase(), v[0]])) }));
    },
    close: () => child.kill("SIGTERM"),
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
