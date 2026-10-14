// The wasm transport: the module runs in a Worker (a browser Worker or a Node
// worker_threads Worker), and the page speaks ACP to it by messages.
import { pageBridge } from "./bridge.mjs";
import { spawnWorker } from "./endpoint.mjs";
import { acpStream } from "./stream.mjs";

const isNode = typeof globalThis.process?.versions?.node === "string" && typeof globalThis.WorkerGlobalScope === "undefined";

/**
 * wasm names the WebAssembly transport. module is the harness module: a URL,
 * a path (Node), an ArrayBuffer, or a WebAssembly.Module. wasmExec is the URL
 * or path of the wasm_exec.js of the same Go version. The module runs in a
 * Worker; worker must be true.
 */
export function wasm({ module, wasmExec, worker = true }) {
  return { kind: "wasm", module, wasmExec, worker, supports: allOptions, defaultTools: defaultBuiltinTools, start: startWasm };
}

function absolute(value) {
  if (value instanceof URL) return value.href;
  if (typeof value === "string" && typeof globalThis.location?.href === "string") return new URL(value, globalThis.location.href).href;
  return value;
}

/**
 * defaultBuiltinTools is the tool set of a session when createHarness gets no
 * builtinTools: the file and search tools, read_tool_result, and session_info.
 * The module has no exec, so bash and process stay out; task, model, and goal
 * stay out because they start unattended work or switch providers. The names of
 * the page tools join it.
 */
export const defaultBuiltinTools = Object.freeze(["read_file", "write_file", "edit_file", "ls", "glob", "grep", "read_tool_result", "session_info"]);

const allOptions = Object.freeze({ tools: true, builtinTools: true, proxy: true, fetch: true });

const browserStoreDir = "/persist/sessions";

function belowPersist(dir) {
  const parts = dir.split("/");
  return parts[0] === "" && parts[1] === "persist" && parts.length > 2 && parts.slice(1).every((p) => p !== "" && p !== "." && p !== "..");
}

// lockStore returns the function that releases the store of this page. In the
// browser the Worker holds the store lock, so there is nothing to release here.
async function lockStore(store) {
  if (!isNode) {
    if (store.persist && store.dir !== undefined && !belowPersist(store.dir)) {
      throw new Error("store.dir must be a path below /persist in the browser, with no . or .. segment");
    }
    return () => {};
  }
  if (store.persist && !store.dir) throw new Error("store.persist needs store.dir under Node");
  if (!store.dir) return () => {};
  const { acquireStoreLock } = await import("./storelock.mjs");
  return acquireStoreLock(store.dir);
}

async function startWasm(transport, { config, proxy, fetch: pageFetch, tools, builtinTools, store, log }) {
  if (transport.worker !== true) throw new Error("the wasm transport runs in a Worker: set worker: true");
  const unlock = await lockStore(store);
  let w;
  try {
    w = await spawnWorker();
  } catch (err) {
    unlock();
    throw err;
  }
  let closed;
  let failed = false;
  let cleaned;
  let closing;
  // cleanup is the one end of the harness, for a failure and for close(): it
  // stops the bridges and the Worker, releases the store directory once, and
  // ends the stream.
  const cleanup = (err) =>
    (cleaned ??= (async () => {
      bridge.close();
      await w.terminate();
      unlock();
      stream.end(err);
    })());
  const stream = acpStream({
    send(message) {
      if (closing || failed) throw new Error("the harness is closed");
      w.post({ frame: JSON.stringify(message) });
    },
    close: () => close(),
  });
  const bridge = pageBridge({ tools, pageFetch, post: (m, transfer) => w.post(m, transfer) });
  const ready = new Promise((resolve, reject) => {
    w.onMessage((m) => {
      if (m.ready) resolve();
      else if (m.error) reject(Object.assign(new Error(m.error), m.code ? { code: m.code } : {}));
      else if (m.frame !== undefined) stream.emit(JSON.parse(m.frame));
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
    storeDir: isNode ? store.dir : store.persist ? (store.dir ?? browserStoreDir) : undefined,
    persist: !isNode && Boolean(store.persist),
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
  return { stream: { readable: stream.readable, writable: stream.writable }, close };
}
