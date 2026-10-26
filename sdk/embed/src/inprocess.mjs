import { refuseRedirect, toolResult } from "./bridge.mjs";
import { installMemoryFs } from "./memoryfs.mjs";
import { acpStream } from "./stream.mjs";

const workDir = "/work";

async function moduleSource(source) {
  if (source instanceof WebAssembly.Module || source instanceof ArrayBuffer || ArrayBuffer.isView(source)) return source;
  const res = await fetch(source);
  if (!res.ok) throw new Error(`loading the module: HTTP ${res.status}`);
  return res.arrayBuffer();
}

async function loadWasmExec(wasmExec) {
  if (typeof globalThis.Go === "function") return;
  if (typeof wasmExec === "function") await wasmExec();
  else await import(wasmExec);
}

// gate returns a promise that stays pending once the module has exited: Go attaches handlers to the promises that it
// gets from the host, and a handler cannot run in a program that has exited.
function gate(state, promise) {
  return new Promise((resolve, reject) => {
    promise.then(
      (value) => state.exited || resolve(value),
      (err) => state.exited || reject(err),
    );
  });
}

function gateBody(state, body) {
  const reader = body.getReader();
  return new ReadableStream({
    async pull(controller) {
      const { value, done } = await gate(state, reader.read());
      if (done) controller.close();
      else controller.enqueue(value);
    },
    cancel: (reason) => gate(state, reader.cancel(reason)),
  });
}

export function guardedFetch(state, pageFetch) {
  return async (url, init) => {
    const res = await gate(state, pageFetch(url, init));
    refuseRedirect(res, init?.redirect);
    return res.body ? new Response(gateBody(state, res.body), res) : res;
  };
}

export async function startInProcess(transport, { config, proxy, fetch: pageFetch, tools, builtinTools, store, log }) {
  if (store.persist || store.dir !== undefined) throw new Error("the in-process wasm transport keeps the store in memory: remove store.persist and store.dir");
  installMemoryFs();
  await loadWasmExec(transport.wasmExec);
  const go = new Go();
  go.argv = ["harness-wasm"];
  go.env = {};
  const source = await moduleSource(transport.module);
  const instance = source instanceof WebAssembly.Module ? await WebAssembly.instantiate(source, go.importObject) : (await WebAssembly.instantiate(source, go.importObject)).instance;
  // Go reads globalThis.fs once, when it starts, so each module gets a tree of its own.
  installMemoryFs();
  const state = { exited: false };
  const exited = go.run(instance);
  const exit = globalThis.harnessExit;
  let conn;
  let closing;
  const stream = acpStream({
    send(message) {
      if (closing) throw new Error("the harness is closed");
      conn.send(JSON.stringify(message));
    },
    close: () => close(),
  });
  conn = globalThis.harnessConnect({
    config,
    builtinTools,
    defaultDir: workDir,
    proxy,
    modelFetch: guardedFetch(state, pageFetch ?? ((url, init) => globalThis.fetch(url, init))),
    tools: tools.map(({ name, description, schema, run }) => ({ name, description, schema, run: async (input, { signal }) => toolResult(await gate(state, Promise.resolve(run(input, { signal })))) })),
    onFrame: (frame) => stream.emit(JSON.parse(frame)),
    onLog: (line) => log?.(line),
  });
  function close() {
    return (closing ??= conn.close().then(() => {
      state.exited = true;
      exit();
      // wasm_exec.js keeps its timers after the program exits, and each one throws when it fires.
      for (const timer of go._scheduledTimeouts.values()) clearTimeout(timer);
      stream.end();
      return exited;
    }));
  }
  return { stream: { readable: stream.readable, writable: stream.writable }, close };
}
