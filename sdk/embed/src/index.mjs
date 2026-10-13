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
  let streamOpen = true;
  let closed;
  let failed = false;
  let cleaned;
  const emit = (frame) => {
    if (!streamOpen) return;
    try {
      controller.enqueue(frame);
    } catch {
      streamOpen = false;
    }
  };
  const endStream = (err) => {
    if (!streamOpen) return;
    streamOpen = false;
    try {
      if (err) controller.error(err);
      else controller.close();
    } catch {
      // The reader canceled the stream.
    }
  };
  // cleanup is the one end of the harness, for a failure and for close(): it
  // stops the bridges and the Worker, releases the store directory once, and
  // ends the stream.
  const cleanup = (err) =>
    (cleaned ??= (async () => {
      bridge.close();
      await w.terminate();
      unlock();
      endStream(err);
    })());
  const readable = new ReadableStream({
    start: (c) => (controller = c),
    cancel() {
      streamOpen = false;
      return close();
    },
  });
  const bridge = pageBridge({ tools, pageFetch, post: (m, transfer) => w.post(m, transfer) });
  const ready = new Promise((resolve, reject) => {
    w.onMessage((m) => {
      if (m.ready) resolve();
      else if (m.error) reject(new Error(m.error));
      else if (m.frame !== undefined) emit(JSON.parse(m.frame));
      else if (m.log !== undefined) log?.(m.log);
      else if (m.closed) closed?.();
      else bridge.handle(m);
    });
    w.onFailure((err) => {
      failed = true;
      reject(err);
      closed?.();
      cleanup(err);
    });
  });
  const options = {
    config,
    builtinTools,
    proxy: { headers: proxy.headers, baseURL: proxy.baseURL === undefined ? undefined : absolute(proxy.baseURL) },
    storeDir: isNode ? store.dir : undefined,
    tools: tools.map(({ name, description, schema }) => ({ name, description, schema })),
    bridgeFetch: typeof pageFetch === "function",
  };
  w.post({ init: { module: absolute(transport.module), wasmExec: absolute(transport.wasmExec), options } });
  try {
    await ready;
  } catch (err) {
    await cleanup(err);
    throw err;
  }
  let closing;
  function close() {
    return (closing ??= (async () => {
      if (!failed) {
        const done = new Promise((resolve) => (closed = resolve));
        w.post({ close: true });
        await done;
      }
      await cleanup();
    })());
  }
  const writable = new WritableStream({
    write(message) {
      if (closing || failed) throw new Error("the harness is closed");
      w.post({ frame: JSON.stringify(message) });
    },
    close,
    abort: close,
  });
  return { stream: { readable, writable }, close };
}
