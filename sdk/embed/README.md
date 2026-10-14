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
  builtinTools,           // allowed tool names; absent: defaultBuiltinTools and the names of tools. A list you pass is used as it is: page tools are not added
  store: { persist: true, dir },  // Node: an exclusive directory. Browser: the origin private file system below dir (default /persist/sessions). Absent: memory
  log: (line) => {},
});
// h.stream is { readable, writable } of ACP messages. Connect an ACP client to it.
await h.close();
```

- `wasm_exec.js` is the file of the same Go version as the module (`$(go env GOROOT)/lib/wasm/wasm_exec.js`).
- `proxy.baseURL` sends all model traffic to a same-origin proxy: the module sets `base_url` of the native `anthropic` and `openai` providers and of each `config.providers` entry that sets none. The module fills `base_url` before it validates the config, so an `openai` entry that has no `base_url` is valid. Put no key in the page: the proxy holds it.

## Reach a model

Use a same-origin proxy. It serves the module and `wasm_exec.js`, and it forwards `/model-proxy/<path>` to the model backend. Same origin and CORS are not authorization: any HTTP client can call the proxy and spend the credential. The proxy must authenticate and authorize each request with the session of your application, before it removes page credentials such as the session cookie. It must also apply abuse controls: rate limits and a list of allowed paths. Then it removes `Origin`, `Referer`, `Cookie`, `Sec-*`, and any `Authorization` or `x-api-key` header of the page, and then adds the authentication headers of the backend. The page sets `proxy: { baseURL: "/model-proxy" }` and holds no credential. For a ChatGPT Codex backend, the config is `providers: { codex: { type: "openai", responses_path: "/responses" } }`, and the proxy forwards `/model-proxy/responses` and `/model-proxy/models`, with `Authorization`, `ChatGPT-Account-ID`, and `originator` set on the server. Use `httputil.ReverseProxy` with `FlushInterval: -1`, so the stream is not buffered.

Direct Anthropic access is for demonstration only. Anyone who loads the page can read the key. Set `extra_headers` on the `anthropic` entry. The first header makes Anthropic answer the preflight of the browser. The second carries the key, because `createHarness` gives the module no environment:

```js
config: {
  model: "anthropic/claude-fable-5",
  providers: { anthropic: { extra_headers: { "anthropic-dangerous-direct-browser-access": "true", "x-api-key": key } } },
},
```

Harness does not send this header by itself. Direct OpenAI access is not supported.
- In a browser, one page of the origin holds the persistent store at a time. A second `createHarness` with `store.persist` waits up to 2 s for the store and then rejects with `error.code === "store_in_use"`; start it without `persist` to run with a memory store. The work directory `/work` is in memory.
- `h.stream` has no backpressure in either direction: frames queue without a bound.
- A write after `close()` rejects, and aborting the writable closes the connection. Cancelling the readable closes it too.
- `run` returns a string, `{text, isError}`, or a promise of either. The signal aborts when the turn is canceled.

Test: build the module with `GOOS=js GOARCH=wasm go build -o harness.wasm ./cmd/harness-wasm`, build the scripted model server with `go build -o embedfake ./internal/embedfake`, then run `HARNESS_EMBEDFAKE=embedfake HARNESS_WASM=harness.wasm HARNESS_WASM_EXEC=wasm_exec.js node --test sdk/embed/test/*.test.mjs`.
