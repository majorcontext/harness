// Test support: a scripted Anthropic Messages server and a minimal ACP client
// over the stream of a Harness.
import fs from "node:fs";
import { spawn } from "node:child_process";
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

/** Path of the native harness binary, which the Node job builds. */
export function harnessBinary() {
  const bin = process.env.HARNESS_BIN;
  if (!bin) throw new Error("set HARNESS_BIN to the built cmd/harness");
  return bin;
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
      return (list ?? []).map((r) => ({ ...r, headers: Object.fromEntries(Object.entries(r.headers).map(([k, v]) => [k.toLowerCase(), v[0]])) }));
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
  const chunkWaiters = [];
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
        if (value.params.update.sessionUpdate === "agent_message_chunk") chunkWaiters.splice(0).forEach((resolve) => resolve());
      }
    }
  })();
  return {
    updates,
    done,
    /** Resolves when an agent_message_chunk has arrived. */
    streaming: () => (updates.some((u) => u.sessionUpdate === "agent_message_chunk") ? Promise.resolve() : new Promise((resolve) => chunkWaiters.push(resolve))),
    cancelReader: () => reader.cancel(),
    abortWriter: (reason) => writer.abort(reason),
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

const segment = (value) => Buffer.from(JSON.stringify(value)).toString("base64url");

/** A ChatGPT access token (an unsigned JWT) that ends in lifetimeSeconds from now. */
export function chatgptToken(lifetimeSeconds) {
  return [segment({ alg: "none" }), segment({ exp: Math.floor(Date.now() / 1000) + lifetimeSeconds, "https://api.openai.com/auth": { chatgpt_account_id: "acct-1" } }), "sig"].join(".");
}

/** Writes the login file of a store directory. */
export function seedLogin(dir, token) {
  fs.mkdirSync(path.join(dir, "auth"), { mode: 0o700, recursive: true });
  fs.writeFileSync(path.join(dir, "auth", "codex.json"), JSON.stringify({ access_token: token, refresh_token: "refresh-1", account_id: "acct-1" }), { mode: 0o600 });
}

/**
 * Starts a scripted ChatGPT issuer: the device code endpoints and the token endpoint. With redirectTo, every
 * request answers 307 to that URL. requests lists {method, path} of each request.
 */
export async function fakeIssuer({ redirectTo } = {}) {
  const requests = [];
  const server = http.createServer((req, res) => {
    requests.push({ method: req.method, path: req.url });
    req.resume();
    req.on("end", () => {
      if (redirectTo) {
        res.writeHead(307, { location: redirectTo + req.url }).end();
        return;
      }
      const json = (body) => res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify(body));
      if (req.url === "/api/accounts/deviceauth/usercode") json({ device_auth_id: "d1", user_code: "ABCD-1234", interval: "1" });
      else if (req.url === "/api/accounts/deviceauth/token") json({ authorization_code: "code-1", code_challenge: "c", code_verifier: "v" });
      else if (req.url === "/oauth/token") json({ access_token: chatgptToken(3600), refresh_token: "refresh-2", id_token: chatgptToken(3600) });
      else res.writeHead(404).end();
    });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return { url: `http://127.0.0.1:${server.address().port}`, requests, close: () => server.close() };
}
