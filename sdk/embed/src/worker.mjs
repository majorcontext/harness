// The Worker script. It loads the WebAssembly module, calls harnessConnect, and
// moves ACP frames between the module and the page. It runs in a browser Worker
// and in a Node worker_threads Worker.
import { workerPort } from "./endpoint.mjs";
import { workerBridge } from "./workerbridge.mjs";

const port = await workerPort();
const inNode = typeof globalThis.process?.versions?.node === "string" && typeof globalThis.WorkerGlobalScope === "undefined";
const bridge = workerBridge(port.post);
let conn;

async function loadWasmExec(source) {
  if (inNode) {
    const { createRequire } = await import("node:module");
    const { fileURLToPath } = await import("node:url");
    const fs = await import("node:fs");
    const path = await import("node:path");
    globalThis.fs = fs.default;
    globalThis.path = path.default;
    globalThis.crypto ??= (await import("node:crypto")).webcrypto;
    createRequire(import.meta.url)(source.startsWith("file:") ? fileURLToPath(source) : source);
  } else {
    await import(source);
  }
}

async function moduleBytes(source) {
  if (source instanceof WebAssembly.Module) return source;
  if (source instanceof ArrayBuffer || ArrayBuffer.isView(source)) return source;
  if (inNode && !/^https?:/.test(source)) {
    const { readFile } = await import("node:fs/promises");
    const { fileURLToPath } = await import("node:url");
    return readFile(source.startsWith("file:") ? fileURLToPath(source) : source);
  }
  const res = await fetch(source);
  if (!res.ok) throw new Error(`loading the module: HTTP ${res.status}`);
  return res.arrayBuffer();
}

async function start({ module, wasmExec, options }) {
  await loadWasmExec(wasmExec);
  const source = await moduleBytes(module);
  const go = new Go();
  go.argv = ["harness-wasm"];
  go.env = inNode ? { TMPDIR: (await import("node:os")).tmpdir() } : {};
  const result = source instanceof WebAssembly.Module
    ? { module: source, instance: await WebAssembly.instantiate(source, go.importObject) }
    : await WebAssembly.instantiate(source, go.importObject);
  go.run(result.instance);
  if (options.bridgeFetch) globalThis.fetch = bridge.fetch;
  const connect = {
    config: options.config,
    builtinTools: options.builtinTools,
    storeDir: options.storeDir,
    proxy: { headers: options.proxyHeaders },
    tools: options.tools.map((spec) => ({ ...spec, run: (input, { signal }) => bridge.callTool(spec.name, input, signal) })),
    onFrame: (frame) => port.post({ frame }),
    onLog: (line) => port.post({ log: line }),
  };
  conn = globalThis.harnessConnect(connect);
}

port.onMessage(async (m) => {
  if (m.init) {
    try {
      await start(m.init);
      port.post({ ready: true });
    } catch (err) {
      port.post({ error: err instanceof Error ? err.message : String(err) });
    }
  } else if (m.frame !== undefined) {
    conn?.send(m.frame);
  } else if (m.close) {
    await conn?.close();
    port.post({ closed: true });
  } else {
    bridge.handle(m);
  }
});
