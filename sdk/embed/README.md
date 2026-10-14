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
- `proxy.baseURL` sends all model traffic to a same-origin proxy: the module sets `base_url` of the native `anthropic` and `openai` providers and of each `config.providers` entry that sets none. The module fills `base_url` before it validates the config, so an `openai` entry that has no `base_url` is valid. Put no key in the page: the proxy holds it.

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

The tool takes `{command}` and returns stdout, stderr, and the exit code. The working directory is `/work`; `/tmp` is mounted when the module has it. There is no network (`curl` does not exist), no process, no symbolic link, no version control, and no `tar`. Each call starts a new shell: `cd` and variables end with the call. A call stops after `timeoutMs` (default 10000) or `maxCommands` (default 10000), and each of stdout and stderr is cut to `maxOutput` characters (default 20000). The tool takes `h.fs` from the harness, so it needs no reference to `h`.

just-bash has a browser build, `just-bash/browser`, which has dependencies of its own. A bundler resolves it. Without a bundler, pass `load` with your own build: `shellTool({ load: () => import("/vendor/just-bash.js") })`. The tool does not allow the network, and nothing here lets a caller turn it on.

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
