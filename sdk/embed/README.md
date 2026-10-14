# @majorcontext/harness-embed

Runs the harness in a Worker as a WebAssembly module, or as a native `harness acp` subprocess (Node), and gives the caller an ACP stream to it. The package has no dependency and no UI.

```js
import { createHarness, wasm } from "@majorcontext/harness-embed";

const h = await createHarness({
  transport: wasm({ module: "/harness.wasm", wasmExec: "/wasm_exec.js", worker: true }),
  config,                 // config.Config JSON; no keys
  proxy: { baseURL: "/model-proxy", headers: {} },
  fetch,                  // optional: a page fetch that carries the model requests
  tools: [{ name, description, schema, run: async (input, { signal }) => "text" }],
  builtinTools,           // allowed tool names; absent: defaultBuiltinTools and the names of tools. A list you pass is used as it is: page tools are not added
  store: { persist: true, dir },  // Node: an exclusive directory. Browser: the origin private file system below dir (default /persist/sessions). Absent: memory
  log: (line) => {},
});
// h.stream is { readable, writable } of ACP messages. Connect an ACP client to it.
await h.close();
```

- `subprocess` is the other transport (Node only): `createHarness({transport: subprocess({command: "harness", args: ["acp"], env, cwd, closeTimeout}), config, store, log})` spawns the native binary, which has its own tools, `bash` and `process` among them, so `defaultBuiltinTools` does not apply. `config` goes in a temporary file that `HARNESS_CONFIG_FILE` names (`transport.env` cannot override it); the project config `<cwd>/.harness.json` still merges over it, and the `HARNESS_*` variables still apply. `store.dir` is the session directory (`HARNESS_SESSION_DIR`; the process holds its own lock); without it the sessions go to `~/.harness/sessions`, not to memory, and `persist` has no effect. The Codex login and the plugin cache stay in their usual place. The transport refuses `tools`, `builtinTools`, `proxy`, and `fetch`: `createHarness` rejects with the name of the option. Stdout carries ACP frames only; stderr goes to `log`, one line at a time. `close()` ends the input of the process, then sends `SIGTERM` and `SIGKILL` after `closeTimeout` ms (default 5000) each, and rejects when the process exits with a status other than 0, dies from a signal, or needed a signal. A command that cannot start rejects `createHarness`.
- `wasm_exec.js` is the file of the same Go version as the module (`$(go env GOROOT)/lib/wasm/wasm_exec.js`).
- `proxy.baseURL` sends all model traffic to a same-origin proxy: the module sets `base_url` of the native `anthropic` and `openai` providers and of each `config.providers` entry that sets none. Put no key in the page: the proxy holds it.
- In a browser, one page of the origin holds the persistent store at a time. A second `createHarness` with `store.persist` waits up to 2 s for the store and then rejects with `error.code === "store_in_use"`; start it without `persist` to run with a memory store. The work directory `/work` is in memory.
- `h.stream` has no backpressure in either direction: frames queue without a bound.
- A write after `close()` rejects, and aborting the writable closes the connection. Cancelling the readable closes it too.
- `run` returns a string, `{text, isError}`, or a promise of either. The signal aborts when the turn is canceled.

Test: build the native binary with `go build -o harness ./cmd/harness`, build the module with `GOOS=js GOARCH=wasm go build -o harness.wasm ./cmd/harness-wasm`, build the scripted model server with `go build -o embedfake ./internal/embedfake`, then run `HARNESS_BIN=harness HARNESS_EMBEDFAKE=embedfake HARNESS_WASM=harness.wasm HARNESS_WASM_EXEC=wasm_exec.js node --test sdk/embed/test/*.test.mjs`.
