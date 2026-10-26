# @majorcontext/harness-embed

Runs the harness in a Worker as a WebAssembly module, or as a native `harness acp` subprocess (Node), and gives the caller an ACP stream to it. The package has no dependency and no UI.

```js
import { createHarness, wasm } from "@majorcontext/harness-embed";

const h = await createHarness({
  transport: wasm({ module: "/harness.wasm", wasmExec: "/wasm_exec.js", worker: true }),
  config: {
    model: "anthropic/claude-fable-5",
    providers: { anthropic: { auth: "injected" } },
  },
  proxy: { baseURL: "/model-proxy", headers: {}, providers: {} },
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
- `proxy.baseURL` sends all model traffic to a same-origin proxy: the module sets `base_url` of the native `anthropic` and `openai` providers and of each `config.providers` entry that sets none. The module fills `base_url` before it validates the config, so an `openai` entry that has no `base_url` is valid. A URL alone is not a credential: set `config.providers.anthropic.auth` (or `openai.auth`) to `"injected"` when the proxy adds the credential. Put no key in the page.
- `proxy.providers` maps a provider name to `{ baseURL, loginURL }`, which set `base_url` and `login_url` of its entry: `{ codex: { baseURL: "/codex", loginURL: "/settings/codex" } }` (see "Log in through your server"). Both fields are optional, except that a native provider such as `codex` that `config` does not define needs `baseURL`; the module then also sets `auth: "injected"`. Relative URLs resolve against the page. The module follows no redirect of a model request, so a server answers `401`, not a redirect to a sign-in page. A `fetch` callback must pass `init.redirect` on to the network: when it follows a redirect itself, the module cannot stop the request that the redirect makes.

Add the `config` and `proxy` options above to the tool examples below.

## Run in Cloudflare Workers

A Cloudflare Worker cannot start a Worker, so the default `wasm` transport does
not run there. Pass `worker: false` and the module runs in the isolate of the
Worker on an in-memory file system. Bundle the module with the Worker: Wrangler
imports a `.wasm` file as a compiled `WebAssembly.Module`, and
`wasm_exec.js` of the same Go version (`$(go env GOROOT)/lib/wasm/wasm_exec.js`)
loads through a function, so that it runs after the file system exists.

```js
import { createHarness, wasm } from "@majorcontext/harness-embed";
import harnessModule from "./harness.wasm";

export default {
  async fetch(request, env) {
    const h = await createHarness({
      transport: wasm({
        module: harnessModule,
        wasmExec: () => import("./wasm_exec.js"),
        worker: false,
      }),
      config: { model: "my-model/example", providers: { "my-model": { type: "openai-compat", base_url: env.MODEL_URL } } },
      fetch: (url, init) => {
        const headers = new Headers(init?.headers);
        headers.set("authorization", `Bearer ${env.MODEL_KEY}`);
        return fetch(url, { ...init, headers });
      },
    });
    try {
      // Speak ACP on h.stream: initialize, session/new with cwd "/work", session/prompt.
    } finally {
      await h.close();
    }
  },
};
```

- Start the module inside the request handler. A Worker forbids `setTimeout` at
  global scope, and the module starts timers.
- Pass `fetch`. The module calls it for each model request, so the Worker adds
  the credential and the module holds none. Do not put a key in `config`.
- The store is in memory, and the work directory is `/work`. `store.persist` and
  `store.dir` reject, and `h.fs` is `undefined`. The sessions end with the
  isolate.
- The mode replaces `globalThis.fs`, `process`, and `path`, so use it in a
  Worker or another isolate that has no real process. Under Node,
  `createHarness` rejects it.
- Without a file system, `session/new` fails and says so. To load the module
  your own way, call `installMemoryFs()` from
  `@majorcontext/harness-embed/memoryfs` before `wasm_exec.js` runs.
- A deployed Worker reads timers as frozen during CPU work, so a timer in the
  Worker does not measure CPU time. Measure it with `wrangler tail`.

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

## Tools from a page with WebMCP

`@majorcontext/harness-embed/webmcp` offers the [WebMCP](https://webmachinelearning.github.io/webmcp/) tools of the page to the model. `webmcpTools` reads `document.modelContext.getTools()` and returns the tools as page tools:

```js
import { createHarness, wasm } from "@majorcontext/harness-embed";
import { webmcpTools } from "@majorcontext/harness-embed/webmcp";

const h = await createHarness({
  transport: wasm({ module, wasmExec, worker: true }),
  tools: await webmcpTools(), // { prefix: "webmcp__", fromOrigins: [], log }
});
```

A tool is named `prefix` and its WebMCP name, and keeps its description and input schema. A tool with a name that providers do not take, or with no JSON Schema object as `inputSchema`, is left out and `log` names it. `fromOrigins` adds the tools that frames of those origins expose, and the description of such a tool names its origin. The list is a snapshot: call `webmcpTools` again and start a new harness to see tools that the page registers later. A call to a tool that the page has unregistered fails with `NotFoundError`.

`webmcpTools` never rejects. It returns an empty list where the page has no `document.modelContext` (no WebMCP in the browser, or Node) or `getTools` rejects, so a page can always call it. A failed call is an error result with the name and message of the exception. A call that navigates the page ends the page and the Worker. The hints are data for the model and block no call: `readOnlyHint` and `consequentialHint` add a line to the description, and the result of a tool with `untrustedContentHint` is wrapped in `<untrusted_tool_output>` tags with each `<` escaped. A page that wants the user to confirm a call asks in its `execute` callback.

## Log in through your server

The module holds no model credential, and signs in no user itself. A server holds the credential of each user and adds it to the request. The page marks a provider as `injected`, and the module sends no `Authorization`, `x-api-key`, `ChatGPT-Account-ID`, or `originator`, and no user identifier:

```js
const h = await createHarness({
  transport: wasm({ module, wasmExec, worker: true }),
  config: {
    model: "codex/gpt-5.6-sol",
    providers: { codex: { type: "openai", auth: "injected", models_from: "chatgpt", responses_path: "/responses",
      omit_response_params: ["max_output_tokens", "temperature", "top_p", "metadata"], sanitize_tool_schemas: true } },
  },
  proxy: { providers: { codex: { baseURL: "/codex", loginURL: "/settings/codex" } } },
});
```

Without a `providers.codex` entry in `config`, `proxy.providers.codex` starts from the native `codex` entry and sets `auth: "injected"` too. Put the base URL on a path of the page origin: `fetch` then sends the cookies of the origin, and the server knows the user from its own session.

The server contract:

1. A request with no usable credential for the caller gets `401`, never a redirect to a sign-in page. The module maps `401` to the error `login_required`.
2. The wire's model-list GET is the status probe of the entry, even without `models_from`. For Anthropic it is `GET <base_url>/v1/models` with `Anthropic-Version`; for Responses it replaces the last segment of `responses_path` with `models`. Authorize it exactly like a model call: the same session and no server cache. ACP `session/new`, `session/load`, and `session/resume` send the probe, with a 30-second client cache. A cross-origin probe can add a CORS preflight. A `200` is signed in and a `401` rejects the session request with `login_required`. A server with no probe answers `404` or `405`; only a model call then reports `login_required`. `createHarness` and ACP `initialize` send no probe.
3. The page at `loginURL` runs the sign-in on the server, binds it to the session of the user, and defends against cross-site requests on its start and finish routes. It never returns a token to the page.
4. The server defends against cross-site requests to the model path: `SameSite` cookies, a check of `Sec-Fetch-Site`, or a custom header that the page sets with `proxy.headers`.

For the user, set `loginURL` and use an ACP client that advertises `clientCapabilities.elicitation.url` (`{}`). The module then offers the method `injected-codex` in `authMethods`. `authenticate` with `{ methodId: "injected-codex" }` probes first and answers `{}` when the server accepts. Otherwise the module sends `elicitation/create` with `mode: "url"`, `requestId` of the `authenticate` request, the `loginURL` as `url`, and a `message`. Show the URL, and answer `{ action: "accept" }` when the user opens it, or `decline` or `cancel` to stop. The module then probes until the server accepts (for at most 10 minutes), sends `elicitation/complete`, and answers `{}`. `$/cancel_request` for the `authenticate` stops the sign-in. The module offers no ChatGPT method of its own and reads no login file.

The module also hashes the session ID in Responses `prompt_cache_key` instead of sending the raw ID. A model call that gets `401` fails with `login_required`, except within two minutes of a successful probe: it fails as `permanent` because the server accepted the probe but rejected the call. A `403` is not a login error. Without `loginURL`, the module still probes but offers no interactive auth method for that entry.

## Reach a model

Use a same-origin proxy. It serves the module and `wasm_exec.js`, and it forwards `/model-proxy/<path>` to the model backend. Same origin and CORS are not authorization: any HTTP client can call the proxy and spend the credential. The proxy must authenticate and authorize each request with the session of your application, before it removes page credentials such as the session cookie. It must also apply abuse controls: rate limits and a list of allowed paths. Then it removes `Origin`, `Referer`, `Cookie`, `Sec-*`, and any `Authorization` or `x-api-key` header of the page, and then adds the authentication headers of the backend. The page sets `proxy: { baseURL: "/model-proxy" }` and `config.providers.anthropic.auth: "injected"` (or `openai.auth`) and holds no credential. A URL alone does not make a native API-key provider available. `session/new` rejects a native provider without credentials with `model_unavailable` before a turn starts, also for the default model. For per-user credentials, the server adds the credential of the user (see "Log in through your server"). Use `httputil.ReverseProxy` with `FlushInterval: -1`, so the stream is not buffered.

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
