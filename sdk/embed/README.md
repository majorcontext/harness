# @majorcontext/harness-embed

Runs the harness WebAssembly module in a Worker and gives the page an ACP stream to it. The package has no dependency and no UI.

```js
import { createHarness, wasm } from "@majorcontext/harness-embed";

const h = await createHarness({
  transport: wasm({ module: "/harness.wasm", wasmExec: "/wasm_exec.js", worker: true }),
  config,                 // config.Config JSON; no keys
  proxy: { baseURL: "/model-proxy", headers: {} },
  fetch,                  // optional: a page fetch that carries the model requests
  tools: [{ name, description, schema, run: async (input, { signal }) => "text" }],
  builtinTools,           // allowed tool names; absent: every tool
  store: { persist: true, dir },  // Node only: an exclusive directory; absent: memory
  log: (line) => {},
});
// h.stream is { readable, writable } of ACP messages. Connect an ACP client to it.
await h.close();
```

- `wasm_exec.js` is the file of the same Go version as the module (`$(go env GOROOT)/lib/wasm/wasm_exec.js`).
- `proxy.baseURL` sets `base_url` of each entry of `config.providers` that sets none. Put no key in the page: a same-origin proxy holds it.
- `run` returns a string, `{text, isError}`, or a promise of either. The signal aborts when the turn is canceled.

Test: build the module with `GOOS=js GOARCH=wasm go build -o harness.wasm ./cmd/harness-wasm`, then run `HARNESS_WASM=harness.wasm HARNESS_WASM_EXEC=wasm_exec.js node --test sdk/embed/test/*.test.mjs`.
