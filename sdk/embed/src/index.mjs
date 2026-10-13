// The page-side client API of the embedded harness: createHarness starts the
// WebAssembly module in a Worker and returns an ACP stream to it.
import { pageBridge } from "./bridge.mjs";
import { spawnWorker } from "./endpoint.mjs";

const isNode = typeof globalThis.process?.versions?.node === "string" && typeof globalThis.WorkerGlobalScope === "undefined";

/**
 * wasm names the WebAssembly transport. module is the harness module: a URL,
 * a path (Node), an ArrayBuffer, or a WebAssembly.Module. wasmExec is the URL
 * or path of the wasm_exec.js of the same Go version. The module runs in a
 * Worker; worker must be true.
 */
export function wasm({ module, wasmExec, worker = true }) {
  return { kind: "wasm", module, wasmExec, worker };
}

function absolute(value) {
  if (value instanceof URL) return value.href;
  if (typeof value === "string" && typeof globalThis.location?.href === "string") return new URL(value, globalThis.location.href).href;
  return value;
}

// withProxy fills base_url of each provider entry that sets none.
function withProxy(config, proxy) {
  if (!proxy?.baseURL) return config;
  const base = absolute(proxy.baseURL);
  const providers = {};
  for (const [name, provider] of Object.entries(config.providers ?? {})) {
    providers[name] = provider.base_url || provider.type === "claude-code-cli" ? provider : { ...provider, base_url: base };
  }
  return { ...config, providers };
}

async function lockStore(store) {
  if (!isNode) {
    if (store.persist) throw new Error("a persistent store is not available in the browser");
    return () => {};
  }
  if (store.persist && !store.dir) throw new Error("store.persist needs store.dir under Node");
  if (!store.dir) return () => {};
  const { acquireStoreLock } = await import("./storelock.mjs");
  return acquireStoreLock(store.dir);
}

/**
 * createHarness starts the module and resolves with {stream, close}. stream is
 * {readable, writable} of ACP messages (parsed JSON objects). tools are the
 * page tools {name, description, schema, run(input, {signal})}. fetch is an
 * optional page fetch that carries the model requests. proxy is {baseURL,
 * headers} of a same-origin proxy. store is {persist, dir}: under Node, dir is
 * an exclusive directory; otherwise the store is in memory. log receives each
 * JSON log line of the module.
 */
export async function createHarness({ transport, config = {}, proxy = {}, fetch: pageFetch, tools = [], builtinTools, store = {}, log } = {}) {
  if (transport?.kind !== "wasm") throw new Error("transport must come from wasm()");
  if (transport.worker !== true) throw new Error("the wasm transport runs in a Worker: set worker: true");
  const unlock = await lockStore(store);
  let w;
  try {
    w = await spawnWorker();
  } catch (err) {
    unlock();
    throw err;
  }
  let controller;
  let closed;
  const readable = new ReadableStream({ start: (c) => (controller = c) });
  const bridge = pageBridge({ tools, pageFetch, post: (m, transfer) => w.post(m, transfer) });
  const ready = new Promise((resolve, reject) => {
    w.onMessage((m) => {
      if (m.ready) resolve();
      else if (m.error) reject(new Error(m.error));
      else if (m.frame !== undefined) controller.enqueue(JSON.parse(m.frame));
      else if (m.log !== undefined) log?.(m.log);
      else if (m.closed) closed?.();
      else bridge.handle(m);
    });
    w.onFailure((err) => {
      reject(err);
      closed?.();
      try {
        controller.error(err);
      } catch {
        // The stream has ended.
      }
    });
  });
  const options = {
    config: withProxy(config, proxy),
    builtinTools,
    proxyHeaders: proxy.headers,
    storeDir: isNode ? store.dir : undefined,
    tools: tools.map(({ name, description, schema }) => ({ name, description, schema })),
    bridgeFetch: typeof pageFetch === "function",
  };
  w.post({ init: { module: absolute(transport.module), wasmExec: absolute(transport.wasmExec), options } });
  try {
    await ready;
  } catch (err) {
    bridge.close();
    await w.terminate();
    unlock();
    throw err;
  }
  let closing;
  const close = () =>
    (closing ??= (async () => {
      const done = new Promise((resolve) => (closed = resolve));
      w.post({ close: true });
      await done;
      bridge.close();
      await w.terminate();
      try {
        controller.close();
      } catch {
        // The stream failed first.
      }
      unlock();
    })());
  const writable = new WritableStream({
    write(message) {
      if (!closing) w.post({ frame: JSON.stringify(message) });
    },
    close,
  });
  return { stream: { readable, writable }, close };
}
