// The client API of the embedded harness: createHarness starts a transport and
// returns an ACP stream to it. wasm runs the WebAssembly module in a Worker;
// subprocess spawns the native `harness acp` (Node only).
import { defaultBuiltinTools, wasm } from "./wasm.mjs";
import { subprocess } from "./subprocess.mjs";

export { defaultBuiltinTools, subprocess, wasm };

/**
 * createHarness starts the transport and resolves with {stream, close}. stream
 * is {readable, writable} of ACP messages (parsed JSON objects). tools are the
 * page tools {name, description, schema, run(input, {signal})}. builtinTools
 * is the allowed tool names of every session; absent, it is the default set of
 * the transport (defaultBuiltinTools for wasm, none for subprocess) and the
 * names of the page tools. fetch is an optional page fetch that carries the
 * model requests. proxy is {baseURL, headers, providerBaseURLs} of a same-origin proxy. store is
 * {persist, dir}: under Node, dir is an exclusive directory; in the browser,
 * persist keeps the sessions in the origin private file system below dir
 * (default /persist/sessions) under a lock that one page of the origin holds,
 * and createHarness rejects with code "store_in_use" after 2 s while another
 * page holds it. Otherwise the store is in memory. With subprocess, dir is the
 * session directory of the process, and without it the process uses its own
 * default directory. log receives each JSON log
 * line of the module. A transport refuses an option it cannot honor: the
 * promise rejects and nothing starts.
 */
export async function createHarness({ transport, config = {}, proxy = {}, fetch: pageFetch, tools = [], builtinTools, store = {}, log } = {}) {
  if (typeof transport?.start !== "function") throw new Error("transport must come from wasm() or subprocess()");
  const refused = [
    ["tools", tools.length > 0],
    ["builtinTools", builtinTools !== undefined],
    ["proxy", Object.values(proxy).some((v) => v !== undefined)],
    ["fetch", pageFetch !== undefined],
  ].filter(([name, given]) => given && !transport.supports[name]);
  if (refused.length > 0) {
    throw new Error(`the ${transport.kind} transport does not support ${refused.map(([name]) => name).join(", ")}`);
  }
  const allowed = builtinTools ?? (transport.defaultTools && [...transport.defaultTools, ...tools.map((t) => t.name)]);
  return transport.start(transport, { config, proxy, fetch: pageFetch, tools, builtinTools: allowed, store, log });
}
