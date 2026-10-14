# @majorcontext/harness-embed

Runs the harness in a Worker as a WebAssembly module, or as a native `harness acp` subprocess (Node), and gives the caller an ACP stream to it. The package has no dependency and no UI.

```js
import { createHarness, wasm } from "@majorcontext/harness-embed";

const h = await createHarness({
  transport: wasm({ module: "/harness.wasm", wasmExec: "/wasm_exec.js", worker: true }),
  config,                 // config.Config JSON; no keys
  proxy: { baseURL: "/model-proxy", headers: {}, providerBaseURLs: {} },
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
- Node 24 can deadlock in `process.exit` when V8 compiles baseline code (Sparkplug) on worker threads at the same time. The Node host of the harness (`cmd/harness-wasm/node/host.mjs`) turns this off with `v8.setFlagsFromString("--no-concurrent-sparkplug")`. If you build your own Node host for the module, set the same flag, either that way or with `node --no-concurrent-sparkplug`. `NODE_OPTIONS` cannot carry this flag: Node refuses it there.
- `proxy.baseURL` sends all model traffic to a same-origin proxy: the module sets `base_url` of the native `anthropic` and `openai` providers and of each `config.providers` entry that sets none. The module fills `base_url` before it validates the config, so an `openai` entry that has no `base_url` is valid. Put no key in the page: the proxy holds it.
- `proxy.providerBaseURLs` maps a provider name to the `base_url` of its entry, also of a native provider: `{ codex: "/codex" }` sends the ChatGPT backend traffic to that path (see "Log in with ChatGPT"). It is a pass-through path and holds no credential. In a browser, a request of a provider that uses the ChatGPT login to another origin than the page fails before it is sent, whatever option set the URL, because the token would go there. The `fetch` option carries model requests only: the sign-in and the refresh go to the ChatGPT issuer directly, and neither follows a redirect.

## Files and a shell in the browser

With the wasm transport in a browser, `h.fs` reads and writes the file system of the module, the one that the file tools use. The calls are async and take absolute paths; an error has the POSIX `code` (`ENOENT`, `EEXIST`, `ENOTEMPTY`). Under Node the module runs on the disk of the host, and with `subprocess` there is no module, so `h.fs` is `undefined` there.

```js
await h.fs.mkdir("/work/data", { recursive: true });
await h.fs.writeFile("/work/data/people.json", JSON.stringify(people)); // a string or bytes; { append: true } appends
const bytes = await h.fs.readFile("/work/note.txt");                    // Uint8Array
await h.fs.readdir("/work");                                            // string[]
await h.fs.stat("/work/note.txt");                                      // { isDirectory, size, mtimeMs }
await h.fs.rename("/work/a.txt", "/work/b.txt");
await h.fs.rm("/work/data", { recursive: true, force: true });
```

The calls are not atomic: a write of the page and a write of the module to one file can interleave. `rename` cannot move a directory below `/persist`.

`@majorcontext/harness-embed/shell` gives the model a shell over the same files. It needs [`just-bash`](https://github.com/vercel-labs/just-bash) (Apache-2.0), an optional peer dependency: `npm install just-bash`. A page that does not import the export loads nothing of it, and just-bash loads at the first call of the tool (about 360 KB gzip).

```js
import { createHarness, wasm } from "@majorcontext/harness-embed";
import { shellTool } from "@majorcontext/harness-embed/shell";

const h = await createHarness({
  transport: wasm({ module, wasmExec, worker: true }),
  tools: [shellTool()], // the tool is named bash; shellTool({ name: "shell" }) renames it
});
```

The tool takes `{command}` and returns stdout, stderr, and the exit code. The working directory is `/work`; `/tmp` is mounted when the module has it. There is no network (`curl` does not exist), no process, no symbolic link, no version control, and no `tar`. Each call starts a new shell: `cd` and variables end with the call. A call stops after `timeoutMs` (default 10000) or `maxCommands` (default 10000), and each of stdout and stderr is cut to `maxOutput` characters (default 20000, head and tail kept) in the result the model sees. `maxOutputBytes` (default 8 MiB) is a separate limit on the bytes that one call writes, to stdout, stderr, or a file; a command over it stops. A pipeline can therefore write a large file while the model sees a clipped result. The tool takes `h.fs` from the harness, so it needs no reference to `h`.

The default `load` is `import("just-bash/browser")`. That build has dependencies of its own and imports the names `gzipSync`, `gunzipSync`, and `constants` from `node:zlib`, which a browser bundler cannot resolve. Bundle it with `node:zlib` aliased to the stub of this package, `@majorcontext/harness-embed/zlib-stub`; with the stub, `gzip`, `gunzip`, `zcat`, and `rg -z` on a `.gz` file fail with "gzip is not available in the browser" and no other command is affected. With esbuild: `alias: { "node:zlib": "@majorcontext/harness-embed/zlib-stub" }` (the test fixture builds this way). Vite (`resolve.alias`) and webpack 5 (`resolve.alias`) take the same alias; they are not tested here. Without a bundler, build just-bash once that way, serve the file, and pass `load`: `shellTool({ load: () => import("/vendor/just-bash.js") })`. The tool does not allow the network, and nothing here lets a caller turn it on.

Every page tool gets `fs`, and `fs` can write `/persist`, the session store of the harness. Give `fs` only to code that you trust, and do not pass it to code that takes input from the model. The shell tool mounts `/work` and `/tmp` only.

## Log in with ChatGPT

The module signs in with the device code flow, and keeps the ChatGPT token in its store, so the login survives a reload. This holds the token of the user in the page: any script of the page can read it. Use it only when the page is the user's own, as in a demo. An application with many users keeps the proxy of the next section, which holds the credential on the server.

1. Start the harness with a persistent store (`store: { persist: true }` in a browser, `store: { dir }` under Node). A store in memory has no login file, and the module then offers no login.
2. Use an ACP client that advertises `clientCapabilities.elicitation.url` (`{}`). Without it the module offers no login. The module offers the method `chatgpt` in `authMethods`.
3. Call `authenticate` with `{ methodId: "chatgpt" }`. The module sends `elicitation/create` with `mode: "url"`, `requestId` of the `authenticate` request, a `url` (the ChatGPT device page with the code filled in), and a `message` that names the code. Show the URL to the user and answer `{ action: "accept" }` when the user opens it, or `decline` or `cancel` to stop. The module polls the issuer, stores the login under `<store dir>/auth/codex.json` (in a browser, `/persist/sessions/auth/codex.json` by default), sends `elicitation/complete`, and answers `{}`. `$/cancel_request` for the `authenticate` stops the sign-in.
4. Use a model such as `codex/gpt-5.6-sol`. `codex/*` models are listed when the module offers the login or a login file exists.

`auth.openai.com` allows cross-origin calls, so the sign-in and the token refresh run in the browser. `https://chatgpt.com/backend-api/codex` does not, so a browser needs one pass-through path on the server of the page. Set `proxy: { providerBaseURLs: { codex: "/codex" } }`, and forward `/codex/<path>` to `https://chatgpt.com/backend-api/codex/<path>` unchanged: same method, body, and headers, a streamed response, and no credential added. The module sends `Authorization`, `ChatGPT-Account-ID`, and `originator` itself. Keep the route off the open internet for a multi-user application. Under Node there is no CORS, so the module calls the backend directly and needs no option.

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

Test: build the native binary with `go build -o harness ./cmd/harness`, build the module with `GOOS=js GOARCH=wasm go build -o harness.wasm ./cmd/harness-wasm`, build the scripted model server with `go build -o embedfake ./internal/embedfake`, then run `HARNESS_BIN=harness HARNESS_EMBEDFAKE=embedfake HARNESS_WASM=harness.wasm HARNESS_WASM_EXEC=wasm_exec.js node --test sdk/embed/test/*.test.mjs`.
