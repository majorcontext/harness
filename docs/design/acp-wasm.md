# ACP and WebAssembly: one adapter, two hosts

Harness speaks the Agent Client Protocol (ACP) through one adapter, and the same core runs as WebAssembly in a browser and in Node. Andy approved the decisions of section 12. Each PR in section 14 edits the spec sections it implements, in the same PR (section 10).

Sources:
- ACP v1 pages `https://agentclientprotocol.com/protocol/v1/<page>.md`; v2 draft `.../protocol/v2/overview.md` and `.../protocol/v2/migration.md`; the RFD `.../rfds/streamable-http-websocket-transport.md`.
- Schema release `schema-v1.25.0` and pre-release `schema-v2.0.0-alpha.8` of `github.com/agentclientprotocol/agent-client-protocol`; `github.com/agentclientprotocol/acp-tck`; `github.com/coder/acp-go-sdk` v0.13.5.
- Harness `main`: `docs/architecture.md`, `docs/plugins-and-protocols.md`, `AGENTS.md`, `cmd/harness/AGENTS.md`, `e2e/AGENTS.md`, `internal/gates/structure.go`.
- Spike results: the Node, reactor, browser, and size measurements in sections 5 and 6. The spike code is not in this repository.
- Boxes console source: `meetneptune/boxes` `web/src`.

## 1. Summary and recommendation

Andy's goal: run harness in a browser. Node is a subset of that goal.

Build one ACP adapter, `internal/acp`, on the public Go API. It reads and writes bytes through an `io.Reader` and an `io.Writer`, so it does not know its host. Three hosts use it:

- `harness acp`: ACP v1 over stdio, for editors (Zed, JetBrains, Neovim, Emacs) and for Node programs that spawn it.
- `cmd/harness-wasm` in a dedicated browser Worker: a `GOOS=js` wasm module. The page talks to it in ACP frames through `postMessage`. Tool calls round-trip to the page.
- `cmd/harness-wasm` in Node: the same module, with `globalThis.fs` on the real disk.

A JS package, `sdk/embed`, gives all three one client API with two transports: wasm (Worker or Node) or a native `harness acp` subprocess.

The wasm module is built from the graph that exists today. A measurement (section 6) shows that cutting every `os/exec` subsystem saves 2.35 MB of 20.32 MB raw (11.6%) and 0.34 MB of 3.55 MB brotli (9.5%). That does not justify a public `harness/native` package and five new `Options` seams. The browser spike (section 5) measured the module at 3.58 MB brotli, 0.7 s cold load at 50 Mbps, 16 MB of linear memory after 20 turns, and no long task.

The browser needs six changes, each small: a Worker host and the JS SDK; the custom fetch `RoundTripper` always; a transport error classification fix; a persistent store with a store-wide single-writer lock; a declared tool set that hides `bash` and `process`; and a Playwright CI job. Section 5 designs each one.

One core change comes from ACP `session/new` (section 7.4): per-session MCP servers, passed as a Go-only `SessionOption` to `Create` and `Open`. The spec says "All Agents **MUST** support connecting to MCP servers via stdio" (`session-setup.md`, line 395), and a client cannot opt out.

The work directory stays a property of the runtime. `harness acp` builds one `Runtime` for each `cwd`, each with the project config of that `cwd`. A per-session work directory touches ten readers and one project config (section 4.2) and waits for a consumer. The browser has one project, so it builds one runtime.

Items that still wait for a consumer: the wasip1 reactor, the `Workbench` seam, and a native IndexedDB `Store` (section 15).

Decisions for Andy are in section 12. In brief: ship a browser target (Worker, `sdk/embed`, Playwright job, persistent `DiskStore` on OPFS under a store-wide Web Lock); adopt the ACP adapter; ACP v1 first; no SDK dependency; no core split; one `Runtime` per `cwd` and per-session MCP before the first ACP release; `internal/acp` joins the pure-code unit-test list; the boxes console stays on HTTP.

## 2. The spec to implement against

### 2.1 Versions and sources

| Item | Value | Source |
| --- | --- | --- |
| Stable | v1, schema release `schema-v1.25.0`, published 2026-10-09 | `gh release view -R agentclientprotocol/agent-client-protocol` |
| Draft | v2, pre-release `schema-v2.0.0-alpha.8` | same |
| Schema files | `schema.json` (stable), `schema.unstable.json` (opt-in drafts), `meta.json` (method names) | release assets |
| Pin | `schema.json` of `schema-v1.25.0`, sha256 `4bcd1d2051ff7ed379bd9b234fb9b09c5ab9245478698e9fab805bd9b52a33a6` | release asset digest |
| Prose spec | `protocol/v1/*.md`: overview, initialization, session-setup, prompt-turn, content, tool-calls, agent-plan, session-config-options, slash-commands, extensibility, transports, schema | `llms.txt` |
| v2 status | "The v2 protocol surface as a whole is still labeled draft, so gate **v2 support behind explicit version negotiation and feature flags until it stabilizes.**" | `protocol/v2/migration.md`, line 23 |
| Conformance | `acp-tck` (Python, run with `uv`), Apache-2.0, experimental | its README |

Answer to "is there an ACP spec to implement against": yes. The prose pages are the spec, the JSON Schema is its machine-readable form, and the TCK turns many MUST rules into tests. The prose adds rules that the schema cannot state, for example: "the Agent **MUST** respond to the original `session/prompt` request with the `cancelled` stop reason" (`prompt-turn.md`, line 572).

### 2.2 Best practice for an agent implementer

1. One protocol version per connection. The client sends its latest version in `initialize`; the agent answers the same version, or its own latest (`initialization.md`). The adapter then builds the v1 layer or the v2 layer and never mixes them.
2. A thin layer per version over one shared mapping. The mapping produces version-neutral values (`TextChunk`, `ToolCallUpsert`, `UsageUpdate`, `TurnEnd`). Each layer encodes them.
3. Advertise only what works. The TCK counts a failure of an advertised capability as non-conformance.
4. stdout carries ACP messages only, one per line, with no embedded newline. Logs go to stderr (`transports.md`).
5. Pin the schema by tag and digest. Validate every frame, in both directions, against it in tests.
6. Run the TCK in CI against the real binary.
7. Extensions use `_meta` and method names that start with `_` (`extensibility.md`).

### 2.3 TCK

- `uv run acp-tck [args] -- <agent command>`. It starts a fresh stdio process for each test. v1 is the default; `--protocol-version 2` runs the draft suite.
- Tiers: MANDATORY (a failure or an untested requirement makes the run non-conformant), CAPABILITY (only for advertised capabilities), ADVISORY, INFORMATIONAL.
- It covers `initialize`, session lifecycle, prompt turns, cancellation, modes, config options, content capabilities, auth, extensibility, errors, and stdio hygiene. It does not cover MCP, terminal, or filesystem.
- It is experimental and its coverage is incomplete. CI pins it by commit. A pass is evidence, not proof.

### 2.4 SDK landscape and the recommendation

There is no official Go SDK. The official SDKs are Rust, TypeScript, Python, Kotlin, and Java. The Go community libraries (`libraries/community.md`): `coder/acp-go-sdk`, `ironpark/acp-go`, `eino-contrib/acp`, `spachava753/acp-sdk`, `Tangerg/acp`, `caelis-labs/acp-go-sdk`, `BrokkAi/acp-go`. Several share a name, so the URL identifies them.

`coder/acp-go-sdk`: Apache-2.0, v0.13.5 (2026-06-02), `go 1.21`, a `go.mod` with no `require` lines, files generated by `cmd/generate` from `schema/schema.json` (`types_gen.go`, `agent_gen.go`, `client_gen.go`). It offers an `acp.Agent` interface and `acp.NewAgentSideConnection`. Its page names no v2 schema. I did not read its code.

| Option | For | Against |
| --- | --- | --- |
| A. `coder/acp-go-sdk` | No JSON-RPC code. No transitive dependency. | Version 0.x. No v2. It exports about 190 types for a slice of about 30. Its `Agent` interface is declared by the producer, against rule 3. The module has five direct dependencies today; this adds a sixth that owns the message loop. |
| B. Generator from the pinned schema | Types follow the schema. | The schema uses `oneOf` with `const` tags, `anyOf` with `null`, `allOf` wrappers, and `x-` keywords. A general generator is large for 30 types. |
| C. Hand-written slice plus a schema gate | About 400 lines per version. No dependency. Structs designed from the consumer. | A new schema release needs a manual diff review. |

Recommendation: C. The test client validates every frame against the pinned `schema.json` (section 9). If the slice passes about 60 types, build B then. The framing code is `internal/acp/rpc`, about 150 lines: newline-delimited messages, request IDs, notifications, one writer.

## 3. Layers

```
  editor (Zed, ...)        Node program          browser page
     │ ACP v1/v2, stdio       │ sdk/embed            │ sdk/embed
     ▼                        ▼  (spawn, or wasm)    ▼ postMessage frames
  cmd/harness acp        cmd/harness acp /     Worker: cmd/harness-wasm (GOOS=js)
                         cmd/harness-wasm            │ host capabilities (JS)
     └──────────────┬──────────────────────────────┘
                    ▼
        internal/acp  (rpc · v1 · v2 · mapping)     imports harness, protocol, config
                    ▼
        harness.Runtime       seams: ModelTransport · Store · Owner · Tools · Logger · Getenv
```

| Layer | Owns | Imports |
| --- | --- | --- |
| `harness` | Runtime, Session, Turn, providers, store interface, seams | unchanged |
| `internal/acp` | Mapping from the Go API to ACP, version layers, per-connection state, the default tool set of a host | `harness`, `harness/protocol`, `harness/config`. Never `cmd`, never an internal runtime package. |
| `internal/acp/cwdruntimes` | The `Runtimes` of the adapter: one `Runtime` for each `cwd` on demand, over one shared `Store` and one shared `Owner`. The host injects the config loading and the shared `Store` and `Owner`, so the package reads no file and no variable. | `harness`, `config`, `internal/acp`. Never `cmd`. |
| `cmd/harness` | The `acp` command: the composition root. It wires the real config loading, session directory, and owner into `cwdruntimes` and calls `acp.Serve(ctx, runtimes, in, out, cfg)`. | adds `internal/acp`, `internal/acp/cwdruntimes` |
| `cmd/harness-wasm` | The wasm entry (build tag `js`): it exports `harnessConnect` and wires the adapters | `harness`, `internal/acp`, `internal/jshost` |
| `internal/jshost` | The adapters of a JS host (build tag `js`): `fetchRT`, JS tools, the frame channel, the readers of host options | `harness`, `harness/protocol` |
| `sdk/embed` | The JS package: one client API, two transports (section 5.8). It also owns the store lock (5.5). | the official ACP TypeScript SDK |

`internal/acp` declares the interfaces that it needs in its own package, so a double and the real types both satisfy them (rule 3): `Runtimes` (`For(ctx, cwd) (Runtime, error)`, `ToolImage(ctx, sessionID, key) (io.ReadCloser, error)`) and `Runtime` (`Create`, `Open`, `Read`, `List`, `Models`, `Commands`). `internal/acp/cwdruntimes` implements `Runtimes`, and the e2e in-process host uses it too; `internal/acp` never builds a `Runtime` or loads config. `Runtimes` also has `Close(ctx)`. `ToolImage` returns the bytes of a tool-result image, and only for a key that an `item.completed` record of that session names as an `image/*` blob of a tool result; any other key, including a `backend.state` chunk and a key of another session, fails with `blob_not_found`. The HTTP blobs route stays 404 for tool-result images (`docs/architecture.md`, the blobs route), so the adapter reads them through this method, which a host implements over its own `Store`.

## 4. The capability seam

Every capability that a wasm host supplies is a field of `harness.Options` that exists today.

| Capability | Declared in | Native | Wasm (`GOOS=js`) |
| --- | --- | --- | --- |
| Model transport | `Options.ModelTransport func(provider string) http.RoundTripper` | default `net/http` | `RoundTripper` over `globalThis.fetch`. It is mandatory in the browser and in Node (5.4). |
| Store | `harness.Store` | `store.DiskStore` on the disk | `store.DiskStore` on `globalThis.fs`: OPFS in the browser, the real disk in Node. Weaker durability (5.5). |
| Owner | `Options.Owner` | `NewLocalOwner` | `NewLocalOwner`. A store-wide lock, taken by the JS host before the store mounts, makes one Worker the only writer (5.5). |
| Tools | `Options.Tools []Tool`, `CreateSession.AllowedTools` | built-in tools with `WorkDir` | the built-in file and search tools over `globalThis.fs`, plus JS functions wrapped as `harness.Tool` (5.6) |
| Logger | `Options.Logger *slog.Logger` | JSON `slog` on stderr | an `slog.Handler` that calls a host callback |
| Getenv, config | `Options.Getenv`, `config.Config` literal | `os.Getenv`, files | host closures and a JSON config. Provider keys stay in a same-origin proxy. |

Clock and timers need no Go interface. On `GOOS=js` the JS event loop drives the Go scheduler, so `time` works.

### 4.1 ACP client capabilities

| ACP client capability | Both hosts |
| --- | --- |
| `fs.readTextFile`, `fs.writeTextFile`, `terminal` | Never used. |
| `session.configOptions.boolean`, `session.compaction`, `session.notices` | Read; use when offered. |
| `elicitation` | Use for model questions (phase A8). |

Decision (section 12, item 6): the adapter never calls `fs/*` or `terminal/*`. Reasons:

1. v2 removes them: "The v1 Client capabilities `fs` and `terminal` are removed entirely" (`migration.md`, line 190).
2. They serve editor buffers and editor terminals. A browser host holds a virtual workspace.
3. One rule keeps the adapter identical in both hosts.
4. fx also keeps shell execution out of ACP. Its shell goes through a `workspace.exec` adapter that exists on its terminal surface only; the headless core has no shell (`fx-findings.md` §4).

Native limit: an unsaved editor buffer is invisible to the file tools, which read the disk. Wasm: the built-in file and search tools run on the virtual file system (5.6). A host supplies a shell tool, if it wants one, through `Options.Tools`. A host cannot name a tool `bash` while `WorkDir` is set, because the collision rule applies with a `WorkDir` (`docs/architecture.md`, Built-in tools); it names it `shell` or similar.

### 4.2 The work directory: one Runtime for each cwd

`Options.WorkDir` is read once in `New`, and many readers keep that value. A per-session `CreateSession.WorkDir` must cover each reader. The list (from `runtime.go` at `main`):

| Reader | Today | A per-session work directory needs |
| --- | --- | --- |
| Built-in tools, `read_tool_result`, `session_info` | `tools.go:47`, `tools.go:98` use `r.workDir` | `source` takes the session dir |
| Tool-result retention | `Retain: r.workDir != ""` (`runtime.go:389`) | the session dir |
| `session.created.work_dir` | `create` overwrites it with `r.workDir` (`runtime.go:309-311`) | the request value; the rule changes |
| Prompt chain: AGENTS.md, skills | `prompt.Describe(opts.Config, opts.WorkDir, ...)` (`runtime.go:222`) | `r.prompt` takes a dir |
| Command dirs | `resolveDirs(opts.WorkDir, ..., ".agents/commands")` (`runtime.go:224`) | per-dir command registry |
| Agent profile dirs (`agent_defs_dirs`) | `resolveDirs(opts.WorkDir, ..., ".agents")` (`runtime.go:225`) | per-dir profiles; the `task` tool reads them |
| Plugin host and `shell.env` | `pluginsrc.New(..., opts.WorkDir, ...)` (`runtime.go:252`); the hook sends "the work directory" | one host, many dirs: the hook payload and the plugin `cwd` change |
| `model` tool | exists "with a WorkDir" (`runtime.go:237`) | exists when the session has a dir |
| `Options.Tools` name check | applies "With a WorkDir" (`runtime.go:102`, in `New`) | applies whenever a session can get a dir |
| Claude Code backend | `backend.New(opts.Config, opts.WorkDir, ...)` builds one `claudecode.Backend` for each provider with one `workDir` (`claudecode.go:76,215`) | the `turn.Request` or `Capabilities` carries the dir: a seam change |
| Process manager, `Runtime.WorkspaceChanges`, process routes | `r.procs`, `box.go:33` | stay box-level on `Options.WorkDir` |
| Project config | `config.LoadProject(user, dir)` reads `<dir>/.harness.json`: providers, `mcp_servers`, plugins, processes, `commands_dirs`, `instructions` | one `Runtime` has one `Config`, so one dir's file would apply to every session |

The last row decides the design. With one `Runtime` for many `cwd` values, the project config of a single directory would govern sessions of other projects. That is wrong for an editor with several projects. The honest answers are a per-session config (very large) or one `Runtime` for each `cwd`.

Design for the first ACP release: `cmd/harness acp` keeps a map from `cwd` to `Runtime`. Each runtime is built with `config.LoadProject(user, cwd)` and `Options.WorkDir = cwd`, over one shared `Store` and one shared `Owner` (`NewLocalOwner`). Ownership in the process has two cases. A second `session/load` or `session/resume` of a session that the connection already holds returns the held session: `Runtime.load` returns the running `*Session` before it calls `Owner.Acquire` (`load.go:24-65`), so the adapter keeps one handle and one event pump per session and answers the second request from that handle. An `Open` of the same session through the runtime of another `cwd` fails with `ErrSessionHeld` (`owner.go:44`), because the two runtimes share one `Owner`; the adapter maps it to `session_not_owned`. The `cwd` of `session/load` and `session/resume` selects the runtime; a session whose log has another `work_dir` fails with `-32602`. An editor window runs one project, so one runtime is the common case.

Cost: each extra `cwd` adds one plugin set, one process manager, and one MCP registry set. Command (`docs/architecture.md`) says that `cmd/harness` "builds one `harness.Runtime`"; the A1 spec edit says that `acp` builds one for each `cwd`. No core change is needed, and no client-chosen path reaches a network route.

Deferred: a per-session `CreateSession.WorkDir` with the table above as its design, when a consumer needs one runtime for many projects (section 15).

## 5. Browser and Node targets

### 5.1 `GOOS=js` first

| | `GOOS=js` | wasip1 reactor |
| --- | --- | --- |
| Scheduler | the JS event loop; timers work | the host calls `step()`, `wake(id)`, and a tick; timers fire only inside an export |
| Async host call | goroutine blocks on a channel; a promise callback sends | goroutine parks; the host calls `wake` then `step` |
| JSPI | not used | avoid: it delayed one delta by one chunk (about 900 ms) in the spike |
| Size | 20.3 MB raw | 20.4 MB raw, 3.55 MB brotli |
| Host work | `wasm_exec.js` (about 18 KB, matched to the Go version) | an ABI that we define |

`GOOS=js` needs no ABI and gives `globalThis.fs`, which the file tools and the store use. The reactor is deferred (section 15).

### 5.2 Frames and the Worker

`internal/acp.Serve(ctx, runtimes, r io.Reader, w io.Writer)` is the whole adapter entry. The wasm entry builds two `io.Pipe` ends:

- JS to Go: `connection.send(frame string)` is a `js.FuncOf` callback. It writes `frame + "\n"` into a buffered channel that feeds the pipe, and it never blocks the event loop.
- Go to JS: a goroutine reads lines from the output pipe and calls `onFrame(string)`.

The module exports `harnessConnect({config, tools, builtinTools, onFrame, onLog}) -> {send, close}`. One call opens one ACP connection, so one version per connection holds.

Decision (section 12, item 12): the module runs in a dedicated Worker. The spike measured the main thread with 300 deltas: max gap 27.8 ms, no long task. With the module in a Worker the max gap fell to 11.2 ms. Work that the spike did not measure (replay of a long history, compaction, a large `grep`) grows with the session, and a Worker keeps it off the UI thread. The cost is one `postMessage` hop of 0 to 1 ms per delta. `fetch`, IndexedDB, and Web Locks all work in a Worker. The page sends ACP frames as `{frame}` messages. A tool call goes the other way as `{tool: {tid, name, args}}`, and the page answers `{toolResult}`; the Go goroutine that runs the tool waits on a channel (the spike did exactly this). Tool calls are a host capability, not an ACP method.

### 5.3 What the spike measured (Chrome Headless Shell 156, Apple Silicon, localhost)

| Item | Value |
| --- | --- |
| Module | 20,479,648 B raw; 3,579,937 B brotli q11 |
| Cold load, localhost | 87 to 105 ms boot |
| Cold load, 50 Mbps and 40 ms RTT | 658 to 735 ms boot; 870 to 960 ms wall |
| Cold load, 10 Mbps and 100 ms RTT | about 3.6 s wall |
| Warm HTTP cache | 61 to 68 ms |
| Linear memory | 10.5 MB at start; 16.3 MB after 20 turns of 300 deltas; it plateaus |
| JS heap | 1.9 MB at start; 2.5 to 3.2 MB after turns |
| Main-thread jank | no gap over 50 ms and no long task, with or without a Worker |
| Worker start | 118 ms |

Scripted turns do little CPU work. A real session is not measured. Only Chrome ran. Firefox and Safari did not.

### 5.4 Network

Rule: the host always sets `Options.ModelTransport` to the fetch `RoundTripper`. Reasons:

1. The default js transport cannot cancel a stream. `net/http` aborts only while it waits for the response headers, and the body reader does not watch the request context (`roundtrip_js.go`). In the spike, `Turn.Cancel` returned, the turn still `completed`, and the server sent all 300 frames over 29.6 s. With the custom `RoundTripper`, the turn ended `interrupted` in 4 ms and the connection closed 4 to 6 ms later.
2. Under Node, the default transport is off when `process.argv0` starts with `node`, and when a `Dial*` is set (`roundtrip_js.go:57-69`).
3. The host can inject a proxy or credentials.

The `RoundTripper` keeps its abort tied to `ctx` for the whole body, and each `js.Func` releases itself after it fires (the spike fixed a `call to released function` log when `await` released a `js.Func` while the promise was pending).

Error classification. Today a CORS block shows as `Post "...": <object>`, because `await` formats the rejected JS value with `%v`. `provider.Do` (`internal/provider/request.go:46`) then marks it `RetryableConnectionFailed`. Two retry layers follow: `request_max_retries` inside `Do`, then `stream_max_retries` (default 5) in the turn (`docs/architecture.md`, Turn). So a deterministic CORS failure looks like a flaky network, and the spike logged about six attempts. The fix reuses the existing permanent class, not a second mechanism:

- The Worker owns the fetch that `fetchRT` calls, because a page function cannot be cloned into a Worker. By default `fetchRT` calls the Worker global `fetch` with the `proxy.headers` that `harnessConnect` takes as a serializable value. When the page passes its own `fetch`, `sdk/embed` installs a bridge like the tool bridge: `fetchRT` posts a request frame (method, URL, headers, body) to the page; the page runs its `fetch` and posts the status, the headers, and each body chunk back as a transferred `ArrayBuffer`, then an end or error frame; an abort of the request `ctx` posts an abort frame, and the page aborts its `AbortController`. W3 builds the default path and `proxy.headers`; W4 builds the bridge and `proxy.baseURL`. `proxy.baseURL` is an option of `harnessConnect`, applied in Go where `config` owns the provider keys: it is the `base_url` of the native `anthropic` and `openai` providers and of each `providers` entry that sets none, so all model traffic goes through the proxy.
- `fetchRT` returns a `*FetchError{Method, Origin, Name, Message, Online}`. `Error()` reads `name: message` from the JS error (`TypeError: Failed to fetch`) and adds "the browser blocked the request or the host is unreachable (CORS, DNS, TLS, or offline)". It carries no query, header, or body.
- `internal/provider` declares `interface{ Permanent() bool }` for detection only. In the `err != nil` branch, `Do` returns `MarkPermanent(err)` (`internal/provider/retryable.go:92-123`) at once when an error in the chain of `err` implements it and reports true. Detection uses `errors.As` over the whole chain, because `http.Client.Do` wraps a `RoundTripper` failure in `*url.Error`. That skips both retry layers, and `errorClass` (`internal/session/turns.go:333-343`) maps the result to `protocol.ErrorPermanent`, so the turn ends `permanent`. `FetchError.Permanent()` is `Online`, where `Online` is `navigator.onLine` at the failure. A browser that reports offline keeps the retry. `navigator.onLine` stays inside the js-tagged `FetchError`; nothing branches on a platform name.
- JS cannot tell a CORS block from a network error (both are `TypeError`), so the rule is a policy: while the browser is online, a request that gets no response is not retried. A failure after the response headers (a reset stream) is a different path and keeps `RetryableStreamTruncated`.
- The spec edit is the retry bullet of the Turn section, which lists "a transport error" under `ErrRetryable`: it gains "except a transport error that declares itself permanent". W2 makes it.

Keys. The supported pattern is a host proxy: the page fetches a same-origin URL (`base_url` in the provider config), the proxy injects the key, and `Getenv` returns nothing. This keeps keys out of the page, makes CORS one same-origin hop, and moves forbidden headers to the proxy. Direct provider access exposes the key to every script on the page.

- Direct Anthropic needs `anthropic-dangerous-direct-browser-access: true`. Without it the preflight has no `Access-Control-Allow-Origin` and Chrome blocks the request. With it the request passes: the spike got a clean `authentication_error` (HTTP 401) for a bogus key. The adapter does not send it. Decision (section 12, item 15): add no code. The provider config already has `extra_headers` (`config/provider.go:38`); a demo sets `{"anthropic-dangerous-direct-browser-access": "true"}` explicitly, and the `sdk/embed` documentation marks it demo-only. A Playwright row proves the header reaches the fake server.
- Direct OpenAI is unproven. The preflight answered, but the `POST` responses to `/v1/responses` and `/v1/chat/completions` carry no `Access-Control-Allow-Origin` on the 401 path, and the spike had no real key. Do not plan on it.

Preflight. Every request preflights, because `content-type: application/json` is not safelisted and the Anthropic adapter adds `x-api-key`, `anthropic-version`, and `anthropic-beta`. The result is cached (`max-age` 600): a two-request tool turn made one preflight. The server must list these headers in `Access-Control-Allow-Headers`. Headers that the browser drops silently: `User-Agent`, `Accept-Encoding`, `Host`, `Content-Length`, `Connection`, `Origin`, `Cookie`. Impact: none for the adapters, which set none of them; a custom `User-Agent` cannot work. `Content-Encoding` and custom headers pass, and the preflight lists them. Codex request compression (`http_compression.go`, zstd) works in wasm and needs `content-encoding` in the server's allow list.

### 5.5 Store and the single-writer lock

The browser store is the existing `store.DiskStore` over `globalThis.fs`. `sdk/embed` implements `globalThis.fs` in the Worker itself (`src/opfsfs.mjs`, about 600 lines, no dependency): exactly the callback calls that `syscall/fs_js.go` and `wasm_exec.js` make (`open close read write fsync fstat stat lstat ftruncate truncate mkdir readdir rmdir unlink rename readlink symlink link chmod fchmod chown fchown lchown utimes`, and `writeSync` for fd 1 and 2), `process.cwd()`, and `path.resolve`.

Backing:
- `/persist` is the origin private file system (OPFS, `navigator.storage.getDirectory()`). A file stays open through a `FileSystemSyncAccessHandle`, which only a Worker has: reads and writes take an offset, so an append writes only the new bytes; `truncate`, `getSize`, and `flush()` exist. `fsync` is `flush()`. Descriptors of one path share one handle, because a handle locks its file and `DiskStore` opens a log twice at once (the append handle and the scan). A directory is a `FileSystemDirectoryHandle`.
- `/work`, `/tmp`, and the rest are an in-memory tree in the same module. `/work` is memory because a workspace file is seeded by the host, a reload should start it clean, and a memory write has no cost; a host that wants a persistent workspace is out of scope (5.6).
- Every path resolves against `process.cwd()` (`/work`), so a relative path works. fd 0, 1, and 2 are the standard streams: `fstat` answers a character device, and a write goes to the console. Every callback answers once and never throws; an error carries a code from Go's table (`syscall/tables_js.go`), because an unknown code panics Go. A DOM error maps (`NotFoundError` to `ENOENT`, `TypeMismatchError` to `ENOTDIR`, `InvalidModificationError` to `ENOTEMPTY`, `NoModificationAllowedError` to `EBUSY`, `QuotaExceededError` to `ENOSPC`, `NotAllowedError` to `EACCES`) and anything else is `EIO`. A directory opened for write is `EISDIR`. `chmod`, `chown`, and `utimes` check that the path exists and change nothing in OPFS; `symlink` and `link` are `ENOSYS`.
- `rename` is the `move()` of the file handle. Chromium replaces an existing destination and refuses a file that has an open handle. A source that is open is `EBUSY`; a destination that is open gets a copy into its handle, because `GetBlob` can hold a blob that `PutBlob` replaces. `unlink` of an open file is `EBUSY`. A directory cannot be renamed (`ENOSYS`); `DiskStore` renames files only. A browser without `move()` gets a copy and a remove. The spec states that the browser, not harness, makes the rename atomic against a crash.

The failure to guard against: two tabs that write one session corrupt it, because `lockFile` is a no-op on js (`store/lock_other.go`). The sync access handle already refuses a second handle on one file, but two tabs would still interleave the directory listing that `Create` changes and the writes of different files. Design: one store-wide Web Lock, taken by the JS host before any file of the store opens.

1. The Worker bootstrap of `sdk/embed` runs this order: request `navigator.locks` `harness:store:<origin>` with `mode: "exclusive"` and an `AbortSignal.timeout` of 2 s; if granted, open OPFS; then install `globalThis.fs`, `process`, and `path`; then load `wasm_exec.js` and start the module. The lock callback returns a promise that stays pending until `close()` or the end of the Worker. Harness never passes `steal: true`.
2. If the lock is not granted in 2 s (the wait covers a reload, because the old Worker can still hold the lock when the new page boots), boot fails with `store_in_use`, and `sdk/embed` offers `store: {persist: false}` (a memory store) to the app. One tab writes at a time for each origin. A second tab does not read.
3. The `Owner` stays `NewLocalOwner`: inside one Worker the in-process `Owner` already grants a session once, and the store lock extends that guarantee to the origin.
4. Durability is what `flush()` gives: a Worker that ends after `fsync` returned loses none of the bytes written before it, and a Worker that ends during a write leaves a prefix of the bytes written since the last `fsync`, which `Open` reads as a torn tail. A row pins both: a Worker that ends after an acknowledged turn has every record, and a Worker that ends after an `fsync` and a later unsynced write leaves the synced bytes and a prefix of the rest. `flush()` does not promise that the bytes survive a power loss.
5. A rename of a blob is as atomic as the browser makes `move()`, and the standard does not say how atomic that is. A record can name a blob that is missing, which a later read reports as `ErrBlobNotFound`. The spec's Store section says so for the browser host.

Rows: two tabs create different sessions; the second tab gets `store_in_use` and the first loses no record; a tool-result blob survives a reload; an acknowledged append survives a Worker kill; the first tab closes and the second boots with every record; a Go probe checks each call of Go's `syscall` layer on the adapter.

Node. `lockFile` is also a no-op on js on the real disk. A Node wasm process that shares a session directory with native `harness` or with another Node process has no fence. The Node store directory is therefore exclusive: `sdk/embed` creates `<dir>/.embed.lock` by a hard link with the pid and a token, refuses to start when the pid is alive, and removes the file at `close()`. Processes that find one stale lock race for one claim file named by the token of the stale lock; the winner replaces the lock by an atomic rename after it checks the lock again, and a claim of a dead process is removed. It enforces this among SDK processes only. Sharing a directory with native `harness` is unsupported for wasm; the subprocess transport (5.7) is the way to share it.

A native IndexedDB `Store` (events as records) waits (section 15).

### 5.6 The tool set

The built-in `read_file`, `write_file`, `edit_file`, `ls`, `glob`, `grep`, and the `AGENTS.md` discovery work on an in-memory `WorkDir` (`/work`), including the `write_file` read guard. `bash`, `process start`, and `WorkspaceChanges` (git) fail with `exec: ... executable file not found`. Today the model is still offered `bash` and `process`, so it calls tools that cannot work.

Where `/work` lives. In the spike `/work` was an in-memory backend and only `/persist` was persistent (`src/core.js:10`). So workspace files are lost on reload, while the session log keeps `work_dir`. The design keeps that default: `/work` is memory, and the host seeds it from the page. A host that wants a persistent workspace mounts `/work` on IndexedDB; it then sits under the same store lock (5.5), so two tabs cannot edit it. A workspace on a user's disk (the File System Access API) is out of scope.

The mechanism to hide tools already exists: `CreateSession.AllowedTools` holds tool names in one namespace, with the runtime tools, the plugin tools, the MCP tools, and the built-in tools of the backend (`docs/architecture.md`, `AllowedTools`); `nil` allows all, and a list narrows the set the model sees and may call. So no new `Options` field is needed. The consumer that needs a default is the ACP adapter: `session/new` has no field for it.

Design: `internal/acp.Config` (declared by the adapter, rule 3) holds `AllowedTools []string`. The adapter passes it as `CreateSession.AllowedTools` on every `session/new`. `harnessConnect({builtinTools})` fills it. `sdk/embed` defaults it, in the browser and in the wasm host under Node, to `read_file`, `write_file`, `edit_file`, `ls`, `glob`, `grep`, `read_tool_result`, `session_info`, and the names of the page's JS tools. Native `harness acp` leaves it nil. The golden of the JS tool turn comes from the Node host, because native hosts have no JS tools.

Why the default leaves out `task`, `model`, and `goal`:
- `task` starts child sessions that run unattended. A background tab throttles timers, and every child adds writes under the single store lock. A child gets "the allowed tools of the parent narrowed by the profile" (`docs/architecture.md`, Spawn), so a host that lists `task` still does not expose `bash` or `process` to children.
- `model` lets the model switch to a provider that the host's proxy does not serve. The host changes models through the ACP config option.
- `goal` starts autonomous turns with a second model call for the evaluator, in a tab that the user can close.
A host lists any of them in `builtinTools` to enable it.

The value is recorded in the session, so a reload keeps it. JS tool names are fixed at `Create`: a later page that adds a tool does not reach older sessions. A scenario asserts that the first model request of a browser session lists no `bash` or `process`.

A host that wants a shell supplies it as a page tool over the same files. `h.fs` gives the page the file system of the module (the file tools and `bash` of a shell must see one set of files, so the module owns the state and the shell is a consumer). `@majorcontext/harness-embed/shell` builds the tool on just-bash, an optional peer dependency: just-bash is a JavaScript interpreter, so it needs no process, and it reaches the module files through `h.fs` only, at `/work` and `/tmp`. The network commands stay off, because in a page `curl` would run with the origin and the cookies of the page. The shell runs on the page thread and stops at a time limit and a command limit. `h.fs` exists in a browser only: under Node the module runs on the disk of the host, where a shell over `fs` would reach every file of the user. A page that needs a shell there uses the `subprocess` transport.

### 5.7 Node as a subset

Under Node, `wasm_exec.js` maps `globalThis.fs` to the real `fs`. The file tools and `DiskStore` therefore work on the real disk with no shim. The store directory is exclusive to one process (5.5); the `Owner` is `NewLocalOwner`. Network still needs the injected `RoundTripper`, because the default transport is off (`roundtrip_js.go:57`); there is no CORS. There is no `exec`, but a host may supply a shell as a JS tool under a name other than `bash`.

Full-feature Node: `sdk/embed` spawns native `harness acp` and speaks the same ACP over its stdio. That gives `bash`, processes, git, stdio MCP, plugins, and the Claude Code backend. One client API, two transports: wasm, or a native subprocess.

### 5.8 The JS SDK (`sdk/embed`)

A new package, separate from `sdk/typescript`. `sdk/typescript` is the zero-dependency Node SDK for writing harness plugins (`sdk/typescript/README.md`), and `sdk/AGENTS.md` binds it to the NDJSON plugin protocol: zero dependencies, Node built-ins only, Node 18, and parity with the Go plugin host. Each of those rules conflicts with `sdk/embed`, which depends on the ACP TypeScript SDK, ships a wasm asset and a Worker script, and speaks ACP. The `sdk/embed` PR (W4) scopes the rules of `sdk/AGENTS.md` to `sdk/typescript` and adds `sdk/embed/AGENTS.md`. It also corrects the sentence of the spec that calls `sdk/typescript` "the TypeScript client of the HTTP contract" (Public packages, below the table).

```ts
const h = await createHarness({
  transport: wasm({ module, worker: true }),          // or subprocess({ command: "harness", args: ["acp"] })
  config,                                             // config.Config JSON; no keys
  proxy,                                              // browser: { baseURL, headers } of a same-origin proxy; serializable
  fetch,                                              // optional: a page fetch, bridged to the Worker by postMessage
  tools,                                              // [{name, description, schema, run(input, {signal})}]
  builtinTools,                                       // default: the list in 5.6
  store: { persist: true, dir: "/persist/sessions" }, // browser: OPFS under the store lock; Node: an exclusive real directory
  log,
});
h.stream                                              // { readable, writable } of ACP messages
h.close()
```

A transport is an object `{kind, supports, defaultTools, start}`. `supports` names the options (`tools`, `builtinTools`, `proxy`, `fetch`) that the transport honors, and `createHarness` rejects any other option that the caller sets. `defaultTools` is the default tool set before the names of the page tools, or absent when the transport leaves the choice to the host. `start` returns the `Harness`. `createHarness` never branches on `kind`.

`h.stream` is a `{readable, writable}` pair of parsed ACP messages, compatible with the `Stream` type of the ACP TypeScript SDK, and `sdk/embed` has no dependency on that SDK. The pair has no backpressure in either direction. The app connects `h.stream` to the official ACP TypeScript client (`libraries/typescript.md`). The SDK has no UI and no ACP client of its own. A web UI is bailey's surface.

## 6. The core split: measured, and not part of this plan

### 6.1 Measurement

Method: a spike build cuts every `os/exec` subsystem out of the root import graph behind a build tag, and builds `GOOS=js` with and without the tag from the same tree. The sizes are bytes of the module.

| Build | raw | gzip -9 | brotli -q 11 |
| --- | --- | --- | --- |
| current | 20,316,245 | 4,979,763 | 3,546,914 |
| cut | 17,964,460 | 4,495,208 | 3,210,528 |
| gain | 2,351,785 (11.6%) | 484,555 (9.7%) | 336,386 (9.5%) |
| current, `-s -w` | 19,863,929 | 4,886,871 | 3,483,508 |
| cut, `-s -w` | 17,584,738 | 4,413,636 | 3,162,638 |
| gain | 2,279,191 (11.5%) | 473,235 (9.7%) | 320,870 (9.2%) |

Reading: the earlier estimate of "about 8 MB" was wrong by a factor of 3.4. The cut is an upper bound for a real split, because a real design keeps the in-process plugin host (`Options.Plugins` runs the host over a pipe), which the crude cut removed. The 17.6 to 18.0 MB that remains is the runtime, providers, TLS, and `net/http`, which no split changes.

### 6.2 Consequence

- The wasm build ships on today's graph. `os/exec` code links and returns errors at run time on js.
- No `harness/native` package, no `Workbench`, `Processes`, `MCPDialer`, `PluginLauncher`, or `BackendBuilder` seam is added. Rule 4: no seam without a consumer that needs it.
- A native embedder that wants a smaller binary, or a wasm consumer with a page-weight budget, can reopen the split. The import edges that such a split must cut (from `go list -deps .` and `go list -f '{{.ImportPath}} {{.Imports}}'` at `main`) are:

| Importer | Imports | Cut |
| --- | --- | --- |
| `harness` (root) | `internal/process` (`box.go`, `commands.go`, `runtime.go`), `internal/workspace` (`box.go`), `internal/tool/builtin` (`tools.go`), `internal/plugin` (`tools.go`, `ValidateHookTimeouts`), `internal/tool/mcpsrc`, `internal/tool/pluginsrc`, `internal/tool/proc` | consumer-declared interfaces in `harness`; constructors passed in by `cmd/harness` |
| `internal/session` | `internal/plugin` for `SanitizeSessionError` (`goal.go:14,373`, `turns.go:15,321`) | move the function to a package with no process code |
| `internal/selftool` | `*process.Manager` (`sessioninfo.go:49,69`); `internal/tool/proc` | a `Processes` interface owned by `selftool` |
| `internal/backend` | `internal/backend/claudecode` (`router.go:15,70`), which imports `internal/backend/external`, which imports `internal/mcp` | a builder registry keyed by `config.Provider.Type` (`config/provider.go:29`, `router.go:69`) |
| `internal/tool/mcpsrc` | `internal/mcp`, which holds the stdio transport (`stdio.go`, `os/exec`) and the HTTP transport in one package | split stdio into its own package |
| `internal/tool/pluginsrc` | `internal/plugin` (`host.go`, `resolve.go`, `os/exec`) | a launcher interface for process plugins |

The seven packages that import `os/exec` are `internal/mcp`, `internal/backend/external`, `internal/backend/claudecode`, `internal/plugin`, `internal/process`, `internal/tool/builtin`, and `internal/workspace`. The proof of any later split is a `go list -deps` check in `internal/gates` (Go tooling): it fails when `os/exec` appears in the deps of `.` or of `internal/acp`. A `GOOS=js` build proves nothing, because `os/exec` compiles on js. A ban on `syscall` cannot apply to the whole graph, because `os` and `net` import it.

### 6.3 What the wasm build does need

- One build tag for the Codex websocket dial. On `main` the call is `internal/provider/openai/ws.go:78`; the spike moved it to `ws_dial.go` (`!js`) and `ws_dial_js.go` (`js`). A browser WebSocket cannot set request headers, so that transport cannot exist there.
- `harness.New` with an empty `WorkDir` starts no process and reads no file (`spike-findings.md` §1). With an in-memory `WorkDir` it also works (5.6).
- The items of section 5: `fetchRT`, the error classification, the store lock and the file system of the Worker, and the default tool set.

## 7. Mapping: ACP v1 to the Go API

Status words: **maps**, **gap** (the Go API lacks it), **non-goal** (the spec excludes it), **ignore** (the adapter does not use it).

### 7.1 Methods

| ACP | Harness | Status | Notes |
| --- | --- | --- | --- |
| `initialize` | none (answered before a `Runtime` is built) | maps | `agentInfo{name:"harness", title, version}`. `authMethods: []` unless the host offers a login (decision 21). `promptCapabilities{image:true, embeddedContext:true}` on every host, because the runtime admits attachments on every host. `mcpCapabilities{http:true, sse:false}` once C1 lands, on the native host only (7.4). `sessionCapabilities` by phase. |
| `authenticate`, `logout` | none | ignore | Offered only by the native host, for a login that the user config uses (decision 21). Without one, a call answers `-32601`. Other credentials come from the environment that the editor passes and from config (`Options.Getenv = os.Getenv`). |
| `session/new {cwd, mcpServers, additionalDirectories}` | `Runtimes.For(cwd)`, then `Runtime.Create(req, WithSessionMCP(servers))` | maps after C1 | 7.4. `additionalDirectories` is not advertised. |
| `session/load` | `Runtime.Read` for replay, then `Runtime.Open` | maps | 7.5. |
| `session/resume` | `Runtime.Open` | maps | No replay. |
| `session/list {cwd, cursor}` | `Runtime.List(ListSessions{After, Limit})` | maps | The adapter drops sessions with a `ParentID` (`internal/listing/listing.go:63-119` lists children) and compares `Session.WorkDir` for the `cwd` filter. `cursor` wraps `Next`. `title` is null. Order is creation order. |
| `session/close` | `Session.Interrupt`, then `Session.Release` | maps | `Release` suspends the turn, waits for Sync, and releases ownership (`Session.Release`, `session.go:206`). The log stays. |
| `session/delete` | none (`Runtime.End` keeps the log) | gap | Not advertised. |
| `session/prompt` | `Session.Prompt`, `Turn.Wait` | maps | 7.2, 7.3. |
| `session/cancel` | `Turn.Cancel` | maps | 7.3. |
| `session/set_config_option` | `Session.Update(SettingsPatch)` | maps | 7.7. |
| `session/set_mode` | none | non-goal | Plan mode is a settled non-goal. No `modes` in responses. |
| `$/cancel_request` | cancel the `ctx` of the named request | maps | Answer `-32800`. |
| `session/request_permission` | none | non-goal | Never sent (`AGENTS.md`, `docs/plugins-and-protocols.md`). |
| `fs/*`, `terminal/*` | none | ignore | 4.1. |
| `elicitation/create` | `RequestOpened` (kind `question`), `Session.Resolve` | maps (A8) | Only when the client advertises `elicitation.form`. |

### 7.2 Prompt content to `protocol.Input`

The v1 `PromptRequest` is `{sessionId, prompt, _meta}` and `PromptResponse` is `{stopReason, _meta}` (`schema.json`). Stable v1 has no prompt `messageId` and no `userMessageId`. The adapter mints `Input.ID` (a ULID) for each prompt. A repeat after a reconnect cannot be detected, because the client sends no ID.

| ACP block | `protocol.Part` | Notes |
| --- | --- | --- |
| `text` | `{type:"text"}` | A prompt that is exactly one `text` block whose text starts with `/` is sent with `Source = protocol.SourceTyped`. `Session.Prompt` then runs the slash-command resolution, which needs a non-empty `ID` and exactly one text part (`session.go:99`). |
| text plus other blocks | all blocks as parts, `Source` unset | `slash-commands.md` allows "any other user message content types (images, audio, etc.) in the same prompt array". Harness runs a command only for one text part, so the line goes to the model as text. This is a limit; a spec change could lift it. |
| `image` | `{type:"blob"}` | png, jpeg, gif, webp, each at most 20 MiB. The adapter decodes the base64 and leaves the media type, size, and content checks to the runtime; a runtime `invalid_request` is `-32602`. Input admission, not the adapter, refuses an image for a session whose model the models.dev facts list without image input: `invalid_request`, which the adapter maps to `-32602` with a message that names the model. A model that the facts lack passes. |
| `resource_link` | `{type:"text"}` with the name and URI | Baseline content that every agent MUST accept. The adapter does not fetch it. |
| `resource` (text) | `{type:"text"}`: a `<resource uri="…">` line, the text, and a `</resource>` line | Needs `embeddedContext`. The header line lets the model tell a file or a selection from the words of the user. |
| `resource` (blob) | `{type:"blob"}` for png, jpeg, gif, webp, pdf | A PDF travels only as an embedded resource, because `promptCapabilities` has no PDF flag. |
| `audio` | none | Not advertised. |
| delivery | `Input.Delivery = "queue"` | A second prompt during a turn waits for the next turn and gets its own `Turn`. `steer` has no ACP form. |

### 7.3 Turn end and cancel

| Outcome | ACP v1 | Notes |
| --- | --- | --- |
| `completed` | `end_turn` | |
| `interrupted` | `cancelled` | After `session/cancel` the agent MUST answer `cancelled` and "**MUST** catch these errors and return the semantically meaningful `cancelled` stop reason" (`prompt-turn.md`, line 579). The adapter sends every pending update first. |
| `failed` (this includes a turn that the cause `provider_exhausted` ends, `protocol/events.go:51`) | JSON-RPC error `-32603`, `data{code, error_class}` | v1 has no `error` stop reason. v2 adds it. The text is the masked `Error`. Classes: `permanent`, `timed_out`, `rate_limited`, `unrecovered`, and provider class names (`protocol/events.go`). |
| `awaiting_input` | `end_turn`, plus an elicitation when allowed | A8. |
| handoff | `Turn.Wait` fails with `ErrSessionReleased` | `suspended` is an event kind, not a `StopReason` (`protocol/events.go:41-46` lists four). The prompt request fails with `-32603`, `data.code = session_not_owned`. Handoff happens only on `Release` or `Close`, which the adapter starts. |
| no mapping | `max_tokens`, `max_turn_requests`, `refusal` | Harness records no matching stop reason. Not emitted. |

`Turn.Cancel` withdraws a queued input or interrupts a running turn. Tool calls that the interrupt cut short have an error result in the log, and the v1 layer sends them as `failed`. The v1 `ToolCallStatus` has `pending`, `in_progress`, `completed`, and `failed`; `cancelled` exists only in v2.

### 7.4 Session setup

- `cwd` selects the `Runtime` (4.2). It must be absolute.
- `mcpServers` goes to a Go-only option: `harness.WithSessionMCP(map[string]config.MCPServerSpec)`, keyed by server name,, a `SessionOption` that `Create` and `Open` accept as a trailing variadic argument. It adds no field to `protocol.CreateSession`, so it is not in `openapi.json` or `protocol.ts`, and no HTTP client can send it. The Go caller is trusted, as it is for `Options.Tools` and `Options.WorkDir`. That one fact replaces any option gate. The field covers stdio (`name`, `command`, `args`, `env`) and HTTP (`type:"http"`, `name`, `url`, `headers`) servers alike; both are client-chosen, and an HTTP URL can name an internal endpoint.
- Secrets. Stdio `env` and HTTP `headers` can hold secrets. The servers never enter `session.created`, the log, or a log line; a log line names a server and nothing else. Because they are not persisted, an `Open` by another process (`serve`, `run -r`) has no session servers, and a resumed turn that calls one of their tools gets `no such tool available`. `session/load` and `session/resume` carry `mcpServers` again, so the editor restores them.
- Lifecycle. A session registry is built at `Create` or `Open` and connects lazily at the first model call, like the runtime registry (`docs/architecture.md`, MCP tools): a failed first connect retries in the background and parks after the bounded attempts, and the `[mcp: unavailable ...]` block names it for the model. `Release`, `End`, and `Runtime.Close` stop every stdio child of the session, and `Close` waits for them. The adapter also sends an ACP `notice` (with `session.notices`) when a session server parks. With an `OwnsMCP` backend, the session servers go to the external harness together with the runtime servers; C1 verifies this path.
- `session/load` and `session/resume`: the `cwd` selects the runtime; a session whose log names another `work_dir` fails with `-32602`.
- Trust. The ACP peer has operator trust: it can run any command through `mcpServers`, and any tool that the runtime grants. `harness acp` over stdio inherits the trust of the process that started it. A remote transport (a tunnel, or the RFD transport) needs its own transport-level authentication before it ships; `harness acp` adds none.
- Wasm. `os/exec` fails at run time on js, so a wasm host cannot meet the stdio MUST. HTTP MCP does not reach the fetch transport either: `internal/mcp/http.go:44` holds its own `*http.Client`, and `Options.ModelTransport` reaches only `backend.New` (`runtime.go:253`). Under Node wasm the default transport is off (`roundtrip_js.go:57`), so MCP HTTP fails there; in a browser it works but cannot cancel (5.4, reason 1). The wasm host therefore advertises no `mcpCapabilities` (`http:false`, `sse:false`), and answers `session/new` with any `mcpServers` entry by `-32602` and `data.reason = "mcp unavailable on this host"`. It declares this nonconformance with the stdio MUST. The alternative, a transport seam for MCP HTTP, is a spec item without a consumer today; a browser app that needs MCP supplies it as JS tools (the fx pattern, `fx-findings.md` §4). `Config.MCPServers` in a wasm config is likewise unsupported. The TCK cannot reach this case, because it does not cover MCP (2.3).

### 7.5 Replay (`session/load`)

`Runtime.Read` serves the replay without ownership. `harness.Reader.Messages(ctx, 0, limit)` returns the newest page first (`Session.Messages`, `session.go:210`); the adapter pages backward with `before` and emits the pages in reverse, so messages arrive oldest first. For each message:

- user: `user_message_chunk` (text, images);
- assistant `text`: `agent_message_chunk`; `reasoning`: `agent_thought_chunk`;
- assistant `tool_call` part: `tool_call`; the following tool message: `tool_call_update` with `completed` or `failed`;
- `engine_context` and `task_report` parts: not replayed.

Replay reads one `View` (`Runtime.Read`). A `View` is a snapshot, so its `Snapshot().HeadSeq` is the replay boundary B: every message of the replay is in the log at or before B. The actual order is: read the view, replay, send the commands update, call `Runtime.Open`, start the pump at B, then answer `{configOptions}`. The adapter opens before it answers, so a failed `Open` (`session_not_owned`) is an error answer and not a half-loaded session, and the TCK (ACP-LOAD-002) allows no update after the answer. Work that `Open` resumes streams as live updates through the event pump (7.6), before or after the answer. The adapter pages the view ascending (`before = next + page`) and skips the overlap of the last page by `FirstSeq`; this gives the same messages as paging backward and reversing, with bounded memory. Replay seeds the `tool_call` ids and diffs that the event pump keeps for calls that await a result (A3), so that a `tool_call_update` after a load keeps the diff of its call. `session/resume` pages the whole view to seed the diffs and sends no history. The requests that load, resume, or close one session run in wire order, one after the other: a ticket taken in `Admit` and released when the answer is sent. A tool result whose text is `(no output)` replays empty; the spec states this lossy case. A session that has a parent (a task child) is `-32602` for both, because `list` hides children and a `close` would release a child that a task uses. A load of a session the connection holds waits until the pump has sent every update through the head of a new view, replays that view with a state of its own, and answers; a resume of it sends nothing; `mcpServers` in either is `-32602`, because the held session keeps its servers. A `session/list` with empty params returns the first page, through a held runtime or the process work directory; a host with neither advertises no `list` capability (wasm without the `defaultDir` option). The Worker of `sdk/embed` sets `defaultDir` to the work directory of its file system, `process.cwd()` under Node and `/work` in a browser, so the wasm host advertises `list` over a memory store and over the OPFS store. One list call reads at most 4 steps of 50 and then answers a `nextCursor`. A client that wants no resumed work uses `session/resume` only after it knows the state.

### 7.6 Updates

One event pump per held session and connection reads `Session.Events(ctx, after)`. After `session/new`, `after` is the head of the new session. After `session/load`, `after` is the replay boundary B of 7.5, not the head after `Open`, so a record appended between the replay and the ownership hand-off reaches the client as a live update, and no record arrives twice. It is the only reader. A goal can start turns with no `session/prompt` open; the pump sends their updates as notifications and no response (v1 does not forbid it; v2 states "Background updates may continue while idle.", `migration.md`, line 787). A prompt handler waits on `Turn.Wait`, then asks the pump to flush through the `turn.ended` seq, then sends the response. That keeps ACP's rule that updates precede the response.

| `sessionUpdate` | Source | Status | Notes |
| --- | --- | --- | --- |
| `agent_message_chunk` | `item.delta` text frames; `item.completed` for the uncovered rest | maps | Frames are ephemeral and a full subscriber drops them (`docs/architecture.md`, Events). The pump tracks emitted length per `item_id` and sends the tail from `item.completed`. `messageId = msg_<item_id>`. `item.discarded` has no v1 form; text of a failed attempt stays in the client. The v2 whole-message upsert can replace it. |
| `agent_thought_chunk` | `item.delta` reasoning frames | maps | |
| `tool_call` | `item.completed` assistant message with `tool_call` parts | maps | No frame streams tool arguments. `status: "in_progress"`. |
| `tool_call_update` | `item.completed` tool message | maps | `completed` or `failed` (`is_error`); text and image content; `rawOutput` is the result text. |
| `plan` | none | non-goal | No plan structure exists. |
| `available_commands_update` | `Runtime.Commands` | maps | Commands that `serve_support` marks supported, plus prompt commands. `input.hint` from `ArgHint`. |
| `current_mode_update` | none | non-goal | |
| `config_option_update` | `settings.changed` event | maps | After `/model` or the model tool. |
| `session_info_update` | `turn.ended` | maps | `updatedAt`. `title`: gap. |
| `usage_update{used,size,cost}` | `Snapshot().Context{Tokens, Window}` | maps | Emit on `context.measured` and `turn.ended`. `cost{amount, currency:"USD"}` comes from `SubscriptionUsage.SessionCostUSD` (`protocol/protocol.go:98`) when set, which only the Claude Code lane reports. Otherwise omitted. |
| `compaction_update`, `compaction_summary_chunk` | `status` frames `compacting`, `compaction_failed`, and `compaction_canceled`, `compaction.applied`, and the summary message of the replay (`cmpsum_` ID) | maps | Only when the client advertises `session.compaction`. Without it, no compaction message at all (the replay skips the summary), and a compaction shows only as a fall of `usage_update.used`. |
| `notice` | adapter diagnostics, lost ownership | maps | Only with `session.notices`. |

Tool `kind` comes from a table in `internal/acp` keyed by tool name: `read_file` read; `write_file`, `edit_file` edit; `glob`, `grep`, `ls` search; `bash`, `process` execute; `task`, `goal`, `model`, MCP tools, plugin tools other; the Claude Code names in the same table; unknown names other. A tool name is not a provider name (rule 3).

`edit_file` takes `path`, `old_string`, `new_string`, and `replace_all` (`internal/tool/builtin/files.go:202-216`). The v1 layer sends `content:[{type:"diff", path, oldText, newText}]` and `locations:[{path}]`. A `replace_all` edit sends the snippet pair once. `write_file` sends `oldText: null`.

Children: a `task` child shows as the parent's tool call. The child text does not stream, and `session/list` hides children. A later change can add `_meta["harness/child_session"]` from `child.spawned`.

### 7.7 Config options

| Option | `category` | Backed by |
| --- | --- | --- |
| `model` | `model` | `Runtime.Models()` as `provider/model`; `Session.Update(SettingsPatch{Model})` |
| `effort` | `thought_level` | the fixed values `default`, `off`, `minimal`, `low`, `medium`, `high`, for every model; `SettingsPatch{Effort}` |

The `service_tier` option is not built. A model with no curated list has one value, its current model.

`Runtime.Models()` needs no session. The adapter lists values and never branches on a provider.

### 7.8 Harness features without an ACP form

Goals, steer, the queue, `compact`, processes, and the workspace diff have no ACP method. Typed slash commands give the user these in any ACP client: `/goal`, `/compact`, `/model`, `/thinking`, `/status`. The command status returns as an `agent_message_chunk`. `_meta` keys and `_harness/` methods are added only for a named consumer.

### 7.9 Errors

| Harness code | JSON-RPC |
| --- | --- |
| `invalid_request`, `input_conflict`, `request_not_pending` | `-32602`, `data{code}` |
| `session_not_found`, `blob_not_found` | `-32002` |
| `session_not_owned` (including `ErrSessionHeld`, `ErrOwnershipLost`), `session_busy`, `turn_mismatch`, `session_exists`, `draining` | `-32603`, `data{code}` |
| `model_unavailable` | `-32603`, `data{code}` |
| `session_unreplayable`, `internal` | `-32603`, masked text |
| unknown method | `-32601` |
| bad JSON | `-32700`; the connection stays open |

`model_unavailable` does not use `-32000` ("Authentication required"): `authMethods` is empty, and a client could show an empty login dialog. One function owns this table from `protocol.CodeOf(err)`, and the Errors gate gets a row check for it.

### 7.10 The v2 delta

| v1 | v2 (`migration.md`) | Adapter change |
| --- | --- | --- |
| `authenticate`, `logout` | `auth/login`, `auth/logout` | none |
| `session/load` | `session/resume` with `replayFrom: {type: "start"}` (already in the draft) | same replay code |
| `session/list`, `close` optional | baseline | already built |
| response carries `stopReason` | response carries `messageId`; `state_update` carries `stopReason` and `error` | the layer answers at insertion; `failed` becomes `stopReason: "error"` |
| `tool_call` and `tool_call_update` | upsert `tool_call_update`; status `cancelled` | encode only |
| chunks | chunks and whole-message upserts, `messageId` required | use `msg_<item_id>`; upserts fix discarded attempts |
| `diff{path,oldText,newText}` | `changes[]`, optional git patch | read the file before the tool runs, or omit the diff |
| `plan`, modes | `plan_update`; modes removed | none |
| `fs`, `terminal` capabilities | removed | none |
| `mcpServers` without `type` for stdio; `sse` | `type` required; `sse` removed | encode only |
| stdio single messages | stdio accepts batch arrays | the v2 `rpc` reads arrays |

## 8. Processes, concurrency, startup

- Each editor window starts its own `harness acp`. All processes share `~/.harness/sessions` (`DiskStore`), and `harness serve` can share it. `Open` appends `owner.acquired`; the later `Open` keeps the session, and the earlier owner stops with `ErrOwnershipLost` (`runtime.go:74-79`, Ownership in the spec). Two windows on one session take it from each other on each `session/load`. When `Session.Done` closes with that cause, the adapter sends a `notice` (if the client advertises it), fails the open prompt with `-32603` and `data.code = session_not_owned`, and fails later prompts the same way. The client calls `session/load` again to take the session back. Inside one process, all runtimes share one `Owner`, so a second `Open` fails with `ErrSessionHeld` (`owner.go:44`). `session/list` lists every stored root session, held or not. The adapter does not call `CatchUp`, because it sets no `Sync`. The rule differs by host: native, the later `Open` wins; browser, the first holder of the store lock wins and a second tab gets `store_in_use` (5.5); Node wasm, the exclusive directory admits one process (5.5).
- One reader goroutine per connection decodes lines and starts one goroutine per request. `session/cancel` is a notification and reaches the session while a prompt runs.
- One writer goroutine per connection owns the output. Update order per session is the order of the pump, which reads the log order.
- Per-connection state (session map, per-item lengths, pending requests) belongs to the pump and the handler goroutines through channels. Session state stays in the session actor (rule 2).
- End of input: `Release` each session, then `Runtime.Close` with a deadline.
- stdout carries ACP only; the `slog` JSON logger writes to stderr.
- Startup: `harness acp` reads no config and makes no network call before it answers `initialize`. It builds the runtime at the first `session/new`. The startup budget step (`.github/workflows/ci.yml`, 50 ms best-of-5) gains a second measurement: time to the `initialize` response. No `init()` side effects. No cgo.

## 9. Testing

### 9.1 What the existing harness allows

- The `driver` interface (`e2e/driver_test.go:17-47`) is HTTP-shaped: `CommandRecords`, `SetGoal`, `Journals`, `Repeat`, `SteerOtherTurn`, and `callResult` goldens with HTTP status and body. ACP has no counterpart for most of them.
- `rowKind` (`e2e/runtime_host_test.go:79-92`) has `rowSame`, `rowRegolden`, `rowDeleted`, `rowPending`. `rowDeleted` means "the spec deletes what the row pins". None means "not applicable to ACP".
- Over ACP the only transcript reader is `session/load` replay, which tests the adapter, not the log.

So ACP does not join the driver table. It gets its own scenario table.

### 9.2 The ACP scenario table

Files: `e2e/acp_scenarios_test.go`, `e2e/acp_client_test.go`, `e2e/acp_schema_test.go`, goldens in `e2e/testdata/acp/`.

- `acpScenario{name, config, model []harnesstest.Step, steps []acpStep}`. Steps: `acpInit`, `acpNew{as, cwd, mcp}`, `acpPrompt{as, blocks}`, `acpCancel{as}`, `acpLoad{as}`, `acpList`, `acpExpectError`. The model is the existing `harnesstest` scripted server.
- Host: the real `harness acp` binary over stdio in its own process group (`e2e/AGENTS.md`: start a subprocess only when the process boundary is under test; the boundary here is stdio). A second host runs `acp.Serve` over `io.Pipe` in process, for `-race` and speed.
- Golden: `{requests, sessions, wire}`.
  - `requests`: the normalized model requests (the existing normalizer).
  - `sessions`: the final transcript, read out of band with `harness.OpenView` over the `DiskStore` after the connection closes. It does not depend on ACP replay.
  - `wire`: the ACP frames in order, ids and paths replaced by placeholders.
- The test client validates every frame in both directions against the pinned `schema.json`: frames the adapter writes, and frames the client sends, so a test client that drifts cannot hide a decode bug. `wireClient` checks responses only (`e2e/wire_openapi_test.go:171`).
- The validator is new code. The pinned `schema.json` uses these keywords, and the validator implements each assertion: `$ref` (into `$defs`), `type` (including arrays with `null`), `properties`, `required`, `additionalProperties`, `unevaluatedProperties`, `items`, `enum`, `const`, `oneOf`, `anyOf`, `allOf`, `not`, `minimum`, `maximum`, `minLength`, and `format`. It treats these as annotations and ignores them: `$schema`, `$defs` (as a container), `description`, `title`, `default`, `discriminator`, `propertyName`, and every key that starts with `x-` (`x-method`, `x-side`, `x-deserialize-default-on-error`, `x-deserialize-skip-invalid-items`, `x-docs-ignore`). It fails on any other keyword, so a schema bump that adds a keyword fails at load, in a test, not in production. About 300 lines of test helper.
- Each scenario cites a sentence of the new ACP section of the spec, as the Contract suite requires of rows.
- Rows for the first release: initialize and version negotiation; new with `cwd` and a stdio MCP server; a second `cwd` gets its own project config; prompt with text; prompt with an image; prompt with a tool call and a diff; cancel during a tool call; cancel before the first token; failed turn error code; unknown session; bad JSON; `$/cancel_request`; slash command as text; two prompts in a row; model change by config option; ownership lost while connected; goal-driven turn with no prompt open; load and replay equality; list with a `cwd` filter; `mcpServers` that fails to connect.
- Hosts of the table: the binary host (`harness acp` over stdio), the in-process host (`io.Pipe`), the Node wasm host, and the browser host (9.6). Each scenario carries the flags `needsExec` (it calls `bash`, a process, or git) and `needsMCP` (any `mcpServers` entry). The Node and browser hosts run the scenarios with neither flag. A `WorkDir` scenario with file tools runs in both, because the Worker file system and the real disk serve the file tools.
- The `sessions` golden in the wasm hosts: the Playwright script copies the session directory out of the virtual file system (OPFS in the browser, the real directory in Node) into a temp directory, and the Go test reads it with `store.NewDiskStore` and `harness.OpenView`, as it does for the binary host. The `requests` and `wire` goldens come from the frames. The browser job compares `requests` and `wire` only until W6 gives it a persistent store; the `sessions` golden joins then.
- Runtime-only rows: C1 rows cannot run on the serve driver, because `serve` does not pass `WithSessionMCP`. They live in the ACP table (the binary host starts `harness acp`) and in a runtime-only table beside `contract_runtime_options_test.go`, which already holds options that serve does not set. A row that pins "`serve` has no way to set session servers" is a normal serve-driver row: the create body has no such field, and an unknown field is `invalid_request`.
- Spec and rules edits in A1: the Contract suite section says that a golden holds "not event shapes"; the ACP `wire` golden holds frames, because frames are the ACP contract, and the section says so. `e2e/AGENTS.md` ("Contract scenarios drive the real binary over HTTP") names the ACP table over stdio. A new `TestACPRows` checks that each scenario cites a sentence of the ACP section, as `TestRuntimeRows` does for the driver rows.

### 9.3 Unit tests

`AGENTS.md` allows unit tests for pure code. `internal/acp/rpc` (framing: bad JSON, empty line, long line, concurrent writes, `$/cancel_request` races, v2 batch arrays) and the mapping tables (tool kinds, error codes, delta tail computation) are pure. Without a unit test, each framing edge case becomes a process-level scenario. Decision 10 asks to add `internal/acp` to the pure-code list of the Unit tests section and of the "Test lines outside the contract suite" gate. The alternative is a line in `testdata/test-exceptions.txt` for each file.

### 9.4 TCK

A job builds `harness`, writes a config with a scripted model, and runs `uv run acp-tck -- harness acp`, pinned to a TCK commit. It is advisory until it passes on `main` for one release cycle, then required. `--protocol-version 2` starts with the v2 layer. The TCK is a third-party Python tool run by a workflow step; it is not repo tooling.

### 9.5 CI cost

These minutes are estimates. Each PR that adds a job measures its job and states the number.

| Job | When | Estimate |
| --- | --- | --- |
| ACP scenario table, in-process host | every PR, in the Contract job | 1 to 2 min |
| ACP scenario table, binary host | every PR | 1 to 2 min |
| TCK | `main`, nightly, and PRs that touch `internal/acp/**` or `cmd/harness/acp*` | 3 to 6 min (`uv` setup, one process per test) |
| `GOOS=js` build and size check | every PR, in the Static job | under 1 min |
| Node 24 wasm scenario run | PRs that touch the wasm, `sdk/embed`, or `internal/acp` files, and `main` | 2 to 3 min |
| Playwright, headless Chromium | the same paths, and `main` | 3 to 5 min (browser download is cached) |

### 9.6 The browser job

A Go test, `e2e/browser_test.go`, owns the job; a Playwright script is its fixture. The test starts a fake model server in Go on a second origin (`harnesstest` behind a CORS wrapper that test code owns), starts a static server for the page, the Worker, and the wasm asset (served with `Content-Encoding: br`), runs `node sdk/embed/test/browser.mjs` (Playwright, Chromium), reads its JSON result, and asserts in Go. The test runs only with `HARNESS_E2E_BROWSER=1`, as the Node host runs only with its variable, so a local `go test ./e2e/` builds no module and starts no browser. The CI job sets the variable, and with it a missing tool fails the test, because a skip in a required job hides a gap. The Node wasm job does the same with `HARNESS_E2E_NODE=1`; `ci.yml` has no Node job today, and W3 adds it. The test is a process-boundary test, which `e2e/AGENTS.md` allows.

Rows (the browser scenario subset, plus rows that exist only in the browser):

| Row | Pins |
| --- | --- |
| streamed text turn, tool turn, JS tool round trip through the Worker | frames and `requests` goldens equal the binary host (W7) |
| cancel mid-stream | the turn ends `interrupted` and the fake server sees the close within a bound; the default transport is not used |
| CORS blocked (the server allows only `content-type`) | one attempt; the error names the browser block; no retry |
| offline (`navigator.onLine` false) | the retry path stays |
| preflight | one `OPTIONS` for a two-request turn; the allow-headers list matches the adapter's headers |
| forbidden headers set through `extra_headers` | the page runs; the browser values arrive |
| file tools on the Worker file system | `read_file`, `edit_file`, `write_file`, `ls`, `glob`, `grep`, and `AGENTS.md` in the system prompt |
| default tool set | the first model request lists no `bash` and no `process` |
| reload | the page reloads at once, while the old Worker can still hold the store lock; the new page waits up to 2 s, gets the store, and `session/list` with no `cwd` lists the session, which then resumes with its history |
| list on a memory store | a streamed text turn, then `session/list` with no `cwd` lists the one session |
| two tabs, one store | the second tab waits 2 s and then gets `store_in_use`; no log corruption; after the first tab closes, the second boots and sees every record |
| two tabs, different new sessions | the second tab cannot create; the first loses no record |
| blob rename | a tool-result blob and an attachment survive a reload |
| crash durability | a Worker kill after an acknowledged append loses no record |
| direct Anthropic with the `extra_headers` entry (W9) | the header reaches the fake server |
| size and load | the module is under the budget (`go run ./internal/wasmsize`); the row reports the brotli size for information only; cold boot under a bound with CDP throttling at 50 Mbps |

The size budget is the raw and `gzip -9` size of the module, each the measured size plus 3% (rounded up to 50,000 bytes), set by the W1 PR and checked by `go run ./internal/wasmsize`. Brotli needs a module outside the standard library, so it is not a budget; the spike's brotli value is 3.58 MB. A bound on boot time is a regression guard, not a promise. Chromium only runs in CI first; the Playwright fixture takes the browser as a parameter, so Firefox and WebKit are one workflow line each when a consumer needs them.

## 10. Spec changes, by PR

The spec changes in the same PR as the code. A PR never names a package that does not exist (`internal/gates/structure.go:334` fails a Public packages row with no package).

| PR | Spec sections |
| --- | --- |
| P0 | Goals and non-goals: add "an ACP adapter over the Go API" and "a core that builds for WebAssembly". Non-goals unchanged. One line: the wasm host runs the module; harness embeds no JS engine. |
| C1 | Go API block: `SessionOption`, `WithSessionMCP`, the `Create` and `Open` signatures; MCP tools: session registry rules (7.4); no `protocol/` change. |
| A1 | New section "ACP" (methods, capabilities, schema pin, one version per connection, error table); Internal packages (`internal/acp`, `depguard` line); Command (`acp` builds one `Runtime` for each `cwd`); Startup rules in `AGENTS.md` (check the 80-line limit). |
| A2 to A8 | The mapping rows of the ACP section that each PR implements. |
| A4 | Tests and guardrails: ACP scenario table, schema check, TCK job. |
| W1 | New section "WebAssembly" (targets, seam table, size budget); CI gates row for the size budget. |
| W2 | Turn section, retry bullet: "except a transport error that declares itself permanent"; Internal packages (`internal/provider`: `Do` and the permanent marker). The Errors section is unchanged. |
| W3 | WebAssembly section: `harnessConnect`, the frame channel, `fetchRT`; Tests and guardrails: the Node wasm job. |
| W4 | Public packages: `sdk/embed`, and the corrected sentence on `sdk/typescript` (it is the plugin SDK); `sdk/AGENTS.md` scoped to `sdk/typescript`; new `sdk/embed/AGENTS.md`. |
| W5 | Tests and guardrails: the browser job and its rows. |
| W6 | Store and Ownership: a `DiskStore` on `globalThis.fs` in the browser, the store-wide lock, and the weaker durability. |
| W7 | `AllowedTools` text: the default set of a host. |
| W8 | The `sdk/embed` subprocess transport (SDK only; no spec section beyond the W4 package row). |
| W9 | WebAssembly section: the direct Anthropic demo (`extra_headers`), marked demo-only. |
| A9 | ACP section: the v2 layer (methods, updates, and the version rule for the second version). |
| (any) | `docs/plugins-and-protocols.md` External Protocol Surfaces: replace "Harness does not implement an ACP adapter today" with a pointer; keep "must not invent `session/request_permission`". `docs/design/slash-commands.md` names the ACP adapter as a consumer of `Runtime.Commands`. `CHANGELOG.md` entry under the unreleased heading for each user-visible PR. |

## 11. Boxes web console and ACP

Recommendation: the console does not use ACP as its transport.

Verified in `meetneptune/boxes` `web/src`:

- The browser calls the boxes control plane at `/v1/boxes/{id}/...` (165 mentions of `/v1/` in `lib/api.ts`). The control plane proxies the harness parts (comments at `lib/api.ts:639-677`). ACP would replace the proxied part only.
- Non-test files that mention each term: goal 20, queue 18, children or tree 41, compact 19, abort 14, models 10, journal 11, console-bootstrap 10, processes 7, workspace 6, commands 2. ACP covers prompt, cancel, slash commands, and models as config options. The rest would be `_meta` or `_harness/` extensions.
- The console reconnects with `Last-Event-ID` (`hooks/use-agent-stream.ts:1144`, `hooks/use-box-events.ts:71`). ACP v1 has no replay by sequence.

Verified in the ACP pages:

- The only stable transport is stdio (`transports.md`).
- The remote transport is an RFD. Its change log says "Moved to Active", and it states "This RFD is targeted for inclusion in **v1** as an additive feature". It specifies one connection id per connection and says nothing about several viewers of one connection. v1 sessions survive disconnects; in the RFD, "In-flight messages are not replayed". SSE `Last-Event-ID` resumption is "Deferred to v2" in that RFD. `replayFrom` is already in the v2 draft (`migration.md`, line 45).
- The v1 overview does not say whether a connection serves one client or many.

Boxes generates its console wire types from the harness HTTP contract. On boxes `origin/main`, `go generate ./protocol` (`protocol/internal/gen/harness.go`) copies the harness OpenAPI document as `protocol/harness-openapi.json` and writes `web/src/lib/harness-protocol.ts` from it. An ACP surface for the console is a second wire contract beside that one.

Where ACP fits boxes:

- Editors attached to a box: `harness acp` in the box, reached through a tunnel or the RFD transport once it is stable. The ACP peer has operator trust (7.4): any peer that reaches the tunnel can run commands through `mcpServers`. Boxes must authenticate the transport first; `harness acp` adds no authentication.
- A future in-browser wasm harness that speaks ACP to its host. The durable console stays on HTTP.
- Names: where a harness name is arbitrary, take the ACP name (`docs/plugins-and-protocols.md`).

## 12. Decisions for Andy

1. Adopt the ACP adapter and the wasm build target as goals (P0). Recommendation: yes. Both are clients of the public Go API and break no non-goal. The wasm host runs the module; harness embeds no JS engine, serves no UI (bailey owns it), and adds no permission system.
2. ACP version. Recommendation: v1 first (`schema-v1.25.0`). Start the v2 layer when the draft label leaves the v2 pages and the TCK has a stable v2 suite.
3. SDK for Go. Recommendation: no `coder/acp-go-sdk`. Hand-written slice, pinned schema, schema-checked frames. Build a generator if the slice passes 60 types.
4. Core split. Recommendation: do not split. Upper-bound gain: 2.35 MB raw (11.6%), 0.34 MB brotli (9.5%) (section 6). The browser module is already 3.58 MB brotli and loads in 0.7 s at 50 Mbps.
5. Wasm target. Recommendation: `GOOS=js`; the reactor waits for a non-JS host.
6. Tools. Recommendation: the built-in file and search tools on the virtual file system, a shell as a JS tool when a host wants one, never ACP `fs/*` and `terminal/*`, no `Workbench` type.
7. Work directory. Recommendation: one `Runtime` for each `cwd`, built by `cmd/harness acp` with the project config of that `cwd`, over one shared `Store` and `Owner`. Not a per-session `CreateSession.WorkDir`: it touches the ten readers of 4.2. The browser runs one project, so one runtime.
8. Per-session MCP. Recommendation: build it before the first ACP release as a Go-only `WithSessionMCP` option. The wasm host advertises no MCP and answers any `mcpServers` entry by `-32602` (7.4); a browser app supplies MCP as JS tools.
9. TCK. Recommendation: advisory first, required after one release cycle, pinned by commit.
10. Unit tests. Recommendation: add `internal/acp` (framing and mapping tables) to the pure-code list.
11. Browser target. Recommendation: ship it, with a JS SDK, a headless-Chromium CI job, and persistent storage. Node is a subset of it.
12. Worker or main thread. Recommendation: a dedicated Worker, always. The cost is a 0 to 1 ms hop; the gain is a UI thread that session work cannot block.
13. Browser store and its guard. Recommendation: `DiskStore` on the origin private file system through a small `fs` adapter in `sdk/embed`, under one store-wide Web Lock that the JS host takes before any file opens. One writing tab for each origin; a second tab gets `store_in_use`. The `Owner` stays `NewLocalOwner`. The browser store has weaker durability, stated in the spec: `fsync` is `flush()`, and the rename is the browser's `move()`. Rows pin what holds. Node: an exclusive store directory with a lock file. A native IndexedDB `Store` waits.
14. Tool set of a host. Recommendation: no new `Options` field. The adapter's `Config.AllowedTools` fills `CreateSession.AllowedTools` on `session/new`; `sdk/embed` sets the browser default.
15. Direct provider access. Recommendation: the supported pattern is a same-origin proxy that holds the key. Direct Anthropic works with `anthropic-dangerous-direct-browser-access: true`, set by the demo's own `extra_headers`; add no code option, mark it demo-only. Direct OpenAI is unsupported until it is proven with a real key.
16. Transport failure classification. Recommendation: reuse `provider.MarkPermanent`. `Do` marks a no-response failure permanent while `navigator.onLine` is true (a consumer-declared `Permanent() bool` detects it), so both retry layers are skipped and the turn ends `permanent`; the error text names `TypeError: Failed to fetch` and the likely causes. W2 edits the retry bullet of the Turn section.
17. Where the SDK lives. Recommendation: a new `sdk/embed`, separate from `sdk/typescript` (the zero-dependency plugin SDK, whose `sdk/AGENTS.md` rules conflict with a package that has dependencies and a wasm asset). W4 scopes those rules and adds `sdk/embed/AGENTS.md`. One client API, two transports: wasm (Worker or Node) or a spawned `harness acp`.
18. Browser CI. Recommendation: Playwright with headless Chromium, run by a Go test; Firefox and WebKit later through the same fixture.
19. Boxes console. Recommendation: keep HTTP and SSE. ACP serves editors and wasm. A remote ACP transport needs its own authentication, because the peer has operator trust.
20. `session/delete` and `title`. Recommendation: not advertised and null.
21. Auth. Andy decided on 2026-10-13: the native `harness acp` advertises the logins that the user config uses, so a client shows a login button. `chatgpt` (agent type) runs the browser sign-in of `harness login codex`; `claude-code` (agent type) runs `claude auth login` as a child, which needs no terminal; a terminal method of each runs `harness acp login codex` or `login claude-code` and is advertised only when the client advertises `auth.terminal`. `agentCapabilities.auth.logout` is `{}`, and `logout` ends every offered login. A missing login answers `-32000` with `data.code` `login_required`. Harness never reads Claude Code credentials. The wasm build offers none. `authMethods` stays empty with no such login, and `model_unavailable` stays `-32603`.
22. Several processes or tabs over one store. Recommendation: native, the later `Open` wins and the earlier process reports a notice and `session_not_owned`; two windows on one session take it from each other on each `session/load`. Browser, the first holder of the store lock wins and a second tab gets `store_in_use`. Node wasm, one process for each store directory.

## 13. Risks

1. Only Chrome ran (Chrome Headless Shell 156, localhost). Firefox and Safari are untested: Web Locks, IndexedDB behavior in a Worker, and `fetch` streaming can differ.
2. The TCK is experimental and incomplete. The prose spec and the schema stay the reference.
3. `Open` after replay can resume work (7.5). A client that dislikes this uses `session/resume`.
4. Chunks cannot be retracted in v1 (`item.discarded`). v2 upserts fix it.
5. `mcpServers` carries client-chosen commands and URLs (SSRF). The option is Go-only; the ACP peer has operator trust; a remote transport needs its own authentication. Session servers are not persisted.
6. A runtime for each `cwd` multiplies plugin and process state.
7. A goal-driven turn sends notifications with no open request. A v1 client may ignore them.
8. No `title`, no delete, no cost outside the Claude Code lane.
9. The size gain of a split is an upper bound from a crude cut; the stubbed module was not run.
10. The browser measurements use scripted turns and a fake model, so they omit real model latency and real tool-result sizes. A soak run (`TestBrowserSoak`, Chrome Headless Shell, Apple M1 Max, module built for the run) measured: 300 turns with one tool call each took 30.5 s (median 100 ms, p95 131 ms, max 180 ms); the two automatic compactions took 21 ms and 20 ms; after a reload, `session/load` replayed 1203 updates in 282 ms; a `grep` over 3000 files in `/work` took 33 ms; no main-thread task passed 50 ms and the longest event-loop lag was 3 ms. Linear memory was 12.0 MB after the boot, 20.0 MB after the 300 turns, 17.0 MB after the replay, and 18.8 MB after the `grep` (it never shrinks, so it is a high-water mark); the JS heap of the page stayed under 9 MB. The memory has no sign of a leak up to 300 turns. The scheduled job `browser-soak.yml` repeats the run and asserts only generous bounds (replay under 10 s, memory under 512 MB). A run with a real model, a long tool result, or more than 300 turns is not measured.
11. OPFS sync access handles exist in a Worker only, and `move()` is not atomic by contract (5.5). The store-wide lock covers the writers; durability after a crash and the blob rename depend on `flush()` and `move()` and are pinned by rows. If a browser lacks `move()`, the adapter copies and removes, and the spec states the crash window.
12. A "no response" failure while online is not retried (decision 16). A transient drop before the headers surfaces as a failure the user retries.
13. Direct provider access puts the key in the page. OpenAI direct is unproven.
14. A tool that a host supplies has the privilege of that host. Harness adds no permission check by design.
15. The wasm host connects no MCP server, so it does not meet the stdio MUST (7.4).
16. A 20 MB module on a 10 Mbps link takes about 3.6 s on the first visit.

## 14. Phased PR plan

Tracks: C (core), A (ACP), W (wasm and browser). One concern per PR.

| PR | Goal | Contract rows and tests | Done when |
| --- | --- | --- | --- |
| P0 (docs) | Goals edit only (section 10). | docs gates | Andy approves decision 1. |
| C1 | `SessionOption`, `WithSessionMCP` on `Create` and `Open`: per-session registry, lazy connect, stop on `Release`, `End`, `Close`; no persistence; secrets stay out of logs. | runtime-only rows: a session stdio server reaches the model; it stops at `Release`; a failed connect parks; an `Open` without the option has no session tools; no secret in the log. A serve row: the create body takes no server field. | the spec MCP section covers it; `go generate ./protocol` shows no diff |
| A1 | `internal/acp/rpc`, the pinned schema, the schema validator, `harness acp` answering `initialize`, the ACP section and Command text. | scenarios: initialize, version answer, bad JSON, unknown method; unit tests for `rpc`; a validator self-test | the startup step measures time to `initialize`; stdout holds ACP only; `TestACPRows` passes |
| A2 | `Runtimes` in `cmd/harness`, `session/new`, `session/prompt`, `session/cancel`, the event pump, text and thought chunks, stop reasons, the error table, `Config.AllowedTools`. Needs C1, A1. | scenarios: prompt, cancel, failed turn, `$/cancel_request`, goal turn with no prompt, ownership lost, a second `cwd`, MCP server in `session/new`, a default tool set | pass on both native hosts, race-clean |
| A3 | `tool_call`, `tool_call_update`: kinds, locations, diffs, images. | scenarios: tool call, edit diff, cancel during a tool | schema-valid frames; golden transcripts |
| A4 | TCK job (advisory). | the job | MANDATORY tier has no failure outside a written list |
| A5 | Config options, `available_commands_update`, `usage_update`, `session_info_update`, slash commands as typed text, and the lost-ownership `notice` (7.6, 8; only with the client capability `session.notices`). | scenarios: `/compact`, `/model`, option change, usage, ownership lost with and without `session.notices` | the TCK config-option tests pass |
| A6 | `session/load`, `session/resume`, `session/list`, `session/close`. | scenarios: replay equality, list with `cwd`, close | the TCK CAPABILITY tier for these passes |
| A7 | TCK required. Built: the job fails on a failure, runs on every pull request, and the `TCK` check is required to merge into `main`. | the job | one release cycle green on `main` |
| A8 | Elicitation for `question` requests. Built. | scenario: question round trip | capability appears only with the scenario green |
| A9 | v2 layer. | v2 wire scenarios; `--protocol-version 2` | v1 goldens unchanged |
| A10 (follow-up) | The prompt answer precedes every update of a later turn. The event pump owns this order: it holds at the `turn.ended` event of a prompt's turn until the rpc layer has queued the answer. Andy approved it (2026-10-13). Built. | a row whose frames hold the goal-turn text after the prompt response on every host | the `schedulerOrder` flag leaves the table |
| A5 follow-up | A useful model list for providers with no catalog needs a new Runtime list. | Andy decision | none yet |
| W1 | `cmd/harness-wasm` skeleton (it builds; `harnessConnect` follows in W3), the `ws_dial` tag, the `GOOS=js` build in the Static job, the size budget. | build check | the module builds in CI; the budget fails on growth |
| W2 | Transport failure classification: the `Permanent()` detection and `MarkPermanent` in `provider.Do`. `FetchError` and the readable JS error text move to W3. Independent of the ACP track. | runtime-only rows with a double `ModelTransport`: a permanent error is one attempt and the turn ends `permanent`; an ordinary transport error keeps both retry layers; a canceled request stays an abort | rows pass natively; the Turn retry bullet is edited |
| W3 | `harnessConnect`, the frame channel, `fetchRT` (abort tied to `ctx`, self-releasing `js.Func`), `FetchError` with readable error text, JS tools, `proxy.headers`, Node wasm host with `DiskStore` on a temporary directory, the Node job in `ci.yml`. Needs W1, A2. | the Node wasm host runs the scenario subset (no `needsExec`, no `needsMCP`): `requests`, `wire`, `sessions`; a `FetchError` text has no `<object>` | subset equals the binary host goldens; the job fails, not skips, when Node is missing |
| W4 | `sdk/embed` with the Worker transport: page API, Worker, `postMessage` frames, tool round trip, `proxy.baseURL`, the page-`fetch` bridge; `sdk/AGENTS.md` scoping and `sdk/embed/AGENTS.md`. Needs W3. | TypeScript tests of the client API over the Worker | the Worker runs a tool turn through the SDK |
| W5 | Playwright job with headless Chromium, the fake CORS server, and the rows of 9.6 that need only a memory store. Needs W4. | `e2e/browser_test.go` | green on `main`; the size row reports brotli (Node `zlib`) for information and the raw and gzip -9 budget stays in `go run ./internal/wasmsize`; CORS, cancel, preflight, header, and proxy-binding rows pass; the job fails, not skips, when Chromium is missing. The file-tool rows move to W6, and the frames and `requests` golden comparison of the streamed text, tool, and JS tool turns moves to W7, because a browser session needs a file system that only W6 gives |
| W6 | The `globalThis.fs` adapter over OPFS (`sdk/embed/src/opfsfs.mjs`, no dependency), the store-wide Web Lock before any file opens, `fsync` as `flush()`, `DiskStore` on `globalThis.fs`, the exclusive Node directory (processes that find one stale `.embed.lock` race for one claim). Needs W5. | rows: reload with `session/resume`; two tabs one store; two tabs different new sessions; blob rename; Worker kill (after an acknowledged turn, and after an `fsync` plus an unsynced write); hand-off; a Go probe of every `syscall` call on the adapter; the file tools on `/work` (`read_file`, `write_file`, `edit_file`, `ls`, `glob`, `grep`, `AGENTS.md`) | every row passes; the spec states the durability that the rows prove. `sdk/embed` cannot start a browser session until it lands; no release tag before W6 |
| W7 | Default tool set for a host (`harnessConnect({builtinTools})`, `sdk/embed` default); the frames and `requests` goldens of the browser host. Needs W4, W6. | rows: the first request lists no `bash` and no `process`; the frames and `requests` goldens of the text, tool, and JS tool turns equal the binary host | rows pass in the browser and in Node |
| W8 | `sdk/embed` subprocess transport (`spawn harness acp`). Needs W4, A2. | the same TypeScript tests over both transports | one client API test suite passes on wasm and on the subprocess |
| W9 | Docs for the direct Anthropic demo (`extra_headers`); no code. Needs W5. | the direct Anthropic row of 9.6 | the header reaches the fake server |

Order: P0 first. C1, A1, W1, and W2 can start in parallel after P0; W1 needs nothing from the ACP track because it only builds. A2 needs C1 and A1. W3 needs W1 and A2. W4 needs W3. W5 and W7 need W4. W6 needs W5. W8 needs W4 and A2. W9 needs W5. A3 to A6 follow A2.

## 15. Deferred until a consumer exists

| Item | Reason | Reopens when |
| --- | --- | --- |
| Per-session `CreateSession.WorkDir` | Ten readers and one project config (4.2). | One runtime must serve many projects. |
| wasip1 reactor and its ABI | Needs our own ABI; `GOOS=js` covers JS hosts. | A non-JS host (wazero, wasmtime) is named. |
| Native IndexedDB `Store` | `DiskStore` on OPFS works. | OPFS support or cost becomes a problem. |
| `Workbench`, `FS` and `Runner` interfaces in `builtin` | The built-ins already run on `globalThis.fs`. | A host needs a file system that is not `globalThis.fs`. |
| Core split and `harness/native` | Measured gain is 9 to 12%. | A size budget or a native embedder names the need. |
| A transport seam for MCP HTTP (`internal/mcp/http.go:44`) | No consumer; a browser app supplies MCP as JS tools. | A browser or Node app needs MCP servers inside the module. |
| Firefox and WebKit CI | One fixture parameter; no consumer yet. | A consumer names the browser. |
| `_harness/` methods and `_meta` keys | No consumer. | A client needs goals, queue, or children over ACP. |
| `session/delete`, `title`, `additionalDirectories` | No store method, no title rule, no consumer. | A consumer names one. |
| Opt-in editor buffers through `fs/read_text_file` | Native limit in 4.1. | Editor users report stale reads. |
