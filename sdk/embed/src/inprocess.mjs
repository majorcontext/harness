// The in-process wasm transport: the module runs in the JavaScript isolate of the caller, on an in-memory file
// system, for a host that cannot start a Worker, such as a Cloudflare Worker.
import { toolResult } from "./bridge.mjs";
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
    modelFetch: typeof pageFetch === "function" ? pageFetch : undefined,
    tools: tools.map(({ name, description, schema, run }) => ({ name, description, schema, run: async (input, { signal }) => toolResult(await run(input, { signal })) })),
    onFrame: (frame) => stream.emit(JSON.parse(frame)),
    onLog: (line) => log?.(line),
  });
  function close() {
    return (closing ??= conn.close().then(() => {
      exit();
      stream.end();
      return exited;
    }));
  }
  return { stream: { readable: stream.readable, writable: stream.writable }, close };
}
