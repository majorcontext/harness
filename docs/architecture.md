# Harness architecture

Harness runs coding-agent sessions. A session is an append-only event log, one goroutine owns each session, and every seam is a small interface owned by its consumer. `cmd/harness` runs `serve`, `run`, `sessions`, and `plugin probe` on `harness.Runtime`, and `acp` over the ACP adapter. This document is the binding spec of the system as it is.

## Goals and non-goals

Goals:

- One owner and one source of truth for each piece of state.
- Every public interface comes from what its consumer needs.
- A public Go API small enough to read in one sitting.
- One HTTP contract, built from the same Go types as the Go API.
- Pluggable storage and ownership, so a session survives process loss.
- An ACP (Agent Client Protocol) adapter over the Go API, so an editor or a JavaScript host drives a session.
- A core that builds for WebAssembly and runs in a browser and in Node. The host runs the module; harness embeds no JavaScript engine.

Non-goals:

- Backward compatibility of routes, on-disk formats, config, or the Go API. Every consumer changes in the same release.
- Permission systems, plan mode, a JS runtime, a web UI.
- Multi-writer sessions. One owner writes each session log.
- Durable token deltas. Deltas stay ephemeral.

## Four rules

1. A session is an append-only event log. One `Apply` function builds state from it, live and on replay.
2. One goroutine owns each session's state. Other code sends it commands and reads published, immutable views.
3. Every seam is a small interface declared by the package that uses it. Nothing branches on a backend or provider name.
4. Each interface is designed from its consumer's need. No shape, name, or field survives only because it exists.

### Go principles

The Go guidance these rules follow:

| Rule | Source |
| --- | --- |
| Server logic in `internal/`; small public surface | [Organizing a Go module](https://go.dev/doc/modules/layout) |
| Interfaces belong to the consumer | [Code review comments](https://go.dev/wiki/CodeReviewComments) |
| Share memory by communicating; one goroutine owns data | [Effective Go](https://go.dev/doc/effective_go), [codelab](https://go.dev/blog/codelab-share) |
| Make goroutine exit explicit | [Code review comments](https://go.dev/wiki/CodeReviewComments), [Pipelines](https://go.dev/blog/pipelines) |
| `ctx` first; cancel with a cause | [Context](https://go.dev/blog/context) |
| Wrap with `%w`; match with `errors.Is` and `errors.As` | [Go 1.13 errors](https://go.dev/blog/go1.13-errors) |
| Add, do not change; return concrete types | [Module compatibility](https://go.dev/blog/module-compatibility) |
| Go 1.22 `ServeMux` patterns | [Routing enhancements](https://go.dev/blog/routing-enhancements) |
| Table tests; `synctest` for time | [Table-driven tests](https://go.dev/wiki/TableDrivenTests), [synctest](https://go.dev/blog/synctest) |
| No `util`, `common`, `api`, `types` packages | [Package names](https://go.dev/blog/package-names) |

## Rules for changes

- This document is the binding spec. Code, plans, and reviews follow it as written. A difference from it is a blocker. When it is silent or unclear, stop and ask; do not invent.
- A change to the spec is a decision of the owner. The same PR, or a docs PR, edits the text so that it describes the system as it is after the change. The text holds no history: no phases, no comparison with an earlier system, no dated decision notes. The commit and the PR body carry the reason.
- A PR cites the sections of this document that it implements.
- A contract test is a scenario row in `e2e/`. A behavior change adds a row and a bug fix adds one row to the nearest table. No other test is written unless the code is pure (see Unit tests).
- A comment never states history: no issue numbers, dates, "previously", or "no longer". A history comment leaves when its code changes.
- Every `protocol` type change and every `internal/server` `Table` change commits the output of `go generate ./protocol`.
- Every error code is owned in four places: a `protocol` constant, a `protocol.HTTPStatus` entry, a row of the Errors table with the same status, and a value of the `Error.code` enum of `protocol/openapi.json`. A sentinel error backs every code except `internal`, `payload_too_large`, and `unauthorized`.
- The Sync receiver is not the session API. Its codes (`protocol.CodeStaleEpoch`, `protocol.CodeSyncConflict`) and its body bound (`protocol.MaxSyncBatchBytes`) are `protocol` constants, not rows of the Errors table. A receiver and a sender import them; neither copies the strings or the number.
- One concept has one resolver or one writer, and `internal/gates/structure.go` lists it (see CI gates, Structure). An allow-list entry carries a date and a reason, and a stale entry fails.
- The gates in "CI gates" run on every PR. A change never weakens a gate to pass it.

## Architecture

Three kinds of consumer drive one `harness.Runtime`: a control plane over HTTP and SSE, an embedder through the Go API, and the CLI. The runtime writes through `harness.Store` and runs a session only while `harness.Owner` grants it.

```
  control plane          embedder (in-process)          harness CLI
         │ HTTP + SSE            │ Go API                     │
         ▼                       ▼                            ▼
  ┌─────────────────────────── harness.Runtime ───────────────────────────┐
  │ server    http.Handler over the runtime                               │
  │ session   one goroutine per session; runs only while it owns it       │
  │ turn      agent loop → Backend (capabilities) · []Tool · Source       │
  └──────────────┬─────────────────────────────────────┬──────────────────┘
                 │ Append(id, expectedSeq, records…)   │ Acquire(id) → Ownership
           harness.Store                          harness.Owner
   DiskStore · MemStore · a shared store         local · a lease
```

The session layer is the one owner of state. The store write is the fence: a stale owner's append fails with `ErrConflict`.

Data flow of an input: the HTTP handler or the Go API calls a `Session` method. The method sends a command to the session actor, which appends `input.admitted` and starts a turn. The turn loop calls a `Backend` with the history and a `Source` of tools. The backend calls a provider, or delegates to a third-party harness. Each item and the end of the turn return to the actor as commands. The actor appends them, applies them to `State`, and publishes a view. SSE subscribers and `Session.Events` read the appended records.

### Public packages

These are the compatibility surface.

| Package | Owns | Consumer |
| --- | --- | --- |
| `harness` | `Runtime`, `Options`, `Session`, `Turn`, `TurnResult`, `OpenView`, `ReadEvents`, `View`, `Reader`, `Blob`, `Store`, `Owner`, `Ownership`, `Sync`, `Advance`, `Tool`, `Call`, `AloneTool`, `KeyedTool`, sentinel errors | embedders, control planes, CLI |
| `harness/store` | `DiskStore`, `MemStore`, `CheckName`, `CheckRecords`: the stores that ship with harness | embedders, CLI |
| `harness/httpapi` | `Handler` over a `Runtime`, `ReadHandler` over a `Store`, `ReadOptions`, `Queue`: the HTTP API, built on the exported methods of `harness` | control planes, CLI |
| `harness/httpsync` | `New`, the `Sync` that posts batches over HTTP, and `ApplySync`, the rules of its receiver | box harness, control planes |
| `harness/config` | `Config`, `Defaults`, `Validate`, `ApplyEnv`, `Load`, `Parse`, `LoadProject`, `MergeProject`, `Config.WithProxyBase`; imports only the standard library | boot code that writes a config |
| `harness/plugin` | `Plugin`, `ToolBefore`, `Tool`, `Client`, `Serve`, `ServeConn`, and the wire records of each hook, event, tool, and client call, with `Manifest`, `Part`, and `Message`: the Go API for a plugin, as a process or as a value in a runtime | plugin authors |
| `harness/protocol` | Data types shared by the Go API and HTTP, including `SyncBatch`; source of the generated `openapi.json` and `protocol.ts` | servers, web clients, CLIs |
| `harness/storetest` | Conformance suite for a `Store` | store implementations |
| `harness/harnesstest` | Scripted model servers for contract suites | harness and embedder suites |
| `sdk/embed` | `createHarness`, `wasm`, `subprocess`: the JavaScript client of the WebAssembly module and of the native `harness acp`. It starts the module in a Worker, bridges the tools, the fetch, and the proxy of the page to it, or spawns the binary under Node, and gives the caller an ACP stream | web pages, Node programs |

`cmd/harness` is the binary. `sdk/typescript` is the TypeScript SDK for plugins: it speaks the plugin protocol and has no dependency.

`sdk/embed` is plain JavaScript with no runtime dependency. Without `builtinTools`, the `wasm` transport passes `defaultBuiltinTools` and the names of `tools` to `harnessConnect` (`AllowedTools`). Under Node, `createHarness({transport: subprocess({command, args, env, cwd, closeTimeout}), config, store, log})` spawns `harness acp` instead and speaks ACP over its stdio. It writes `config` to a temporary file and names it in `HARNESS_CONFIG_FILE`, which `transport.env` cannot override, and it sets `HARNESS_SESSION_DIR` to `store.dir`. The project config `<cwd>/.harness.json` still merges over `config`, and the `HARNESS_*` variables of the environment still apply. Without `store.dir`, the sessions go to the session directory of the user, not to memory, and `store.persist` has no effect. The transport rejects `tools`, `builtinTools`, `proxy`, and `fetch`, because the binary has its own tools and the page cannot answer for it. `createHarness({transport: wasm({module, wasmExec, worker: true}), config, proxy, fetch, tools, builtinTools, store, log})` starts the module in a dedicated Worker, a browser Worker or a Node `worker_threads` Worker, and calls `harnessConnect` there with the work directory of its file system as `defaultDir`: the working directory of the process under Node, `/work` in a browser, so the module advertises `session/list` over a memory store and over a persistent store. It returns `{stream, close, fs}`. `stream` is a `{readable, writable}` pair of ACP messages as parsed JSON, which an ACP client reads and writes. The page and the Worker exchange ACP frames as `{frame}` messages. A message moves data, never a function, so every option is serializable. The bridges run on the same rule, one request message from the Worker and the answers of the page:

- A tool call of the module arrives at the page as `{tool: {tid, name, args}}`. The page runs `run(input, {signal})` of the tool and answers `{toolResult}`; `{toolAbort}` aborts the signal. A rejection is an error result.
- When the page passes `fetch`, the Worker gives the module a bridge as the option `modelFetch` and replaces no global. A model request goes to the page as a `{fetch}` message with the method, the URL, the headers, the body, and the `redirect` mode `manual`, which every request carries. The page passes the mode to its `fetch`. A request with the mode `manual` or `error` fails when the page answers with an `opaqueredirect`, a 3xx status, or a response with `redirected` true, also when the page `fetch` ignores the mode. A `fetch` callback must pass `init.redirect` on to the network: when it follows a redirect itself, the module cannot stop the request that the redirect makes, and the token has gone to its target. The page answers the status and the headers, then each body chunk as a transferred `ArrayBuffer`, then an end message or an error message. An abort of the request, or a cancel of the response body, posts `{fetchAbort}` and the page aborts its `AbortController`. Without `fetch`, the module uses the `fetch` of the Worker.
- `h.fs` is the file system handle of the page, and it exists with the `wasm` transport in a browser only: under Node the module runs on the disk of the host, so `h.fs` is `undefined` there and with `subprocess`. Its calls are `readFile`, `writeFile` (with `append`), `readdir`, `stat`, `mkdir` (with `recursive`), `rm` (with `recursive` and `force`), and `rename`, all async, with absolute paths. Each call goes to the Worker as `{fsop: {id, op, args}}` and runs on `globalThis.fs`, the file system of the module (`/work`, `/tmp`, `/persist`), and the Worker answers `{fsres}` with a byte array as a transferred `ArrayBuffer`. An error carries the POSIX `code` of the module. The calls are not atomic against a write of the module, and `close()` rejects every call that waits. `run(input, {signal, fs})` of a page tool gets the same handle. The handle reaches `/persist`, where the session store lives, so a page passes it to trusted code only. `readFile` of a directory fails with `EISDIR`.
- The export `@majorcontext/harness-embed/shell` has `shellTool({name, load, timeoutMs, maxCommands, maxOutput, maxOutputBytes})`, a page tool with the input `{command}` that runs a just-bash shell over `h.fs`. It is a separate file: a page that does not import it loads no part of it, and `just-bash` is an optional peer dependency that loads at the first call. The shell mounts `/work`, which is its working directory, and `/tmp` when the module has it; both are the files that the file tools read. Its bundle needs `node:zlib` aliased to the export `@majorcontext/harness-embed/zlib-stub`, with which `gzip`, `gunzip`, `zcat`, and `rg -z` on a `.gz` file fail. It has no network, so `curl` does not exist, and no process, symbolic link, version control, or `tar`. Each call starts a new shell, so `cd` and variables end with the call. A call stops at `timeoutMs` (default 10 s) or at `maxCommands` (default 10000) and returns stdout, stderr, and the exit code, each stream cut to `maxOutput` characters (default 20000, head and tail kept) for the model. `maxOutputBytes` (default 8 MiB) is a separate limit: a command that writes more bytes than that in one call, to stdout, stderr, or a file, stops, so a pipeline can write a large file while the model sees a clipped result. The default `name` is `bash`: the tool replaces the built-in `bash`, because `sdk/embed` lists the names of the page tools in the allowed tools. Without `h.fs` the call fails.
- `proxy.headers`, `proxy.baseURL`, and `proxy.providers` go to `harnessConnect`. `proxy.providers` maps a provider name to `{baseURL, loginURL}`. A relative URL resolves against the page, because the `url` of an elicitation must be absolute.
- `store.dir` under Node is a directory that one process holds through `.embed.lock`; a second `createHarness` on it fails. In a browser, `store.persist` keeps the sessions in the origin private file system below `store.dir` (default `/persist/sessions`, and `store.dir` has no `.` or `..` segment) and one page of the origin holds the store at a time (see Store); a second `createHarness` rejects with the code `store_in_use`. Without `store.persist`, and under Node without `store.dir`, the store is in memory.

`stream` has no backpressure in either direction. `close()` ends the connection, waits for the module to release its sessions (it does not wait when the Worker has failed), ends the Worker, releases the store directory, and closes `stream`. A cancel of `readable` or an abort of `writable` does the same, and a write after the close rejects.

The Go API:

```go
package harness

func New(opts Options) (*Runtime, error) // no I/O; sessions load on Create, Open, or List

type Options struct {
	Store  Store         // required
	Owner  Owner         // nil: NewLocalOwner(1)
	Sync   Sync          // nil: no replication
	Config config.Config // New checks it with Validate; it reads neither owner_epoch nor sync
	Logins *Logins       // the sign-in state of injected providers; the hosts of this module set it, embedders leave it nil, and each runtime then makes its own from ModelTransport
	// ModelTransport returns the HTTP transport for a model provider.
	// nil, or a nil result: the default transport.
	ModelTransport func(provider string) http.RoundTripper
	// Getenv reads the variables that hold provider API keys. nil: New and the
	// runtime read no variable of the process. A backend child process, a plugin,
	// an MCP stdio server, a process, or a bash call still inherits the environment.
	Getenv func(key string) string
	// Logger receives every log line of the runtime. nil: slog.Default().
	Logger *slog.Logger
	Tools          []Tool // embedder tools, beside the built-in, process, task, MCP, and plugin tools
	// WorkDir is the directory of a coding agent. Each session gets the
	// built-in tools, bash among them, so it grants command execution.
	// Empty: no file is read, no process runs, no built-in tool exists,
	// and the system prompt is append_system_prompt alone. New makes a
	// relative path absolute, and fails when it cannot.
	WorkDir string
	// Version is the build version that the banner names. Empty: no banner.
	Version string
	// AskUserQuestion lets a session ask the user a question, which the
	// embedder answers with Session.Resolve.
	AskUserQuestion bool
	// Plugins run in this process, after the plugins of Config.Plugins. A name must be unique,
	// and a tool name may not repeat a built-in tool, an Options.Tools tool, or another plugin tool.
	// Close waits for their hook and tool calls; a call that ignores its context holds Close
	// until the context of Close ends.
	Plugins []*plugin.Plugin
	// ServeURL and RunToken go to each plugin in its initialize call. A token needs a URL.
	ServeURL, RunToken string
}

// A Runtime hosts many sessions. Each runs only while its Ownership holds.
func (r *Runtime) Create(ctx context.Context, req protocol.CreateSession, opts ...SessionOption) (*Session, error)
func (r *Runtime) Open(ctx context.Context, id string, opts ...SessionOption) (*Session, error) // acquire, fence, replay, resume
// A SessionOption is Go-only: no protocol field and no HTTP route carries it.
type SessionOption func(*sessionOptions)
func WithSessionMCP(servers map[string]config.MCPServerSpec) SessionOption // MCP servers of one session, beside Config.MCPServers; not persisted
func (r *Runtime) End(ctx context.Context, id string) error // end and unload; session_busy while a turn or a control command runs; the log stays
func (r *Runtime) List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error) // creation order
func (r *Runtime) Models(ctx context.Context) []protocol.Model // the models of each provider; the list of a provider entry with `models_from` `chatgpt` that sets no `models` comes from the ChatGPT backend (see ChatGPT subscription login)
func (r *Runtime) Commands(ctx context.Context) (protocol.Commands, error) // the slash-command menu; the cause of ctx when it has ended
// The box: health, the work directory, the logger, and the processes of the box.
func (r *Runtime) Health() protocol.Health
func (r *Runtime) WorkDir() string // "" without a WorkDir
func (r *Runtime) WorkspaceChanges(ctx context.Context, dir, scope string) (protocol.WorkspaceChanges, error) // the diff of the work tree; invalid_request without a WorkDir
func (r *Runtime) Logger() *slog.Logger
func (r *Runtime) Processes() []protocol.ProcessInfo // none without a WorkDir
func (r *Runtime) ProcessLogs(name string, tail int) (protocol.ProcessLogs, error) // ErrProcessNotFound for an unknown name
func (r *Runtime) StartProcess(ctx context.Context, name string) (protocol.ProcessStatus, error) // work that Close waits for; ErrDraining after Close starts
func (r *Runtime) StopProcess(ctx context.Context, name string) (protocol.ProcessStatus, error)
func (r *Runtime) RestartProcess(ctx context.Context, name string) (protocol.ProcessStatus, error)
// Close hands off every session, then returns once Sync has acknowledged
// every record through each handoff. It is idempotent: the first call shuts
// the runtime down and later calls return its result. Each call returns by the
// end of its ctx with context.Cause(ctx). When the ctx of the first call ends,
// it cancels every run and kills the processes, and does not wait for a tool
// that ignores ctx. A session that Sync rejected for good does not fail
// Close; SyncStopped reports it. A model, plugin, or MCP teardown that the ctx cuts short also returns context.Cause(ctx).
func (r *Runtime) Close(ctx context.Context) error

// CatchUp replicates every stored session through Sync, opening none. A box harness calls it at start.
func (r *Runtime) CatchUp(ctx context.Context) error
// SyncStopped reports whether a Sync rejected a batch for good since New. Sync may then lack records of a session that the runtime ran.
func (r *Runtime) SyncStopped() bool
// ProbePlugins probes each plugin of Config.Plugins, refreshes the manifest cache only before the first Create or Open, and returns each plugin of Config.Plugins and Options.Plugins with its tools and hooks.
func (r *Runtime) ProbePlugins(ctx context.Context) ([]protocol.Plugin, error)

// Session is a handle on one running session. Open returns the same handle for a session until its Done closes: the runtime forgets the session and closes Done in one step. It stops for good at Done; call Open to run the session again.
func (s *Session) ID() string
func (s *Session) Done() <-chan struct{} // closes when the session stops on this runtime
func (s *Session) Err() error            // nil before Done; then matches ErrSessionNotOwned and one cause: ErrSessionReleased, ErrSessionEnded, or ErrOwnershipLost; a Store failure or a Sync rejection that stops the session matches ErrSessionNotOwned and that error instead
func (s *Session) Snapshot() protocol.Session // includes HeadSeq and SyncedSeq
// Prompt admits an input and returns the Turn that will take it. A repeated input ID returns the Turn of the original receipt (Admitted.Repeat).
func (s *Session) Prompt(ctx context.Context, in protocol.Input) (*Turn, error)
// Turn returns the Turn of an earlier receipt, for example after Open on a new runtime.
func (s *Session) Turn(a protocol.Admitted) *Turn
func (s *Session) Messages(ctx context.Context, before uint64, limit int) (protocol.MessagePage, error) // a limit above protocol.MaxMessageLimit fails with invalid_request
func (s *Session) Inputs(ctx context.Context) ([]protocol.QueuedInput, error) // queued inputs, oldest first
func (s *Session) Blob(ctx context.Context, key string) (Blob, error) // an attachment that an input names; else ErrBlobNotFound
func (s *Session) Withdraw(ctx context.Context, inputID string) error // removes a queued input; an input that is not queued is no error
func (s *Session) Interrupt(ctx context.Context, req protocol.Interrupt) error
func (s *Session) Resolve(ctx context.Context, requestID string, res protocol.Resolution) (protocol.Resolved, error)
func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)
func (s *Session) SetGoal(ctx context.Context, g protocol.Goal) error
func (s *Session) ClearGoal(ctx context.Context) error
func (s *Session) Compact(ctx context.Context, req protocol.Compact) (protocol.Compacted, error)
func (s *Session) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] // ends with Err when the session stops
func (s *Session) Release(ctx context.Context) error // hand off, flush Sync, release ownership

// Turn follows the outcome of one input. It holds no goroutine: Events and Wait read the log of the session.
type Turn struct{ /* unexported */ }

func (t *Turn) Admitted() protocol.Admitted // the receipt of the input
// Events yields the events from the admission through turn.ended, input.withdrawn, or the final command.recorded; it ends with Session.Err when the session stops first.
func (t *Turn) Events(ctx context.Context) iter.Seq2[protocol.Event, error]
// Wait returns when the turn that took the input ends, the input is withdrawn, or the command finishes.
func (t *Turn) Wait(ctx context.Context) (TurnResult, error)
// Steer admits a steer input with expected_turn_id set to this turn; ErrTurnMismatch when no turn runs.
func (t *Turn) Steer(ctx context.Context, in protocol.Input) (*Turn, error)
// Cancel withdraws the input while it is queued, or interrupts the turn that took it.
func (t *Turn) Cancel(ctx context.Context) error

// TurnResult is the outcome of an input.
type TurnResult struct {
	InputID    string
	TurnID     string              // empty for a withdrawn input and for a slash command
	StopReason protocol.StopReason // completed, interrupted, failed, or awaiting_input
	Cause      protocol.Cause      // why the turn stopped early; empty otherwise
	Error      string              // masked failure text
	ErrorClass protocol.ErrorClass
	Usage      protocol.Usage      // the model calls of the turn that the log holds from the admission of the input on
	Command    string              // the final status of a typed slash command
	Withdrawn  bool                // no turn took the input
}

// OpenView reads a session from any Store without owning it: no Acquire, no appends.
func OpenView(ctx context.Context, st Store, id string) (*View, error)
func (v *View) ID() string
func (v *View) Snapshot() protocol.Session
func (v *View) Inputs(ctx context.Context) ([]protocol.QueuedInput, error)
func (v *View) Blob(ctx context.Context, key string) (Blob, error)
func (v *View) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
func (v *View) Messages(ctx context.Context, before uint64, limit int) (protocol.MessagePage, error)
// Resumable reports whether Open would resume something: a running or suspended turn, a queued input, an active or paused goal (a blocked goal is not resumable), a command that no owner finished, or a child that has not settled.
func (v *View) Resumable() bool

// Reader is the read surface of a session, live or stored. *Session and *View implement it.
type Reader interface {
	Snapshot() protocol.Session
	Messages(ctx context.Context, before uint64, limit int) (protocol.MessagePage, error)
	Inputs(ctx context.Context) ([]protocol.QueuedInput, error)
	Blob(ctx context.Context, key string) (Blob, error)
	Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
}

// Blob is an attachment. The caller closes Body.
type Blob struct {
	MediaType string
	Size      int64
	Body      io.ReadCloser
}

// Read returns the running Session of id, or a View of its stored log. It acquires nothing.
func (r *Runtime) Read(ctx context.Context, id string) (Reader, error)

// ReadEvents yields the stored events after `after`, through the head it reads first; it neither owns the session nor replays the log.
func ReadEvents(ctx context.Context, st Store, id string, after uint64) iter.Seq2[protocol.Event, error]

// NewLocalOwner grants every session to this process at epoch, one grant at a time.
func NewLocalOwner(epoch uint64) Owner
// Sync replicates each session's records elsewhere, in seq order.
type Sync interface {
	// Deliver returns the receiver's head on success and on a seq mismatch;
	// the sender resends from Head+1. ErrStaleEpoch, ErrConflict, or
	// ErrSyncRejected stops the session and releases its Ownership.
	Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error)
}

// Tool is an embedder tool. The runtime reads Spec once, at New. The calls of one
// model response run at the same time, so Run allows concurrent calls, and it
// must return when ctx ends. An error becomes an error result.
type Tool interface {
	Spec() protocol.ToolSpec
	Run(ctx context.Context, call Call) (protocol.ToolResult, error)
}

// Call is one tool call and the session that made it.
type Call struct {
	ID        string // the call ID of the model, stable across a resumed turn, for idempotent effects
	Name      string
	Arguments json.RawMessage
	Session   string // the calling session
	Root      string // the root of the session tree
	TurnID    string
}

type AloneTool interface { Tool; Alone() }            // the call runs with no other call in flight
type KeyedTool interface { Tool; Key(c Call) string } // calls with the same non-empty key run one at a time, in call order
```

The three adapter packages import `harness` and nothing of its internals beyond the packages that `.golangci.yml` lists:

```go
package store // harness/store

func NewDiskStore(root string) *DiskStore // flock-fenced files; fsyncs every Append and PutBlob
func NewMemStore() *MemStore              // in memory, for tests and -no-save runs
func CheckName(name string) error         // reports a session ID or blob key that a Store must refuse
func CheckRecords(records [][]byte) error // reports an empty record, or a record with a newline

package httpapi // harness/httpapi

// Handler serves the HTTP API of r. It calls only exported methods of Runtime and Session and has no authentication.
func Handler(r *harness.Runtime) http.Handler
// ReadHandler serves the read routes over st with no Runtime.
func ReadHandler(st harness.Store, opts ReadOptions) http.Handler
type ReadOptions struct {
	Queue  Queue        // inputs that wait outside the log; nil: the log only
	Logger *slog.Logger // receives the cause of each error that the handler masks as internal; nil: slog.Default()
}
type Queue interface {
	Queued(ctx context.Context, session string) ([]protocol.QueuedInput, error) // oldest first
}

package httpsync // harness/httpsync

// New returns a Sync that posts each batch to spec.URL and reads spec.TokenFile again for each post.
func New(spec config.SyncSpec) harness.Sync
// ApplySync appends b to st by the receiver rules of a Sync (see Events).
func ApplySync(ctx context.Context, st harness.Store, b protocol.SyncBatch) (protocol.SyncAck, error)
```

`httpapi.Handler` is a thin mapping of the exported methods of these types, and it calls no other method; `Runtime.Read` serves every read route, and the compiler proves that the Go API covers each route. `httpapi.ReadHandler` maps `OpenView`. A read of a session that the runtime does not run names the plugins of that runtime. An embedder and a remote client see the same types and the same errors.

### Internal packages

Free to change.

| Package | Owns |
| --- | --- |
| `internal/server` | HTTP mapping of the Go API, generic over the sessions that `httpapi` supplies |
| `internal/acp` | The Agent Client Protocol adapter. It reads and writes bytes through an `io.Reader` and an `io.Writer`, so it does not know its host. It imports `harness`, `config`, and `protocol` |
| `internal/acp/cwdruntimes` | The `Runtimes` of the adapter: one `harness.Runtime` for each work directory, over one `Store` and one `Owner`. The host injects the config loading and the shared `Store` and `Owner`. It imports `harness`, `config`, `internal/acp`, and `internal/injectedauth` |
| `internal/acp/acpauth` | The logins that `harness acp` offers through ACP: the ChatGPT login of `internal/codexauth`, and the login of the Claude Code CLI, which it asks through `internal/claudeauth`, and the login of an injected provider entry, which it asks through the status probe of `internal/injectedauth`. It imports `config`, `internal/acp`, `internal/claudeauth`, `internal/codexauth`, `internal/injectedauth`, and `internal/process` |
| `internal/acp/rpc` | The newline-delimited JSON-RPC 2.0 connection of the adapter: request IDs, notifications, `$/cancel_request`, and one writer. It imports the standard library only |
| `internal/jshost` | The adapters of a JavaScript host for `GOOS=js`: the fetch `RoundTripper` and `FetchError`, JavaScript tools, the frame channel, and the readers of host options. It builds only for js and imports the standard library, `harness`, and `protocol` |
| `internal/listing` | The page of sessions in creation order, shared by `Runtime.List` and `httpapi.ReadHandler` |
| `internal/storecheck` | The name and record rules that `store.CheckName` and `store.CheckRecords` report and the runtime applies |
| `internal/eventlog` | The record codec, `Apply`, and `Check`. The event schema is in `protocol` |
| `internal/session` | Session actor, mailbox, state machines, views, replication, and the child commands |
| `internal/tree` | The child tree: limits, spawn, report, recovery, the tree interrupt, and the `task` tool. It reads the runtime through its own `Sessions` interface |
| `internal/selftool` | The `goal`, `model`, and `session_info` tools, which let a model inspect and change its own session. They reach the runtime through their own `Sessions` interface |
| `internal/turn` | One agent loop; declares `Backend`, `Tool`, and `Source`; `Restrict` |
| `internal/backend` | The `turn.Backend` of a runtime: it builds a backend for each provider of the registry (the native `anthropic`, `openai`, `codex`, and `claude-code`, and each configured entry) and routes each model ref to one |
| `internal/backend/modelapi` | The one model API backend, for every provider wire |
| `internal/provider` | The wire adapters `anthropic`, `openai` (Responses, the Codex lane included), and `openaicompat`, and the typed `provider.Error`. `provider.Do` sends a request again after a transport error, and marks a transport error that declares itself permanent with `MarkPermanent`. Only `internal/backend` builds them |
| `internal/claudeauth` | The login status of the Claude Code CLI: it runs `claude auth status --json` in its own process group and reports `loggedIn`, and it types the failure of a turn that needs the login (`LoginRequiredError`, the class `login_required`). The Claude Code backend asks it after a failed turn, and `internal/acp/acpauth` asks it before a turn. It imports the standard library and `internal/process` only |
| `internal/injectedauth` | The login state of a provider entry with `auth` `injected`: the status probe, its cached answer, the transport that turns a 401 of a model call into the login-required error or the permanent error, and the `Logins` owner that the composition root of a connection makes once and gives to the backend and to the ACP logins. The owner holds the model transport of the connection, the clock, and the function that returns the probe of an entry (`backend.ProbeOf`), so this package names no wire. It imports `config` |
| `internal/codexauth` | The Codex login: the credential file and its lock, the OAuth sign-in flows, the refresh with one flight for each process, and the `RoundTripper` that binds the login to one endpoint. It imports the standard library only |
| `internal/backend/external`, `claudecode` | Third-party harness backends: the shared adapter code, and Claude Code |
| `internal/tool/mcpsrc` | A `turn.Source` that gives MCP tools to each model call |
| `internal/tool/pluginsrc` | A `turn.Source` and `turn.Hooks` that give the plugin tools, hooks, and events to each session |
| `internal/tool/proc` | The `process` tool and the process status line |
| `internal/tool/builtin` | The file, search, and shell tools of a coding agent |
| `internal/tool/webfetch` | The `web_fetch` tool: the address check, the redirect rule, and the HTML converter |
| `internal/toolresult` | Large-result retention and `read_tool_result` |
| `internal/admit` | Checks each part of an input and returns the `input.admitted` record with the blobs of its attachments |
| `internal/prompt` | System-prompt segments and agent profiles, and the engine-context sentinel: `RenderEngineContext` and `NeutralizeEngineContextSentinel` |
| `internal/workspace` | The diff of the work tree for `GET /workspace/changes`. It reads a work tree through an `Engine`, and git is the one production engine. It cannot reach the runtime or any session |
| `internal/message` | The provider-wire conversation model |
| `internal/modelref` | `ModelRef` ("provider/model") and `Effort`, with their parsers. A leaf with no harness imports |
| `internal/backoff` | The wait between two attempts of a retried model request or model call. A leaf with no harness imports |
| `internal/modelmeta` | Context-window table, display names, descriptions, and effort levels from models.dev; only the model API backend reads it for a window. `Runtime.Models` and `GET /models` list the chat models of each provider of the registry, the native `anthropic`, `openai`, `codex`, and `claude-code` among them (the list of a provider entry with `models_from` `chatgpt` comes from the ChatGPT backend, see ChatGPT subscription login), by provider name and then newest first. Each `protocol.Model` has an optional `provider_name` (the models.dev name of the provider), `name`, `description`, and `efforts`. A provider entry that sets `models` lists exactly those ids, in the given order, in place of the catalog of its provider; this is the way to list the models of a provider with no catalog, such as an `openai-compat` gateway. A listed id that the tables hold (after the same namespace stripping as the window lookup, so `anthropic/claude-sonnet-5-5` finds `claude-sonnet-5-5`) has the `name`, `description`, and `efforts` of models.dev. Any other listed id has no `name` and no `description`, and its `efforts` are every level up to `high`, as for a model that `Runtime.Models` does not hold. The `claude-code` aliases keep their facts. Harness makes no network call for such a list, also for an entry with `models_from`. A dated snapshot id is not listed when its undated alias is in the table, and a deprecated model or a model without tool calls is not listed; the window of such a model stays known, so a configured model of this kind runs. `efforts` holds the levels of `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, and `max` that the model supports: the models.dev `none` and `toggle` give `off`, the `anthropicThinkingCanBeDisabled` overrides add `off` for an Anthropic model that accepts an off thinking request but that models.dev lists without it (`claude-opus-5`, and `claude-sonnet-5-5`, whose entry sets the off thinking type `between_tools`), an `effort` control gives its values (a value that harness has no name for, such as `ultra`, is dropped), a `budget_tokens` control gives every level up to `high`, the `claude-code` aliases take every level, and a model with no reasoning has none. The table of a model also holds whether it has an `effort` control and a `budget_tokens` control |
| `internal/mcp`, `plugin`, `skill`, `command`, `process`, `imageclamp` | The MCP client and server roles, the plugin host (`internal/plugin`, with the JSON-RPC transport in `internal/plugin/wire`), skill discovery, the slash-command registry, the process manager, and image clamping |
| `internal/wirerecord` | The program that records the real wire streams of `testdata/wire` and scrubs them. It makes real model calls only with `HARNESS_RECORD_WIRE=1` and reads keys from the environment |
| `internal/wirescrub` | Replaces ids, tokens, emails, home and temporary paths, and socket and pid paths in a recording with stable placeholders, and lists what a recording still holds |
| `internal/acptck` | The program that runs the ACP Test Compatibility Kit against `harness acp` with a scripted model and judges the report against `testdata/acp-tck-allow.txt` |
| `internal/wasmsize` | The program that builds `cmd/harness-wasm` for `GOOS=js` and compares the module size with `testdata/wasm-size-budget.txt` |
| `internal/testtime` | The program that reads `go test -json`, prints the test log, and fails a test or package over `testdata/test-time-budget.txt` |
| `internal/gates` | The repository checks of "CI gates" |
| `internal/workspace/workspacetest` | The contract table of an `Engine`, `Run(t, Factory)`, and `Fake`, an engine over a directory with change IDs, no `.git`, and an operation counter that rises only when the files change. Every engine runs the table |
| `internal/testpoll` | Polling for cross-process waits in tests |

The `depguard` rules of `.golangci.yml` freeze the graph: `eventlog` imports `protocol` only, `turn` imports `backoff`, `eventlog`, and `protocol`, `internal/selftool` imports only `modelref`, `process`, `prompt`, `session`, `tool/proc`, `toolresult`, `turn`, and `protocol`, no package of `internal/backend` or `internal/tool` imports `session`, `tree`, or `internal/server`, only `internal/backend` builds a provider wire, only `internal/backend`, `internal/provider`, `internal/plugin`, `internal/prompt`, `internal/imageclamp`, and `internal/tool/pluginsrc` import `internal/message`, `internal/acp` imports the standard library, `harness`, `config`, `protocol`, and `internal/acp/rpc` only, `internal/acp/cwdruntimes` imports the standard library, `harness`, `config`, and `internal/acp` only, `internal/acp/acpauth` imports the standard library, `config`, `internal/acp`, `internal/claudeauth`, `internal/codexauth`, and `internal/process` only, `internal/acp/rpc` imports the standard library only, `internal/jshost` imports the standard library, `harness`, and `protocol` only, `config` imports the standard library only, the public `plugin` package imports the standard library, `protocol`, and `internal/plugin/wire` only, and no internal package imports `cmd`.

## eventlog

### Store

The store holds opaque records. It knows nothing about the event schema, so a disk or Postgres store stays small and never changes when an event kind is added.

```go
package harness

type Store interface {
	// Append writes records at expectedSeq+1.. or fails with ErrConflict.
	Append(ctx context.Context, session string, expectedSeq uint64, records ...[]byte) error
	Read(ctx context.Context, session string, afterSeq uint64, limit int) ([]Record, error)
	Head(ctx context.Context, session string) (uint64, error)
	Sessions(ctx context.Context, after string, limit int) ([]string, error)
	PutBlob(ctx context.Context, session, key string, r io.Reader) error
	GetBlob(ctx context.Context, session, key string) (io.ReadCloser, error)
}

type Record struct {
	Seq  uint64
	Data []byte
}

var ErrConflict = errors.New("harness: append conflict")
```

- A `Store` refuses what `store.CheckName` and `store.CheckRecords` report: an empty name, `.`, `..`, or a name with a slash, a backslash, or a NUL; and an empty record or a record with a newline. `DiskStore` and `MemStore` call them, and so do the runtime and `OpenView` for a session ID in a path. A store of another storage calls them too, so a name or record that one store accepts, every store accepts.
- `seq` starts at 1 and has no gaps. `Head` returns the last seq, or 0 for an empty session. `expectedSeq` is the last seq the writer has seen, so it equals the record count.
- A stale writer gets `ErrConflict` and stops. This holds across `Store` instances on one storage, in one process or in many: the fence is in the storage, not in an instance.
- Any other append error also stops the session actor, with no retry. The next `Open` fences and replays from the store, so an append that landed is in the replay, and one that did not land is not.
- Compare-and-append alone does not fence a lease handoff: an old owner's in-flight append can still hold the current `expectedSeq`. The new owner therefore appends `owner.acquired` before it replays or runs (see Ownership). Any later append from the old owner conflicts. A shared store may also check its lease in the same transaction as the append.
- `Append` is durable on return. `DiskStore` writes the records of one `Append` with one `fsync`. `MemStore` is for tests.
- `DiskStore` takes an exclusive `flock` of the session log for each append, from its first byte to its `fsync` or rollback, and compares `expectedSeq` with the head read under that lock. Each `Head` takes a shared `flock`, so it never reads bytes that a failed append cuts again, and scans again the bytes that another instance appended since the last scan. A wait for a lock ends with the context of the call. Where `flock` does not exist, only the instance fences its appends.
- On the browser host `DiskStore` runs on `globalThis.fs`, which `sdk/embed` implements in the Worker over the origin private file system (OPFS) below `/persist`. The work directory `/work` and the rest of the tree are in memory and do not survive a reload. `flock` does not exist there, so the host takes one Web Lock for the whole store, `harness:store:<origin>`, in exclusive mode, before it opens any file of the store. It waits up to 2 s for the lock, so a page that reloads while the old Worker still holds it gets it. The lock stays held until `close()` or the end of the Worker, and the host never steals it. A page that does not get the lock in that time fails with `store_in_use`, reads nothing, and can run with an in-memory store. A file of the store stays open through a sync access handle, which locks the file against every other handle, and descriptors of one path share it. A write puts the new bytes at their offset, and `fsync` is `flush()` of the handle: a Worker that ends after `fsync` returned loses none of the bytes written before it, and a Worker that ends during a write leaves a prefix of the bytes written since the last `fsync`, which `Open` reads as a torn tail. `flush()` is the durability that the browser gives and no more: the browser does not promise that the bytes survive a power loss. `rename` relies on the `move()` of the file handle, which replaces the destination; the File System standard does not say whether `move()` is atomic against a crash, so harness promises no atomic rename in a browser. A browser without `move()`, and a destination that a reader holds open, get a copy of the bytes and a remove, and a crash in between leaves the destination with a prefix of the bytes. A source that is open gives `EBUSY`, and so does `unlink` of an open file. Either way a record can name a blob that is missing, and `GetBlob` then returns `ErrBlobNotFound`. The host cannot rename a directory.
- There are no checkpoints. `Open` and `OpenView` replay the whole log. `ReadEvents` reads the events after a seq without a replay. `List` reads the view of a session that this runtime runs, and replays the log of any other session.
- `storetest.Run(t, newStore)` is the conformance suite. Every `Store` runs it. `storetest.RunInstances(t, newStorage)` checks the fence across instances: `newStorage` returns an opener of new storage, and each call of the opener returns one more instance over that storage.

### Layout on disk

```
<root>/<session>/log.jsonl          source of truth; line N holds seq N
<root>/<session>/blobs/<key>        backend state chunks; retained tool results; prompt attachments; images of tool results
```

The runtime reads no other on-disk format.

### Event envelope

```json
{"v":1,"seq":42,"t":"2026-10-02T12:00:00Z","k":"turn.ended","d":{}}
```

`protocol.Payload` is a Go interface with `Kind() string`, and `protocol` holds one struct for each durable kind. `protocol.Event.Payload` decodes the data of an event into a pointer to the struct of its kind, such as `*protocol.TurnEnded`. `eventlog` encodes and decodes the envelope with the same registry. An unknown kind fails the load loudly.

### Event kinds

The Go types of `protocol` own the payload of each kind: the field names, the JSON tags, and the typed enums (`Status`, `Delivery`, `StopReason`, `Cause`, `ErrorClass`, `ResolutionKind`, `GoalState`, `Verdict`, and `Outcome`). `protocol/openapi.json` and `protocol/protocol.ts` generate from them. `protocol.Kind*` names each kind. A golden test in `internal/eventlog` pins the JSON bytes of every kind.

| Kind | Payload type |
| --- | --- |
| `session.created` | `protocol.SessionCreated` |
| `owner.acquired` | `protocol.OwnerAcquired` |
| `settings.changed` | `protocol.SettingsChanged` |
| `input.admitted` | `protocol.InputAdmitted` |
| `input.promoted` | `protocol.InputPromoted` |
| `input.withdrawn` | `protocol.InputWithdrawn` |
| `turn.started` | `protocol.TurnStarted` |
| `item.completed` | `protocol.ItemCompleted`: its `message` has the role `user`, `assistant`, or `tool` |
| `turn.suspended` | `protocol.TurnSuspended` |
| `turn.resumed` | `protocol.TurnResumed` |
| `turn.ended` | `protocol.TurnEnded` |
| `request.opened` | `protocol.RequestOpened` |
| `request.resolved` | `protocol.RequestResolved` |
| `goal.set` | `protocol.GoalSet` |
| `goal.evaluated` | `protocol.GoalEvaluated` |
| `goal.changed` | `protocol.GoalChanged` |
| `compaction.applied` | `protocol.CompactionApplied` |
| `child.spawned` | `protocol.ChildSpawned` |
| `child.settled` | `protocol.ChildSettled` |
| `command.recorded` | `protocol.CommandRecorded` |
| `context.measured` | `protocol.ContextMeasured` |
| `backend.state` | `protocol.BackendState` |
| `tool_result.retained` | `protocol.ToolResultRetained` |

Ephemeral frames go to subscribers and never to the store: `item.started`, `item.delta`, `item.discarded`, `status`. Each has `ephemeral: true` and the last durable seq, and an item frame carries its `item_id`, so a client can place it.

### Apply

```go
func (s *State) Apply(r Record) error
```

`Apply` is the only code that changes durable state. Live code appends, then applies the same record. Replay applies the whole log from seq 1. There is no other fold: `Advance` runs the same fold of status, queue, goal, and last turn one event at a time.

```go
// Advance applies one durable event to p. It is the code that State.Apply uses for these fields.
func Advance(p protocol.Progress, e protocol.Event) (protocol.Progress, error)
```

`protocol.Progress` is the status, running turn, queued input IDs, open request IDs, goal, and last turn that a log folds to at `Seq`. It is plain JSON, so a reader stores it with a cursor and resumes with `harness.Advance` in place of a replay. `State.Apply` computes these fields with `Advance`, so a stored `Progress` and a replay reach the same value. `Advance` assumes a legal log: `Check` has enforced the transitions, and `Advance` only needs the event to follow `p.Seq`. An ephemeral frame, an unknown kind, and an event out of order fail, and `p` stays as it was.

- `State.History` holds the summary message of the newest compaction and the messages after it. The text of that message is the compaction banner `[compacted summary of earlier conversation]`, a blank line, the plain `summary` of the record, and, when the record has an `index`, a blank line and the `index`. Only `State.History` adds the banner and the index. The record, `State.Transcript`, the message page, the events, and ACP hold the plain summary. `client/session.messages` of a plugin reads `State.History`, so a plugin sees the banner and the index, as the model does. `State.ModelHistory` is `State.History` with each pinned segment (see Settle) at its place, and the model reads it.
- `State.Transcript` is what a reader sees. It keeps the messages that each compaction folded, with the summary of each compaction in its place as a divider between the messages it folded and the messages kept. The model never reads the folded messages again.
- The summary for `GET /sessions` is `State.Summary()`.

Usage is part of the fold:

- Each model call that measures anything appends one `context.measured` record with its `usage`, its context reading, and the `subscription_usage` that the provider reported with it.
- A summary call adds its `usage` to the `compaction.applied` record that it produces. A summary that fails appends a `context.measured` record with its `usage` alone. A goal evaluator call adds none.
- `State.Usage()` sums all of it, so a handoff, a crash, and a restart keep it.
- A record with a `source` sets the context reading, zero included, and a record with no `source` leaves it. A model API backend gives a `source` only to a call with prompt tokens.
- The newest `subscription_usage` is the one that the view shows. The actor stamps `captured_at` of a snapshot that has none.
- A backend that reports usage once for a turn, as Claude Code does in its `result` frame, records it once, with the subscription snapshot that its `rate_limit_event` frame reported. The `total_cost_usd` of that frame is `cost_usd` of the record. `State` sums `cost_usd` over the log, and the view shows the sum as `session_cost_usd` of `subscription_usage`, with provider `claude`, no windows, and the time of the view as `captured_at` when no `rate_limit_event` frame came. A session with no `cost_usd` record shows no sum.

### Invariants enforced at append

`eventlog.Check` applies a batch to a copy of the `State` before the actor appends it. A batch that breaks an invariant fails with `ErrIllegal` and is not appended.

- Every tool call item gets exactly one result item, or an open request, before its turn ends. When a turn ends with a tool call still open (completed, failed, or `provider_exhausted`), or a handoff suspends it with a call still open, the runtime gives that call a synthetic result that says the call did not finish. A stopped, goal-cleared, or crashed turn gives its open calls the results that its row names (see State machines). The result is an addition: the log never loses or changes a real tool result.
- `seq` is gap-free per session.
- A `turn.ended` follows every `turn.started`, except for a suspended turn.
- Only a suspended turn resumes. A turn that ended never resumes.

## session

### Actor

Each live session is one goroutine. It holds the `Ownership` and the `State`. Commands arrive through a mailbox as functions that run on the actor goroutine. The caller waits for the reply.

| Command | Effect |
| --- | --- |
| `Submit(input)` | Append `input.admitted`; start a turn, queue, or steer |
| `Interrupt(turnID?)` | Cancel the turn; reply when it has stopped |
| `Cancel()` | Append `input.withdrawn` for each queued input, then cancel the turn, in one step; reply when it has stopped |
| `Update(settings)` | Check the model; append `settings.changed`. A running turn takes the new model and settings at its next model call. When the new model has another kind of backend (one that owns its loop, or one that does not), the turn fails at its next model call, and the error says the model needs another kind of backend |
| `SetGoal(...)`, `StartGoal(...)`, `AdjustGoal(...)`, `ClearGoal()` | Append goal events |
| `Compact(keep)` | Run a compaction as the run of the actor; `keep` replaces `compaction_keep_turns` |
| `Spawn(child, agent)` | Append `child.spawned`; return the `session.created` of the child. A settled child spawns again before it gets an input from the `task` tool; a child that has not settled appends nothing |
| `Settle(outcome, report)` | Append `child.settled` and admit the report as an input with `source: child`; a settled child changes nothing |
| `Relay(report)` | Admit the report of a child that settles in another session as an input with `source: child`, delivered as `Settle` delivers it, and append no `child.settled`. The input ID names the child and its turn, so a report that the log already holds changes nothing |
| `Release()` | Suspend the turn with cause `handoff`; stop; release ownership |
| `End()` | Fail with `session_busy` while a turn or a control command runs; else stop a compaction or an evaluation that runs, as `Release` does, stop each live child, then stop and release ownership. A child that this walk stops settles `canceled` with no report, and the `turn.ended` of its turn has cause `ended`, so a parent that opens later drops only those reports. The log stays |
| `Record(command)` | Append `command.recorded`; a repeated input ID returns the newest status |
| `Withdraw(id)` | Append `input.withdrawn` if still queued; otherwise append nothing |
| `Resolve(requestID, resolution)` | Append `request.resolved`; an answer starts a turn with no input, and a dismissal starts none for the request (a dismissal starts the turn of the queued inputs, if any). The receipt `protocol.Resolved` holds the `seq` of `request.resolved` and the `status`: `started` for an answer, `dismissed` for a dismissal |

The actor appends with no other goroutine. It checks the batch with `eventlog.Check`, appends it with `Store.Append`, applies each record, and publishes a new view. A command that needs durability replies after `Apply`. The store write is the only wait on disk in the actor.

The turn runner is one goroutine per turn. It gets a `turn.Request` that the actor builds from the `State`, calls the backend and tools, and sends each item and the end of the turn to the actor as commands. It touches no session field.

Lifecycle:

- `session.Create` and `session.Open` append and replay, but start no goroutine. They record the first turn, the resumed turn, or the next queued input. `Actor.Run` then starts the actor goroutine, the `Sync` sender, and that run. The runtime publishes the session before it calls `Run`, so a tool of the first run, such as `task` or `goal`, finds its own session.
- When the actor stops, for any cause, it cancels its run, refuses every later command with `ErrNotOwned`, and waits until each turn, compaction, and evaluator goroutine has exited. Only then does it wait for `Sync`, release its `Ownership`, fix its cause, and close `Done`. A next owner therefore never runs beside a run of the earlier actor, such as an external harness in its grace after SIGINT.

The cause of a stop is the first of these. `Release` and a handoff by `Close` give `ErrSessionReleased`. `End` gives `ErrSessionEnded`. A `Lost` that closes, a store `ErrConflict`, and a stale epoch from `Sync` give `ErrOwnershipLost`. A `Close` that passes its deadline gives `ErrSessionReleased`. Each of the three matches `ErrSessionNotOwned`, so the code `session_not_owned` does not change. A store error or a Sync rejection that stops the actor wraps that error under `ErrSessionNotOwned`. `Session.Err` returns the cause once `Done` is closed, and `Events` waits for `Done` and then ends with it.

Reads use `atomic.Pointer[View]`. A `View` is immutable: the `protocol.Session` (status, turn, goal, queue, settings, usage, context gauge, last turn, compaction count, subscription usage, head seq) and whether the actor stopped.

- The gauge window is the window of the session model, or the window of the newest reading when the model reports none. A reading (`context.measured`) carries `window_estimated` with its window. `OpenView` has no backend and reads the window and the flag of the newest reading from the log; the runtime reads of a session that it does not run take the window of the session model from the backend, as a running session does.
- `Session.Snapshot`, `Runtime.List`, and a read of a session that the runtime does not run also fill `Plugins`: the name, state (`not-spawned`, `running`, or `errored`), tools, and hooks of each plugin of `Config.Plugins` and `Options.Plugins`. The plugin host owns that state, and the log does not hold it.
- The `View` also holds the profile of a child session and its unsettled children.
- A read of the whole `State`, such as the history for a plugin or the usage of a child, goes to the actor through `Actor.Read`, which runs a function on the actor goroutine. The runtime replays a log only for a session that it does not run.

### Ownership

```go
package harness

type Owner interface {
	// Acquire returns at once. For a held session it fails with an error that matches ErrSessionHeld.
	Acquire(ctx context.Context, session string) (Ownership, error)
}

type Ownership interface {
	Epoch() uint64 // recorded in owner.acquired and carried by every SyncBatch
	Lost() <-chan struct{} // closes when the grant ends without Release; nil means never lost
	Release(ctx context.Context) error // ends the grant; the runtime calls it once
}
```

`Create` appends `session.created` and `owner.acquired` to an empty log in one append. A conflict there fails with `ErrSessionExists`. A `Create` waits for an `Open` of the same ID that is in flight, and creates the session when that `Open` fails, so an `Open` that races a spawn never fails the spawn. An empty `model` is `Config.Model`, or `config.DefaultModel` when that is empty, after one alias lookup. An explicit model gets no alias lookup. Every session that the runtime creates, a child too, records the absolute, cleaned `WorkDir` of the runtime as `work_dir` in `session.created`; `protocol.Session.work_dir` shows it. It is empty without a `WorkDir`, and for a session whose log has no `work_dir`.

Start-up order of `Open` after `Acquire`:

1. Read `Head`.
2. Append `owner.acquired{epoch, owner}` at that head. On `ErrConflict`, read `Head` again and retry. An old owner's append that lands first is ordered before the fence.
3. Replay through the fence record, then run.

After step 2, every append from a previous owner conflicts. When `Lost` closes, the actor stops without another append; `ErrConflict` from the store has the same effect. The error of that stop says `harness: session not owned: another process took over session <id>`; it matches `ErrOwnershipLost`, `ErrSessionNotOwned`, and `ErrConflict`. Two processes on one session directory therefore fence each other: the later `Open` keeps the session, and the earlier run fails with that error. `ErrSessionHeld` wraps `ErrSessionNotOwned`, so the code `session_not_owned` does not change, and `CatchUp` skips a session whose `Acquire` fails with it. The default `Owner` is `NewLocalOwner(1)`: it grants every session to the local process, one grant at a time. An embedder supplies a lease, or passes `NewLocalOwner` with the epoch of its claim; `cmd/harness` reads the config key `owner_epoch` for that epoch. The actor calls `Release` once, after the last record is acknowledged, with a bound of 10 s. A failure is logged and `Session.Release` and `Runtime.Close` return it. A path that has no caller to return it to, such as a failed `Open`, only logs it.

On the browser host the owner is `NewLocalOwner` and the store-wide Web Lock extends its single grant to the origin (see Store).

### State machines

Session status derives from the turn and the open requests:

| Status | Condition |
| --- | --- |
| `idle` | No turn is running |
| `running` | A turn is running |
| `waiting` | A turn ended `awaiting_input`; a request is open |

`retrying` is not a session status. A turn that waits to call the model again sends an ephemeral `status` frame with `retrying`, `attempt`, and `next_at`, and the session stays `running`.

A compaction that the actor runs as its own run, automatic or from `Compact`, sends an ephemeral `status` frame with `compacting` when it starts, and one frame when it ends: `idle` when the summary is appended, `compaction_canceled` when an interrupt, or the end of an ancestor session, stopped the summary call, or `compaction_failed` when it is not appended for any other cause. Each frame carries the compaction ID in `turn_id`. The session stays `idle` during the run, and the frames are never stored. A compaction inside a turn sends no such frame, except a compaction of the Claude Code CLI: when the CLI reports that it compacts, the turn sends a `status` frame with `compacting`, then `running` when the CLI reports the compact boundary (before the summary is appended as `compaction.applied`), or `compaction_failed` when the CLI reports a compact result that is not a success or ends the turn with no compact boundary, and the actor decides the end by the stop cause: `compaction_canceled` when an interrupt or the end of an ancestor session stopped the turn, else `compaction_failed`. The session stays `running`, and these frames carry the turn ID in `turn_id`.

The actor has one run at a time. A run is a turn, a compaction, or a goal evaluation, and all three end through one path that answers the waiters of the run and then starts the next work. Status derives from the turn alone, so `idle` also holds while a compaction or an evaluation runs, and `running` always has a `turn_id`. A command acts on each kind of run like this:

| Command | Turn | Compaction | Evaluation |
| --- | --- | --- | --- |
| `Submit` | A `steer` input joins at the next item boundary; any other input waits | Waits | Waits |
| `Interrupt` | Stops the turn, and ends an active goal | Stops it, and ends an active goal | Stops it, and ends an active goal |
| `Compact` | `session_busy` | `session_busy` | `session_busy` |
| `ClearGoal` | Stops the turn with `goal_cleared` while the goal is active | The compaction continues | Stops it with `goal_cleared` |
| `Release` | Hands off the turn | Stops it; appends only the usage of the summary call, if any | Stops it and appends nothing |
| `End` | `session_busy` | Stops it; appends only the usage of the summary call, if any | Stops it and appends nothing |
| Typed control command | Refused unless `available_during_task` | Same | Same |

Turn:

```
running ─► completed | interrupted | failed | awaiting_input
running ─► suspended ─► running   (next owner resumes from the last completed item)
```

A turn ends early for one of six causes. A live owner carries the first four with `context.WithCancelCause`; `Open` detects `crashed` in the log; the turn loop reports `provider_exhausted`:

| Cause | Trigger | Effect |
| --- | --- | --- |
| `stopped` | User interrupt | Keep the partial; unfinished tool calls get `interrupted` results; the next queued input runs |
| `goal_cleared` | `ClearGoal` during a goal turn | Same as `stopped` |
| `ended` | `Runtime.End` of an ancestor stops the turn of a descendant | Same as `stopped`, and Recover settles the child `canceled` with no report input |
| `handoff` | `Session.Release`, `Runtime.Close` | Stop at an item boundary: admit no new tool call, let running tools finish within the budget, append `turn.suspended`. A delegated backend (`OwnsLoop`) cannot stop at an item boundary, so a handoff interrupts it, records every item that it already wrote, gives each open tool call a cut-off result, and appends `turn.suspended`. A suspended turn has no open tool call, so the next owner resumes it automatically |
| `provider_exhausted` | A usage limit of the provider: a spent quota, credit balance, or spend cap | Append `turn.ended{failed, provider_exhausted}`; keep the partial; queued inputs wait for the next input |
| `crashed` | `Open` finds `turn.started` with no end or suspend (forced stop, OOM, an exceeded handoff budget) | Append `turn.ended{interrupted, crashed}`; keep the partial; each open tool call gets a result saying it was cut off and to check whether it took effect before running it again; an assistant item, `[harness: this turn was interrupted by a process restart and could not complete]`, closes the turn before `turn.ended`, so the next user message does not join it on the wire. The session then starts the next queued input, or waits for input when none is queued |

The `cancel` action of the `task` tool stops a turn as `stopped` does, and adds `task_cancel` to the `turn.ended` of the turn. Cause stays `stopped` on the wire; Recover reads the mark to route the report again.

`turn.ended` types the cause in `cause`: `stopped`, `ended`, `goal_cleared`, `crashed`, or `provider_exhausted` (`handoff` is the cause of `turn.suspended`). `error` holds only the masked, capped message of a failure; a turn that ended by its cause alone has no `error`. A `provider_exhausted` turn keeps the provider message in `error` and the provider `RecoverHint` in `recover_hint`. Any other failed turn types its error in `error_class`: `permanent`, `timed_out`, `rate_limited` (a rate limit that outlasted the retries), the class name of another retryable error (`overloaded`, `server_error`, `stream_truncated`, `content_filter`, `connection_failed`), `login_required` (the login of the user is missing or rejected, the server of an injected entry answered 401, and only a new login resolves it; a Claude Code turn that fails while `claude auth status --json` reports `loggedIn` false has it on every host, with the error `not logged in to Claude Code; run `harness login claude-code``, and the status is asked after a failure only, with the environment of the turn, so the config directory of a session mirror is the one that is asked), or `unrecovered`. Every reader branches on `stop_reason` and `cause` and never parses the text of `error`.

After any other failed turn, the next queued input runs, as after a completed turn. Only `provider_exhausted` leaves the queue waiting, and `Open` follows the same rule: it starts the next queued input when no turn is open, except after a turn that ended `provider_exhausted`.

No tool call is ever re-run after a stop. The log stays strictly append-only, and a client hides output by cause if it wants to.

Input:

```
admitted ─► promoted   (queue: next turn; steer: next item boundary)
admitted ─► withdrawn
```

A `steer` input that joins a running turn reaches the model as one user message: `OPERATOR MESSAGES (address these, then continue the task):` and a numbered list of the inputs that join together. A child report is not in that list: the model reads it in the `[tasks: …]` segment (see Children). A backend that owns the loop takes no child report in the middle of a turn: on Claude Code the report waits in the queue, and the turn that starts next takes every queued report with its first input. When a prompt or another input starts that turn, the segment follows the text of that input after a blank line. When only reports start the turn, the segment follows the trigger sentence, `A background task you started has finished. See the engine context below for its result, and continue accordingly.`, after a blank line, so the result reaches the model once. A prompt that arrives while a turn runs joins the turn on both kinds of backend. A `steer` input with `expected_turn_id` fails with `turn_mismatch` if that turn is not running. A `steer` input on an idle session starts a turn. An input with no `delivery` is `steer`.

Goal:

```
active ─► paused ─► active
   ├───► blocked ─► active
   └───► achieved | failed | exhausted
any ─► cleared
```

- `SetGoal` on a session with no running turn and no queued input (or only queued inputs that wait behind an open request) admits the condition as an input with `source: goal`, which dismisses an open request and starts a turn through the normal input events. `SetGoal` while a turn runs, or with queued input, starts nothing; the next turn that ends is the first one evaluated. When nothing drives the goal, it is `blocked` (see below).
- A new `SetGoal` replaces the goal and resets its turn count. `SetGoal`, `ClearGoal`, each verdict, and each pause or failure withdraw the queued inputs with `source: goal`.
- `goal_evaluator_model` (resolved through `aliases`) names the evaluator model; `SetGoal` without it is an invalid request. After each turn that completes or is interrupted while the goal stays active, the evaluator runs as the run of the actor, as a compaction does. It reads the condition and the history, and returns `met`, `not_met` with guidance, or `impossible`. `met` yields `achieved`, and `impossible` yields `failed`. Guidance is an input with `source: goal`. A reply with no verdict is `not_met`, and the reply is the guidance. The evaluator transcript renders each part as a bracketed line, cuts a part at 4096 bytes, and drops the oldest messages beyond a budget of half the evaluator window less 2048 tokens (never less than 2048, at 4 bytes for each token). The evaluator request pins effort `off` and a 256-token cap, and a summary call caps its response at 1024 tokens.
- A turn or an evaluation that fails on a retryable error or a usage limit yields `paused` with `retry_at`. The wait starts at 30 s and doubles with each pause before the next verdict, up to 30 min. `GoalView.pauses` reports the pauses since the last verdict, and each verdict resets it to 0. `GoalView.verdict` holds the last verdict of the evaluator, and `goal.set` clears it. A goal that has paused 6 times since its last verdict and fails again yields `failed` with reason `retries_exhausted`. At `retry_at`, the goal becomes `active` and judges the last turn again after an evaluator error, or admits an input that continues the goal. Any input resumes the goal at once. An error the user must fix yields `failed`.
- A goal is `blocked` when it is `active` and nothing drives it: no run, no open turn, no open request, no queued input that starts, and no ended turn that the goal has not judged. A blocked goal has no `retry_at`, and its reason is `no_driver`. One check moves an undriven goal to `blocked`: it runs at the end of `Open`, after each run settles, after `SetGoal` when the queue is held, after a dismissed request, and after `Cancel` withdraws the queue of an idle session. The check starts no turn and calls no model. A paused goal waits for `retry_at` or an input; a blocked goal waits for an input, `SetGoal`, or `AdjustGoal`. Any input, `SetGoal`, or `AdjustGoal` makes a blocked goal `active`, and the goal judges the turn that follows. A blocked goal does not count in `pauses`, and it does not make a session resumable.
- `max_turns` bounds goal turns; 0 is unlimited. Reaching it yields `exhausted`. `goal.set` carries `turns`, the count that the goal starts at: 0 for a new goal, and the current count for an adjust.
- With `goal_evaluator_model`, the runtime adds the `goal` tool to each session of a backend that does not own the loop; a child session has none. `status` reports whether a goal is active, paused, or blocked and its condition. `set` calls `StartGoal`: `SetGoal` with no turn limit, which fails while a goal is active, paused, or blocked, and admits the condition as an input with `source: goal` even while a turn runs, so the turn that calls `set` is not judged. `adjust` calls `AdjustGoal`: it replaces the condition of the active, paused, or blocked goal, keeps `max_turns` and the turn count, and does nothing for the same condition, so the model cannot extend its own turn limit. A turn that calls `adjust` is judged on the new condition with no spent turn, and a condition that `set` posted and that still waits is replaced by the adjusted one. There is no `clear` action; clearing stays with the operator.
- `ClearGoal` during a goal turn or its evaluation stops it with cause `goal_cleared` and returns after it ends. When the actor then runs nothing, `ClearGoal` starts the next queued input.
- `CancelForEnd`, which the end of an ancestor calls, ends every live goal (active, paused, or blocked) of the session whether the session runs a turn, an evaluation, or a compaction, or is idle, and stops the retry timer of the goal: the goal becomes `cleared` with reason `ended`, and the queued inputs with `source: goal` are withdrawn, so an ended session starts no goal turn.
- An interrupt while the goal is `active` ends the goal, during a goal turn, an evaluation, or a compaction: the goal becomes `cleared` with reason `interrupted`, the queued inputs with `source: goal` are withdrawn, and the run stops, so no goal turn starts. The next queued input then runs.
- The goal lives in the log. `Open` restores it with its turn count. An `active` goal on an idle session judges the last turn when the goal has not judged it, and a `paused` goal keeps its retry time. An `active` goal that nothing drives becomes `blocked`, and a later `Open` appends nothing for it.

Request:

```
pending ─► answered | dismissed
```

A backend opens a request with `Sink.Ask` on an open tool call of its turn, with the call ID as the request ID. The turn then ends `awaiting_input`, keeps the call open, and the session reads `waiting`. `Resolve` closes the request. A dismissal closes the call with an error result that says the user dismissed the question. An answer leaves the call open, and the turn that it starts records the result. That turn has no input; a dismissal starts none. A question takes an answer that maps each question to text. An input from a person dismisses an open request first. An input is from a person when its `source` is `typed`, `command` (a typed slash command that expanded from `commands_dirs`), `user`, or empty (the admission step stores empty as `user`, so a client that sends `user` is also a person); this set has one owner, `fromPerson` in `internal/session`. An input of any other source (`parent`, `child`, `goal`, or a name that the client gives, such as `github`, `api`, or `box`) does not dismiss the request: the session admits it and keeps it in the queue, in `queued` of the session view and of `protocol.Progress`, and starts no turn from it while the request is open. The session stays `waiting`. When the request resolves, the held inputs follow their normal delivery: after an answer, a `steer` input may join the answer turn at its next item boundary, and a `queue` input waits for the next turn, which starts when the answer turn ends; after a dismissal (the `Resolve` dismissal, a settings change to a model of another provider, or an input from a person), the next turn starts at once. An input from a person that arrives while such inputs wait dismisses the request and starts a turn from the queue. A queued input from a person that starts after the turn dismisses the request first. A compaction that a person asks for dismisses an open request. A settings change to a model of another provider dismisses it too. A session of any backend asks with the runtime tool `ask_user_question` when `Options.AskUserQuestion` is set and the session is not a child and has no active goal (see Tools). Its call opens a request of the kind `question` with the tool input as the payload, and the turn that an answer starts records the answers as the result of the call.

### Compaction

Compaction runs as the run of the actor, never beside a turn.

- A turn starts at a user message. A compaction folds every turn before the newest `compaction_keep_turns` (default 2). It never folds only the previous summary.
- `to_seq` is the seq before the first kept message. The `input.admitted` record of a kept or queued input can have a lower seq, so a reader that hides `from_seq` through `to_seq` keeps each `input.admitted` record.
- A backend without `OwnsContext` summarizes the folded messages with the session model. The actor appends `compaction.applied` with `by_backend: false`.
- A backend with `OwnsContext` runs `/compact` as a turn. The backend logs `compaction.applied` with `by_backend: true`.
- `Compact()` returns `protocol.Compacted`: the `from_seq` and `to_seq` of the new `compaction.applied`, `by_backend` for a backend with `OwnsContext`, and `folded`, which is false when the session has too few turns to fold and nothing else is set. It fails with `session_busy` while a turn runs or inputs wait. Its `keep_turns` replaces `compaction_keep_turns` for one call. A `keep_turns` below 1, or any `keep_turns` for a backend with `OwnsContext`, is `invalid_request`.
- Before a queued input starts a turn, the actor compacts first when the newest `context.measured` reading is at or above `compaction_threshold` (default 0.8) of the window of the session model, or of the window of the reading when the model reports none. A setting at or below 0 is the default. A model call with no prompt tokens records no reading.
- When a turn has ended and the newest turn recorded no reading, the actor uses an estimate of the history from the newest compaction in place of the older reading: 1 token for each 4 bytes of text, tool call, tool result, and reasoning, and 1600 tokens for each image. The estimate is not recorded and changes no view. After an automatic compaction applies, or its summary comes back empty, the actor starts no other automatic compaction until the context (reading or estimate) falls below the threshold once, because the pressure then sits in the turns that the compaction kept. A change of the session model that moves the context window ends the wait. A summary that fails for another reason sets no such wait, `Compact()` neither checks it nor sets it, and a new owner starts without it.
- A failed summary appends no `compaction.applied`, only a `context.measured` record with its usage when it has any, and the turn starts on the full history. A handoff stops the summary the same way.
- A model call that overflows the context window compacts while its turn runs, for a backend without `OwnsContext`, and the turn calls the model again on the new history. When no turn can fold or the summary fails, the turn fails. With no new input in the turn, a second overflow fails it.
- `Open` starts the next queued input when no turn is open, unless the last turn ended `provider_exhausted`. The next owner thus runs the input that waited for a stopped summary, and compacts first when the reading still passes the threshold.
- The `summary` of a `compaction.applied` record is the plain text of the summary, with no banner and no framing. Its `index` field lists the newest 32 retained results (see Tool-result retention), and is omitted when none exist. A backend with `OwnsContext` logs its own summary text in `summary`.

### Children

A child is a session with `parent_id`. The `task` tool starts it in the background, and the runtime adds the tool only with a `WorkDir`. The parent holds the child ID, not the child's state. `protocol.Session.agent` is the agent profile name that the child was spawned with, from `agent` of its `session.created`; a root session has none.

- Spawn. The tool reads the agent profile (default `general-purpose`) and checks the tree limits. The parent actor appends `child.spawned`. Then the runtime creates the child in one append: `session.created` with `parent_id`, `agent`, the service tier of the parent, the effort of the call, the model of the call, else of the profile, else of the parent, and the allowed tools of the parent narrowed by the profile; the task as an input with `source: parent`; and the `turn.started` of that input. An effort that is not `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max` fails the call, and so does a model that is not `provider/model` (an alias is not looked up) or that no provider of the registry serves. The tool returns the child ID at once. A child that fails to start settles `failed` with no input, and the tool call fails.
- Settle. When a turn of a child ends, the child reports to its parent: `done` for a completed turn, `failed` for a failed or crashed turn, `canceled` for a stopped turn. A completed turn with a queued input does not report; the next turn reports. The report names the child, its agent, the outcome, and the reason, and holds the last assistant text of the child. The parent appends `child.settled`, with the child turn ID as `result_ref`, and the report as an input with `source: child`, in one append. A child settles once, so a later report changes nothing until a `send` of the `task` tool spawns it again. The runtime opens a parent that it does not run, so a later input to a settled child opens its parent again.
  - Delivery. An idle parent starts a turn with the trigger sentence (see State machines, Input), and the segment follows it, so the text of the child reaches the model once, in the segment. A busy parent takes the report by its backend. On a model API backend the report is a `steer` input at the next item boundary. On a backend with `OwnsLoop`, the report is a `queue` input, and the next turn that starts reads it with every other report that waits. The choice reads the capability and never a backend name.
  - Segment. The model reads the report as a message of its own, not under the `OPERATOR MESSAGES` heading: `[tasks:`, a line `<child> (agent=<agent>) done: <text> (usage: <in> in / <out> out)` for each report that joins together, then `]`, in `<harness-engine-context>` tags on a model API backend. On Claude Code a turn that another input starts reads the segment after the text of that input (`\n\n[tasks: …]`).
  - Pins. The segment is pinned: the model reads it at the same place in each later model call, and it is in no history that a reader sees (`View.Messages`, the `log` of `task`, `get_conversation_history`, `client/session.messages`). Replay rebuilds a pin from the log, so a pin survives a restart. A compaction moves every pin to the end of the history that it leaves, so a pin never falls between a tool call and its result. A compaction inside a turn settles the pins there at once. A compaction outside a turn leaves them at the end for the model, and the next turn start settles them after the inputs of that turn.
  - Reason. A failed child reads `failed: <reason>`, and a stopped turn reads `failed: canceled`. The reason is a fixed prefix by `error_class` (`permanent`, `login_required`, `unrecovered`, another retryable class, `provider_exhausted`, `rate_limited`) followed by the error text; `timed_out` reads `timed out` alone, an error with no class reads as it is, a turn with no `error` reads its `cause`, and a crashed turn reads `lost to restart: turn was in flight when the process last stopped`. The error text is the `error_detail` of the turn end, or its `error` when there is none, masked and cut at 500 runes with `… [truncated]`; the recover hint is masked and cut at 120 runes. The `status` and `log` actions of the `task` tool show this reason. A `login_required` reason reads `turn failed because the login of the provider is missing or was rejected; only the user can sign in again`, then the error text. For `provider_exhausted` and `rate_limited` the line ends with guidance not to spawn a replacement and to resume the child with `task send` after the recover hint.
  - Result size. A result of at most 4096 bytes, after masking, stays whole in the report. A larger result is masked, and the report holds its first 4096 bytes and ` … [full result retained — read the rest with read_tool_result(handle="trh_N")]`: the parent retains the masked result as `tool_result.retained` in the append of `child.settled` (see Tool-result retention). When the parent cannot read the handle back (its allowed tools omit `read_tool_result`, the retained total would pass the budget, or the blob write fails), the preview ends `… [truncated; full result unavailable]`. A runtime with no retention cuts the result at 4000 runes with `… [truncated]`. A newline in the text becomes a space. The `usage` is the total of the child.
- Crash and handoff. Each session follows its own rules. A child that a handoff suspended resumes when it opens, and a crashed child turn ends `crashed` and settles `failed`. When a parent opens, it settles each unsettled child that has ended, because a crash can come between the `turn.ended` of the child and the `child.settled` of the parent. A child whose last turn the end of a session stopped (cause `ended`) settles `canceled` with no report input; every other child that has ended settles with its outcome and its report, except a child whose routed report is being delivered. A child whose last turn a task cancel stopped (`task_cancel` on its `turn.ended`) reports to the nearest ancestor that has not ended its work, else to the root, read from the `session.created` records of the ancestors. When that ancestor is not the opening parent, the parent relays the report to it and settles the child with no report input; a relay that fails leaves the child unsettled for the next open, and the receiver of a relay keeps one report for each child and turn. No tool call runs again.
- Limits. One `internal/tree.Tree` holds `max_task_depth` (default 3) and `max_concurrent_tasks` (default 20) for each root. A negative value fails `Validate`. The runtime reads the root and the depth of a session once, when it loads the session: from its parent when this runtime runs the parent, else from the `session.created` records of the ancestors. The count is the unsettled children in the views of the sessions of the tree that this runtime runs, so a child that a parent opens after a restart counts at once. No ledger holds the count apart from the logs. A lock for each tree makes the count and the append of `child.spawned` one step. `max_tree_tokens` (default 0, no limit) is the token budget of a tree: a spawn fails once the root and its descendants have used that many tokens, input, output, and cache summed, read from the usage in each log of the tree. A session at `max_task_depth` has no `task` tool. A spawn past `max_concurrent_tasks` or `max_tree_tokens` fails the tool call with `engine: tree concurrency limit reached` or `engine: task tree token budget exceeded`, and a spawn that a race lets past the depth fails with `engine: task depth limit reached`. `HARNESS_MAX_TASK_DEPTH`, `HARNESS_MAX_CONCURRENT_TASKS`, and `HARNESS_MAX_TREE_TOKENS` set the keys through `ApplyEnv`.
- Actions. `action` defaults to `spawn`. `cancel`, `status`, `send`, and `log` take a `session_id` that the caller spawned, directly or through its own children; the tool walks the parents of the target to check it.
  - `status` reads the log of the target: its status (`running` until its last turn ends, then its outcome), parent, depth, children, agent, final text, failure, and usage.
  - `log` returns the newest `tail` messages (default 20, at most 100) as text entries, each cut at 2000 runes, in order, keeping the newest entries that fit in 20000 runes (always the newest one).
  - `send` admits the text as a steer input with `source: parent`, so a running child takes it at its next item boundary and an idle child starts a turn. A report of the child that is in flight, from the end of its turn until its parent records it, settles before `send` decides. When the parent has settled the child, the parent appends `child.spawned` again first, so the child reports the new turn.
  - `cancel` withdraws the queued inputs of the target and of each of its descendants and stops their turns. The report of the target and of each canceled descendant goes to the nearest ancestor of the target that is not terminal (its last turn has not ended with an outcome), found when the report is delivered and again after a restart. So the report of the target goes to its parent when that parent works, and to the session that called `cancel` when the parents between are done. A canceled descendant settles in its own parent with no report input. The `turn.ended` of each turn that `cancel` stops carries `task_cancel` and cause `stopped`.
- Tree interrupt. `interrupt {tree}` stops the turn of the session, then withdraws the queued inputs and stops the turn of each descendant that this runtime runs (`Cancel`). Before it stops a session, the walk silences the children of that session, so a descendant that ends during the walk settles with no report input. The walk outlives the request context. A tree interrupt opens a parent that this runtime does not run and settles the child there with no report input. `DELETE /sessions/{id}` differs: it stops each descendant turn with cause `ended` and opens no parent; the child settles, with no report input, when the parent opens.

### Shutdown

One `sync.WaitGroup` per runtime tracks every goroutine that the runtime starts. `Runtime.Close` is idempotent: the first call shuts down, and each later call waits for that result, bounded by its own ctx. It releases each session with cause `handoff` and waits for the group. It then closes the model connections, stops the processes, stops the plugins, and closes the MCP servers, once. Stopping the plugins closes each in-process plugin pipe and waits for its serve loop and the calls in flight, bounded by the ctx of the call. Each call returns by the end of its ctx with `context.Cause(ctx)`. When the ctx of the first call ends first, it stops the remaining sessions without an append, which cancels their turns, kills the processes, and returns without waiting for the group: a tool that ignores its ctx keeps its goroutine, and its session keeps its `Ownership` until the tool returns. A lease expires, and `DiskStore` fences on the next `Open`, which finds a crashed turn. A turn ends when its tools and its backend return: a tool gets the canceled context, and an external harness gets SIGINT and is killed after its grace (5 s for Claude Code). New work that `Close` would wait for fails with `ErrDraining` after `Close` starts; work that already runs finishes or is canceled.

## HTTP

### Routes

```
POST   /sessions                              create; client id optional
GET    /sessions?after=&limit=                list in creation order
GET    /sessions/{id}                         view
PATCH  /sessions/{id}                         model, effort, service_tier
DELETE /sessions/{id}                         end; 204; the log stays
POST   /sessions/{id}/inputs                  submit
GET    /sessions/{id}/inputs                  queued inputs, each with its parts
DELETE /sessions/{id}/inputs/{input}          withdraw
POST   /sessions/{id}/interrupt               {turn_id?, tree?}
POST   /sessions/{id}/compact                  {keep_turns?}; 200 with {from_seq, to_seq, by_backend, folded}
PUT    /sessions/{id}/goal                    {condition, max_turns}; 200 with the view
DELETE /sessions/{id}/goal                    204
POST   /sessions/{id}/requests/{request}      {answer} | {dismiss}; an answer replies 202 {seq, status}, a dismissal 204
GET    /sessions/{id}/events?after=&limit=    page; SSE with Accept: text/event-stream
GET    /sessions/{id}/messages?before=&limit= page of the conversation, numbered from 1
GET    /sessions/{id}/blobs/{key}             bytes of an attachment of the session
GET    /models                                models and their capabilities
GET    /commands                              slash commands
GET    /processes · POST /processes/{name}/{action}
GET    /processes/{name}/logs?tail=           last log lines and status
GET    /workspace/changes                     working-tree diff
GET    /health
```

There is no version prefix: harness and its clients change together.

A route that only reads a session, `GET /sessions/{id}`, `GET /sessions/{id}/inputs`, `GET /sessions/{id}/messages`, `GET /sessions/{id}/blobs/{key}`, and the page form of `GET /sessions/{id}/events`, answers from the view of a session that this runtime runs, and replays the log of any other session as `OpenView` does. `ReadEvents` is the read-only form of the events page for an embedder with a `Store` and no runtime: it reads the events after a seq with no replay. It never opens a session, so it appends nothing and starts no turn. `Runtime.Open` runs for each route that changes a session and for the SSE stream of its events, which tails them as they happen.

`DELETE /sessions/{id}` calls `Runtime.End`, which opens nothing: it stops a session that this runtime runs when no run is on, and stops the turn of each descendant that this runtime runs, as the tree interrupt does, and each of those turns ends with cause `ended`. A session that this runtime does not run stays closed, also when a descendant of it settles, and the child settles when the session opens, with no report input for a turn that ended with cause `ended`. A child that a direct interrupt stops still reports `canceled` to its parent; a tree interrupt keeps its rule of no report input. It answers 204 for any session that has a log, `session_busy` while a turn or a control command runs, and 404 for an ID with no log.

A restart opens no session by itself: the embedder opens each session that has work to resume (`View.Resumable`), `cmd/harness` when it starts and a control plane when it wakes the box. A session that has no work stays closed and gets no `owner.acquired`; a log that does not replay is skipped and logged.

`httpapi.ReadHandler(store, opts)` is the HTTP form of those reads for an embedder with a `Store` and no runtime. It serves `GET /sessions`, `GET /sessions/{id}`, `GET /sessions/{id}/inputs`, `GET /sessions/{id}/messages`, `GET /sessions/{id}/events`, and `GET /sessions/{id}/blobs/{key}` through the code of `httpapi.Handler`, so bodies, limits, paging, and error bodies are those of that handler for a session that the runtime does not run. The SSE form sends the durable frames after the cursor, each with its `id`, and ends at the head, because no owner appends. A session lists no plugins. It appends nothing, opens nothing, and starts no turn, and every route that changes something answers 405 in the error body. `opts` is a `httpapi.ReadOptions{Queue, Logger}`. `opts.Queue` is an `httpapi.Queue`, one method `Queued(ctx, session) ([]protocol.QueuedInput, error)`, for inputs that wait outside the log, such as the undelivered inputs of a box that is not running. The inputs of the queue follow the queued inputs of the log in `GET /sessions/{id}/inputs`, and their IDs follow those of the log in `Session.queued` of `GET /sessions/{id}` and of each session of `GET /sessions`. A session with no log but with waiting inputs answers `GET /sessions/{id}/inputs` from the queue alone, for a box that is still starting; every other route answers `session_not_found` for it, and `GET /sessions` does not list it. The handler asks the queue only for the session, the session list, and the inputs; a queue error never fails the messages, events, or blob routes. A nil `opts.Queue` lists the log only. `opts.Logger` receives the cause of each error that the handler masks as `internal` (nil: `slog.Default()`), and `httpapi.Handler` logs those causes to the `Options.Logger` of the runtime. `httpapi.Handler` takes no queue.

`httpapi.Handler` serves every route above. A route that answers with more than one success status has one entry for each status in the route table: `POST /sessions/{id}/inputs` answers 201 for a new input and 200 for a repeat, with the same body, and `POST /sessions/{id}/requests/{request}` answers 202 with `{seq, status}` for an answer and 204 for a dismissal. `GET /health` answers `protocol.Health`: `status`, `version`, `vcs_revision`, `vcs_time`, `started_at` (the start of the runtime, RFC 3339 UTC), and `capabilities` (`delta_row_identity`, which says that each `item.delta` frame names its item). The handler has no authentication; the embedder wraps it.

### Inputs

Request: `{id, parts, delivery?, source?, source_id?, source_label?, expected_turn_id?}`. The client mints `id`. `source` says who sent the input; `source_id` names the instance of that source (a Slack thread, a schedule) in at most 128 printable ASCII bytes, and `source_label` is a name for a reader, valid UTF-8, cut at 256 bytes with control, bidi override, and zero-width characters dropped. A `source_id` that is longer or holds another byte is `invalid_request` and leaves no trace, for a typed slash command too. Both are part of the body that a repeat compares, and `input.admitted` records them. `delivery` is `steer` when the request has none: a prompt that arrives while a turn runs joins that turn at its next item boundary. A `queue` input waits for the next turn. The runtime admits an internal goal input and `/compact` as `queue`.

A part is `{type: "text", text}` or an attachment `{type: "blob", media_type, data}` with its bytes in `data` as base64. Attachments are png, jpeg, gif, webp, and pdf. An attachment is at most 20 MiB, its bytes must be the type that it claims (an image decodes as that image, a PDF begins with `%PDF-`), and a request body is at most 32 MiB (8 MiB for any other route). A part that fails one check is `invalid_request` and leaves no trace. The message of that error is the reason alone, and `details.input_id` is the ID of the input. An image attachment for a session whose model the model facts of `modelmeta` list without image input (the models.dev input modalities) is also `invalid_request`, and its message names the model; a model that the facts lack passes. The check reads the model at admission and runs after the checks of the part, so a bad attachment reports its own error; a later change of the model is not checked. The session, not the route, makes this check, so every caller gets it. The runtime stores the bytes with `Store.PutBlob` under `attachment-<sha256>` before it appends `input.admitted`, whose blob part holds `media_type`, `blob_key`, and `bytes` and never the bytes. `Sync` carries the blob in the batch of that record, as it carries `backend.state`. A turn reads a blob through `turn.Request.Blob`. `modelapi` sends it as an image or file part of the wire, and `claudecode` sends an `image` or `document` content block on stdin after the text block. A steer input keeps its blob parts. The blob is stored before the input is appended, so an input that then fails (a conflict, a turn mismatch, or no ownership) leaves its blob in the store; a retry with the same bytes reuses it.

| Case | Response |
| --- | --- |
| New id | `201 {input_id, seq}` |
| Same id, same body | `200` with the original receipt |
| Same id, other body | `409 input_conflict` |

`Session.Prompt` admits an input in the Go API and returns a `Turn`. A `Turn` is a view over the log: it adds no event kind and holds no goroutine. `Turn.Events` yields the events from the `input.admitted` record of the receipt (the first `command.recorded` of a typed command) through the end of the input: the `turn.ended` of the turn that took it (the turn named by `input.promoted` or by `turn.started.input_ids`), its `input.withdrawn`, or the first `command.recorded` with a status other than `accepted`. `Turn.Wait` folds the same events into a `TurnResult`: the stop reason, cause, masked error and its class from `turn.ended`; the sum of the `usage` of the `context.measured` and `compaction.applied` records from the start of the turn, or from the admission when the input joins a running turn; the command status; and `Withdrawn`. When the session stops first, both end with `Session.Err`. `Turn.Steer` admits a steer input with `expected_turn_id` set to the turn that took the input, so it fails with `turn_mismatch` while no turn runs the input, also while the input waits. `Turn.Cancel` withdraws a queued input, or interrupts the turn that took it, and is no error for an input that is withdrawn, a turn that has ended, or a command. `Session.Turn(receipt)` returns the `Turn` of an earlier receipt, for example on a session that `Open` returned on a new runtime. The HTTP API has no `Turn`: a client tails `GET /sessions/{id}/events` from the `seq` of the receipt.

`protocol.Resolution.Validate` reports a resolution that holds both an answer and a dismissal, or neither, or an answer that is not valid JSON. An answer of `null`, `{}`, or `""` counts as none. `Session.Resolve` calls it and fails with `ErrInvalidRequest`; a remote client calls it before it posts.

A typed slash command answers the same way. Its receipt adds `command`, the newest status of the command, and its `seq` is the first `command.recorded` record. See Slash commands.

`GET /sessions/{id}/inputs` answers each input that no turn has taken, oldest first, as `protocol.QueuedInput`: `{id, parts, delivery, source, source_id, source_label}`. A text part holds its text. An attachment is `{type: "blob", media_type, bytes, key}` and never its data. `Session.queued` stays the list of those IDs.

`GET /sessions/{id}/blobs/{key}` answers the bytes of an attachment with the media type of its part. A blob part of `protocol.MessagePage` and of a queued input carries `key`, the `blob_key` of its `input.admitted`, and a client reads the image from this route. The key must be the `blob_key` of a blob part of an `input.admitted` record of that session. Any other key answers 404 `blob_not_found`: the `backend.state` blob, a retained tool result, an image of a tool result, a key of another session, an unknown key, or a key whose bytes the `Store` lacks. The response carries `Content-Length`, the size that the part records, so a client detects a body that a store failure cut short.

### Events

- One per-session `seq` serves paging and SSE resume.
- Only SSE frames for durable records carry `id: <seq>`. `Last-Event-ID` and `after=` resume exactly, across processes.
- `protocol.DecodeSSE(r)` is the one reader of the events stream for Go clients. It yields each frame as an `Event`, and an `error` frame as a `*protocol.Error` that ends the sequence.
- A live frame (`item.started`, `item.delta`, `item.discarded`, `status`) sets `protocol.Event.Ephemeral` and is never stored. Its `seq` is the last durable seq when it was sent.
- A subscriber that starts while an item streams, with a cursor that is not past the `item.started` frame of that item, first receives that frame and the `item.delta` frames sent so far, each after the durable records up to its `seq`, and then every later frame. A replayed item always begins with its `item.started`, so a client resets that item on `item.started`. A subscriber whose cursor is past the start of the item gets no replay of it. The session keeps the deltas of the item up to 256 KiB of data, so the replay holds a prefix of the deltas. The kept frames end with the `item.completed` or `item.discarded` of the item, with the end of its turn, or when the session stops.
- A subscriber reads durable records from the log, so a slow subscriber never misses one. A full subscriber drops ephemeral frames, so the deltas of an item can have holes; its `item.completed` holds the whole item. An error ends a stream with an `error` frame.
- Replication is `Options.Sync`: each `protocol.SyncBatch{epoch, session, from_seq, records, blobs}` is a remote append. The blobs of a batch are those that its records name: the chunk of a `backend.state` record, the blob of `tool_result.retained`, the blob parts of `input.admitted`, and the blobs of the tool results in `item.completed`. The receiver rejects an older epoch. A batch with `from_seq` at its head plus one is appended. A batch whose records are all at or below its head is a retry: identical bytes are acknowledged as a duplicate, and different bytes are rejected. Any other `from_seq` is a seq mismatch. Every reply is a `protocol.SyncAck{head}`, including a seq mismatch, so the sender resends from `head+1` out of its own Store; that also heals a receiver that missed records before a crash. `httpsync.ApplySync` implements these receiver rules over any `Store`.
- The sender reacts to a rejection by its error. `ErrStaleEpoch` (an older epoch), `ErrConflict` (different bytes at a seq), and `ErrSyncRejected` (a request that a resend cannot fix) are final: each stops the session with no further append and releases its `Ownership`, and `Session.Release` then returns the rejection (`ErrSessionNotOwned` after a stale epoch). Any other error resends the same batch with backoff (250 ms, doubling to 30 s) until it succeeds or the ownership ends. On start, a box harness replicates every session that its `Store` holds, not only the sessions that it opens. A receiver diverges when two owners at one epoch write one store, or when a box disk is restored behind the receiver.
- A box harness replicates through the config keys `owner_epoch` and `sync {url, token_file}`, which the box boot code writes. `New` reads neither key. `cmd/harness` passes `NewLocalOwner(owner_epoch)` and, with `sync` set, `httpsync.New(sync)`. `httpsync.New` returns a sender that posts each `protocol.SyncBatch` as JSON to `sync.url` with `Authorization: Bearer <token>`, and reads the token from `token_file` for each post. The receiver replies `200` with `SyncAck{head}` (also on a seq mismatch); `409` with `protocol.CodeStaleEpoch` (`ErrStaleEpoch`) or `protocol.CodeSyncConflict` (`ErrConflict`); `400` with `invalid_request` (`ErrInvalidRequest`); `413` with `payload_too_large` when the body is over `protocol.MaxSyncBatchBytes` (32 MiB); `401`; or `403`. The `400`, `413`, `401`, and `403` rejections also match `ErrSyncRejected`. Only those rejections (`409` with either code, `400 invalid_request`, any `413`, `401`, `403`) are final; the sender takes every `413` as final whatever its code. Any other status and a transport error resend with backoff. The sender keeps the JSON body of a batch under `protocol.MaxSyncBatchBytes` by cutting it after fewer records than the page of 512, so a `413` happens only when one record with its blobs is over the bound.
- `Runtime.CatchUp` is the start of a box harness: for each session in the `Store` that this runtime does not run, it takes a grant of the `Owner`, sends the log in pages from seq 1 (each acknowledgement moves the next page to the head that the receiver reports), and releases the grant. It appends nothing, so an idle session gets no `owner.acquired`. `Open` of a session that `CatchUp` holds ends the catch-up of that session first, and the opened session replicates itself. `Close` ends `CatchUp` with `ErrDraining`, and the next start catches up again. A stale epoch ends it with `ErrStaleEpoch`. If that `Open` fails, `CatchUp` replicates the session itself. Two calls at once run one after the other. A session that fails for another reason, such as a log that cannot be read or a grant that fails to release, is skipped, and `CatchUp` returns each failure with its session ID after the other sessions finish.
- The epoch is a number because fencing needs order. An embedder maps its own claim to a monotonic epoch.

### Messages

`GET /sessions/{id}/messages?before=&limit=` answers a `protocol.MessagePage` of the conversation that a reader sees, oldest first (`State.Transcript`, which keeps the messages that a compaction folded): `messages`, `first_seq`, `last_seq`, `total`, `has_more`, and `commands`.

- The seq of a message is its place in the conversation, counted from 1: the summary of each compaction stands between the messages it folded and the messages kept, and a compaction inserts its summary before the kept messages, so the seqs of the kept messages move by one and a message ID is the stable key.
- `before` names a seq: the page holds the messages that precede it, and a missing `before`, or one past the end, names the newest page. A page before `first_seq` is the next older page. `limit` is 100 when absent or 0; a limit above 1000 is `invalid_request`. A `before` or `limit` that is negative, not an integer, empty, or repeated is also `invalid_request`.
- The IDs derive from the log: `msg_<input_id>` for an input that a turn started with or that a steer promoted, `msg_<item_id>` for an item, `msg_resolved_<request_id>` for the result of a dismissed request, and `cmpsum_<seq>` for the summary. `protocol.Message.IsCompactionSummary` is true for a message with the `cmpsum_` prefix, so a reader tells a summary from a user message by this and never by its text.
- A message holds `id`, `role`, `parts`, and `created_at`, the envelope time `t` of the record that completed the message: the `turn.started` record for a user message that an input started, the newest `input.promoted` record for a user message of steer inputs, the `item.completed` record for an item, the `request.resolved` record for the result of a dismissed request, and the `compaction.applied` record for the summary.
- A user message also holds the `source`, `source_id`, and `source_label` of the input that started its turn, also when the runtime admitted that input itself (`parent`, `child`, `goal`, or `harness`), so a reader tells it from the input of a person. A user message of steer inputs that joined a running turn (the `OPERATOR MESSAGES` block) holds no source of its own and an `operator_batch` instead: one entry of `id`, `text`, `source`, `source_id`, and `source_label` for each input, in the order that the block numbers them.
- A tool result holds its text in `content`, and a blob part names its media type, size, and `key` (see Inputs). A `web_search` part holds `call_id`, `query`, `results`, and for a failed search the error code in `text` with `is_error`. A text part may hold `citations`. See Web search.
- A `commands` entry is the newest record of a typed slash command, placed after the message that was the newest when its first record was appended.
- `View.Messages` reads the same page for a session that this runtime does not run.

### Errors

Body: `{"error":{"code":"...","message":"...","details":{}}}`.

| Code | Status |
| --- | --- |
| `invalid_request` | 400 |
| `session_not_found` | 404 |
| `session_exists` | 409 |
| `request_not_pending` | 409 |
| `session_not_owned` | 409 |
| `input_conflict` | 409 |
| `turn_mismatch` | 409 |
| `session_busy` | 409 |
| `model_unavailable` | 409 |
| `not_a_repo` | 409 |
| `no_base` | 409 |
| `too_many_changes` | 409 |
| `process_not_found` | 404 |
| `blob_not_found` | 404 |
| `payload_too_large` | 413 |
| `draining` | 503 |
| `unauthorized` | 401 |
| `session_unreplayable` | 500 |
| `internal` | 500 |

Each code except `internal`, `payload_too_large`, and `unauthorized` is a sentinel error, and every code is a `protocol` constant of type `protocol.Code`. A sentinel of the Go API is a `*protocol.Error`, and `Error.Is` matches another `*protocol.Error` with the same code, so an error that a client decodes from a body matches the sentinel of its code. A stop cause (`ErrSessionHeld`, `ErrSessionReleased`, `ErrSessionEnded`, `ErrOwnershipLost`) is a `session_not_owned` error with a cause in its details, and it matches only its own cause and `ErrSessionNotOwned`. The body carries that cause in `details.cause`, so an error that a client decodes matches the same sentinel. The Sync receiver codes are not in the table, so `protocol.HTTPStatus` answers 500 for them, and a receiver chooses its status from the sync wire contract. `protocol.CodeOf(err)` returns the code of the first `*protocol.Error` in the chain of `err`, or `internal`, and `protocol.HTTPStatus(code)` returns the status of the table, or 500 for an unknown code. The message of an `invalid_request` error is only the reason, such as `keep_turns must be >= 1`: it holds neither the leading text of the sentinel (`harness: invalid request`) nor the name of a package. An error that is only the sentinel, with no reason, keeps the sentinel text. `protocol.Reason(err)` returns the text of `err` without the leading text of its `invalid_request` sentinels, and the HTTP body, the ACP error, and the text of a failed typed command use it. `errors.Is(err, ErrInvalidRequest)` matches the error, and `err.Error()` of the Go API keeps the sentinel text. `New` returns a plain wrapped error for a bad `Options`, never `ErrInvalidRequest`. A log that does not replay fails with `ErrUnreplayable`, code `session_unreplayable`, and its body message is fixed. `cmd/harness serve` writes `unauthorized` in its token check, before the request reaches `internal/server`. The session codes are sentinels in `harness`, and `blob_not_found` is `ErrBlobNotFound`. `process_not_found` is `ErrProcessNotFound`, and `not_a_repo`, `no_base`, and `too_many_changes` are sentinels of `internal/workspace`. The server answers `not_a_repo` for a directory that is outside a repository. `internal/server` takes the code of an error from `protocol.CodeOf`, after its own dependencies' sentinels. A body above its limit fails with `payload_too_large`: 32 MiB for `POST /sessions/{id}/inputs`, and 8 MiB for any other route. Any other error is `internal`, and its message is a fixed string. A path or method that no route serves answers 404 or 405 with `invalid_request`.

The actor masks and bounds each error text that it writes to the log (`turn.ended` error, `goal.changed` reason): recognized credential shapes are redacted (best effort: a free-form text can hold a shape that the fixed patterns miss), and a text longer than 256 runes keeps its first 256 runes and ends with `...[truncated]`. A failed `turn.ended` also holds the message as a report to a parent reads it, in `error_detail`, when that differs from `error`: masked by the secret patterns and cut at 500 runes with `… [truncated]` (see Settle).

### Slash commands

`Session.Prompt` resolves an input with `source: typed` and one text part through the `command` package. Any other input is never a command.

- Not a command: the input is admitted as is. `//x` is admitted as the text `/x`.
- An unknown name: the prompt command of that name under `commands_dirs` (default `<WorkDir>/.agents/commands`) is admitted as its expanded text with `source: command` and the typed line, cleaned as `source_label` is, as its `source_label`; the client's `source_id` stays. With no such file, or no `WorkDir`, the line is admitted as text.
- Bad arguments: one `command.recorded` with `failed` and the error of `Resolve`. Nothing runs.
- A frontend command, or a control command with no operation here (`queue-clear`): `unsupported`, "/<name> is not available in this client".
- A control command that is not `available_during_task` while a run is on: `refused`, "/<name> cannot run while a turn is running; send it again after the turn ends".
- Any other control command records `accepted`, runs after `Prompt` returns as work that `Runtime.Close` waits for, and records one more status. After `Close` starts, such a command fails with `draining` and records nothing. The statuses: `succeeded` ("/<name> succeeded", with the JSON result up to 16 KiB), `failed` (the error text of a sentinel error, or "/<name> failed: internal error", also for a panic of the operation), `refused` (a `session_busy` error), or `interrupted` (the runtime stopped).
- The operations are the Go API: `abort` is `Interrupt`, `compact` is `Compact`, `goal` is `SetGoal`, `goal-clear` is `ClearGoal`, `model`, `thinking`, and `tier` are `Update` (`/thinking <effort>` takes `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`; when the model of the session does not list the effort, its text reads "/thinking succeeded: <effort> is not offered by <model>; using <level>", with the level that a request sends, see Reasoning effort), `status` is `View`, `queue` is the queued input IDs, and `processes` is the process list of `GET /processes`. `/compact [keep_turns]` passes `keep_turns` to `Compact`. Its result is the `protocol.Compacted` of `Compact`. A compaction with no turns to fold fails with "/compact did nothing: the session does not have enough turns yet to fold".
- `<name>` is the name or alias that the user typed. The command never becomes an input, so the model never sees it.
- A repeat of the input ID with the same line returns the newest status; another line, or an input ID of another input, is `input_conflict`.
- `Open` records `interrupted` for each command that an earlier owner accepted and never finished: "harness restarted before /<name> finished; it will not run again". No command runs again.

`GET /commands` returns `Runtime.Commands`: each built-in command and each prompt command, sorted by name, with `serve_support` by name and `discovery_errors` for files with no usable name. A control command names its operation and `available_during_task`; the handler adds the route of the same operation, where one exists. A prompt file that is not valid is listed, unsupported, with its error as the reason; the parse stops at the first problem, so a file with an unknown key and a later error lists the unknown key. A client shows the expanded text of a prompt command, not its label.

### Contract source

Go types in `protocol` are the source. `go generate ./protocol` writes `protocol/openapi.json` and `protocol/protocol.ts` with `github.com/invopop/jsonschema` and a route table that `internal/server` exports. CI regenerates and fails on a diff.

- A test in `internal/gates` reads the route list under Routes and the read routes of `httpapi.ReadHandler` from this spec, and fails when the route table, the read routes of the table, or a route that `httpapi.Handler` or `ReadHandler` mounts differs from them.
- The same test fails when the `protocol` `Code` constants, `protocol.HTTPStatus`, the Errors table, or the enum of `Error.code` in `protocol/openapi.json` (generated from the `Code` constants) names a code that another one lacks, or gives a code another status.
- The generated contract holds what the handler does: each request body is optional, because the handler reads an empty body as the zero request; a type that only a request holds refuses unknown fields, as the handler answers `invalid_request`; and each success status of a route is an entry of the table. The process, logs, and health bodies are `protocol` types (`ProcessInfo`, `ProcessStatus`, `ProcessLogs`, and `Health`).

### Command

`cmd/harness` is the composition root of the runtime. It resolves environment variables and flags, builds the `Owner` and `Sync` that `owner_epoch` and `sync` name, builds one `harness.Runtime` (`acp` builds one for each `cwd`), and holds no runtime behavior. It passes `os.Getenv` as `Options.Getenv` and its JSON `slog` logger as `Options.Logger`.

- `serve` applies the `HARNESS_*` variables with `config.ApplyEnv`, then the flags `-no-instructions`, `-skills-dir`, and `-agent-def-dir`, so a flag wins. It keeps the sessions in a `DiskStore` at `HARNESS_SESSION_DIR`, else `session_dir`, else `~/.harness/sessions`. It serves `httpapi.Handler` behind the bearer token of `HARNESS_RUN_TOKEN`; `GET /health` needs no token, a request without the token gets 401 with the error code `unauthorized`, and an empty token is allowed only on a loopback bind or with `-unauthenticated` or `HARNESS_UNAUTHENTICATED`. With `-cors-origin` it adds the CORS headers, and a preflight skips the token. It binds `-addr` before it builds the runtime and takes `Options.ServeURL` from the bound address, so `-addr 127.0.0.1:0` serves on a free port that the `addr` field of the `serve start` log line names. Then it calls `CatchUp` and opens each session that has work to resume, and a failure of either is logged and never stops `serve`.
- `serve` wraps its store to log a slow store operation (`slow store phase`, over 1 s), an operation still in flight (`store phase in flight`, every 5 s after 5 s), the append that creates a session (`session created`), and the append of `child.spawned` (`task spawned`). It also logs the config summary at start, and each GC pause of 200 ms or more.
- `run -p <prompt>` or `run -goal <condition>` creates a session, or opens the one that `-r <id>` or `-c` names, on a `DiskStore`, or on a `MemStore` with `-no-save`. `-c` names the newest root session whose `work_dir` equals the cleaned current directory, and fails with `no sessions to continue in <dir>` when none does; a session from another directory, and a session with no `work_dir`, is never chosen. `-model` replaces the model of an opened session, `-system` appends a prompt segment, and `-max-tokens` sets `max_tokens` and wins over the config key. It submits the prompt as a typed input, so a slash command runs as `Prompt` runs it, and prints each text delta of the session and of each task child to stdout and each tool call and failed tool to stderr, or each event as a JSON line with `-json` (each line holds its `session_id`). It returns when the session has settled. It exits 1 when the turn failed, also under `-goal`, when the goal evaluator failed (the goal is `paused`, or `failed` with a verdict other than `impossible`; stderr names the error), or when a typed command did not succeed. It exits 3 when the evaluator judged the `-goal` not met, or `impossible`, and no turn failed. It prints `session: <id>` on stderr when it saved the session. A command that fails prints its error on stderr with the prefix `harness: `, once: an error that already starts with that prefix is not prefixed again. A run whose session another process opened later fails with the takeover error of `Ownership` and exits 1.
- `-h` and `--help` on any command (`version` included), and `help` at the top level and after `plugin`, print the usage to stdout and exit 0. A missing command prints the usage to stderr. An unknown command prints `harness: unknown command "<x>"`, a missing or unknown `plugin` subcommand prints `harness: plugin: missing subcommand` or `harness: plugin: unknown subcommand "<x>"`, and a flag error or a positional argument that a command does not take (for example `serve help` or `sessions extra`) prints `harness: <error>`, each followed by the usage on stderr. All of these exit 2, and the command does not start. One parse function in `cmd/harness` handles the flags of every command.
- `acp` serves ACP over stdin and stdout (see ACP). It takes no flag. `acp login` and `acp logout` followed by `codex` or `claude-code` run `login` and `logout`, because a client appends the `args` of a terminal method to the command that starts the agent. `initialize` reads the config of the work directory of the process, with its project file, to find the logins to offer (see Authentication). It reads no other config file, makes no network call, starts no subprocess (it looks up the Claude Code program on `PATH`, checks that the Codex login file exists, and runs nothing), and builds no runtime until the first `session/new`, `session/load`, `session/resume`, or `session/list`. Then `internal/acp/cwdruntimes` builds one `harness.Runtime` for each `cwd` that such a request names, and one for the work directory of the process for a `session/list` with no `cwd` when it holds none, with `config.LoadProject` of the user config and that `cwd` and `WorkDir` set to that `cwd`, over one `DiskStore` and one `NewLocalOwner`. It takes the session directory and the owner epoch from the config of the first `cwd`. It sets no `Sync`. A `cwd` that is not a directory is `-32602`. A `SIGINT` or `SIGTERM` ends it with status 0.
- `login codex [--device] [--no-browser] [--status]` signs in to the ChatGPT subscription, or prints the stored login, and `logout codex` deletes it (see ChatGPT subscription login). Both read the user config only, make no network call before the sign-in needs it, and print no token. `login` is the only command that may start the browser, and it opens only the URL that it printed. After a successful sign-in, when the user config file sets no `model`, `login codex` writes `model` `codex/gpt-6.1-sol` into that file (see Config): it creates a missing file with mode 0600, or, in the text of the existing file, replaces the value of each `model` key (of any letter case) or inserts the key first, so every other byte and the mode stay, refuses a file whose top level is not an object, parses the new text before the rename and refuses a text that does not load back with the model and every other key as before, replaces the file in one rename, writes through a symlink, and refuses a symlink to a missing file. It prints `Default model set to <ref> in <path>.`. It never replaces a `model` that is set and never writes a project file. A file that does not parse stays as it is and the command prints why. `--status` writes nothing.
- `login claude-code [--console] [--sso] [--status]` runs `claude auth login` with the terminal of the process, `--console` and `--sso` passed on, or with `--status` runs `claude auth status --text`. `logout claude-code` runs `claude auth logout`. The program is the Claude Code CLI of the user config (the entry of the default model when it runs the CLI, else the first such entry in name order, else `claude` from `PATH`). A user config that does not load is an error and starts no program. Harness never reads or stores Claude Code credentials. When `claude auth login` exits with status 0 and `claude auth status --json` reports `loggedIn` true, the command saves the default model as `login codex` does, with `claude-code/sonnet`. ACP `logout` removes the ChatGPT login only and never runs `claude auth logout`.
- `sessions` lists the stored sessions in creation order with their message counts, or a JSON array with `--json`.
- `plugin probe` calls `Runtime.ProbePlugins`, which probes each plugin again and refreshes the manifest cache, and prints each plugin with its hooks.
- `serve` deletes `serve-stop.json` in the session directory at start. On `SIGINT` or `SIGTERM` it stops the turns, which hand off, waits for `Sync` to acknowledge every record, and closes the runtime and the HTTP server within 5 s. Then it writes the file as one line, `{"stop":"handoff"|"crashed","sync":"synced"|"unsynced"}`, and exits. `stop` is `handoff` when `Runtime.Close` returned nil and `crashed` when it failed or passed the deadline; a session whose tool ignores the cancel then keeps its lease until the lease expires. `sync` is `synced` when `Close` returned nil, a `sync` is configured, the start caught up every stored session, and `Runtime.SyncStopped` is false, else `unsynced`. A missing file, such as after a kill, means `crashed` and `unsynced`.

## ACP

`harness acp` serves the Agent Client Protocol (ACP) version 1 over stdin and stdout, for editors and for programs that start it. `internal/acp` is the adapter. It reads bytes from an `io.Reader` and writes bytes to an `io.Writer`, so it does not know its host. `internal/acp/rpc` is the framing.

### Schema

- The schema is `schema.json` of the release `schema-v1.25.0` of `github.com/agentclientprotocol/agent-client-protocol`, with sha256 `4bcd1d2051ff7ed379bd9b234fb9b09c5ab9245478698e9fab805bd9b52a33a6`. The repository keeps it at `e2e/testdata/acp/schema/schema.json`.
- A test fails when the file does not match the digest.
- The test client checks each frame in both directions against the schema. The validator implements each keyword that the schema uses. A schema that uses another keyword fails at load, in a test.
- One protocol version holds for each connection. Version 1 is the only version.

### Transport

- A message is one JSON-RPC 2.0 object on one line, with no newline inside it. stdout holds ACP messages only. The JSON `slog` logger writes to stderr.
- One goroutine reads the lines, one goroutine writes, and each request runs in its own goroutine. The messages of one goroutine reach stdout in the order of its writes.
- A blank line is skipped. A line of more than 64 MiB is skipped and answered with `-32700`.
- A line that is not JSON is answered with `-32700` and a null id, and the connection stays open.
- A line that is JSON but not a request, a notification, or a response is answered with `-32600` and a null id. An `id` that is not a string, a number, or null is the same error.
- A request with a null `id` is a request and is answered. A message with no `id` is a notification.
- Two numbers that are equal name one id, so `1` and `1.0` are the same id. A request whose id names a request that still runs is answered with `-32600`. A request id is free when the agent has decided its answer. A client that gets the answer and then sends a new request with the same id is not refused.
- A request for a method that the agent lacks is answered with `-32601`; a notification for one has no answer.
- `$/cancel_request` cancels the context of the request that `requestId` names. If that request then fails, the answer is `-32800`. A `requestId` that names no running request is ignored.
- A handler that fails with an error that is not a JSON-RPC error is answered with `-32603` and the text `Internal error`. The error text never reaches the client.
- At the end of the input, the process writes the responses of the requests that run, and then exits with status 0. After `SIGINT` or `SIGTERM` it cancels the requests that run, writes their responses, and exits with status 0. It exits with status 1 when it cannot read its input.

### Methods

| Method | Status |
| --- | --- |
| `initialize` | Answered |
| `session/new` | Creates a session on the runtime of `cwd` |
| `session/load` | Opens a session of the store and replays its conversation |
| `session/resume` | Opens a session of the store with no replay |
| `session/list` | Lists the root sessions of the store |
| `session/close` | Cancels the work of a session and releases it |
| `session/prompt` | Runs one turn |
| `session/cancel` | Notification. Cancels the prompts of a session |
| `session/set_config_option` | Sets the model or the effort of a session |
| `authenticate` | Runs the login of an agent-type method that `initialize` advertises |
| `logout` | Ends the logins that `initialize` offers |
| Any other method | `-32601` |

### Initialize

- `initialize` answers the agent info, the agent capabilities, and `authMethods`, which is empty unless the host offers a login (Authentication).
- The agent info is `name` `harness`, `title` `Harness`, and the harness version.
- When the client version is not 1, the answer is version 1, the latest version of the agent. When it is 1, the answer is 1.
- `initialize` without a `protocolVersion` that is an integer from 0 to 65535 is `-32602`.
- The agent capabilities are `loadSession` true, `mcpCapabilities` with `http` true and `sse` false on a host that starts MCP servers (`harness acp` is one), and `sessionCapabilities` with `resume` and `close`, and with `list` on a host that has a work directory for a `session/list` with no `cwd` (`harness acp` and the wasm Worker of `sdk/embed` are ones). Without `list`, `session/list` is `-32601`. `session/delete` is not advertised. `promptCapabilities` has `image` true and `embeddedContext` true, and no `audio`: the agent advertises a capability only when the method that uses it works.
- The agent never sends `session/request_permission`: harness has no permission system.
- The agent reads five client capabilities. `session.notices`: a `notices` that is an object turns on the lost-ownership `notice` (Notices); any other value, and a missing `session`, leave it off. `session.compaction`: a `compaction` that is an object turns on the compaction updates (Compaction); any other value, and a missing `session`, leave them off. `auth.terminal`: `true` lets the agent advertise terminal methods (Authentication); any other value, and a missing `auth`, leave them out. `elicitation.form`: a `form` that is an object (`{}` at the top level of `elicitation` advertises no mode) turns on the elicitation of a question (Questions); any other value, and a missing `elicitation`, leave it off. `elicitation.url`: a `url` that is an object lets a host offer a login that needs a URL (Authentication); any other value leaves it off. The agent advertises no agent capability for it.

### Authentication

- `harness acp` offers the logins that the config of the work directory of the process uses (the user config and its project file). `initialize` always offers the method `chatgpt`, "Log in with ChatGPT", because the native `codex` provider needs no entry. It offers the method `claude-code`, "Log in with Claude Code", when the program of the Claude Code CLI resolves on `PATH` (a lookup that starts no process): that program is the one of the entry of the default model when it is of type `claude-code-cli`, else of the first such entry in name order, else `claude` of the native `claude-code` provider. A host that gives the adapter no login offers none, and then `authMethods` is empty, the agent capabilities have no `auth`, and `authenticate` and `logout` are `-32601`. The WebAssembly build offers no `chatgpt` method (see WebAssembly).
- `initialize` offers the method `injected-<provider>`, "Log in to <provider>", of type `agent`, for each provider entry of the user config with `auth` `injected` and a `login_url`, and only to a client that advertises `elicitation.url`. `agentCapabilities.auth.logout` is present only when the method `chatgpt` is offered: the page behind the `login_url` owns the logout of an injected login.
- `authenticate` with the method `injected-<provider>` sends the status probe first and answers `{}` at once when it answers 200. Otherwise it sends `elicitation/create` with `mode` `url`, no `sessionId`, `requestId` the id of the `authenticate` request, a fresh `elicitationId`, `url` the `login_url`, and `message` "Sign in on the page that opens, then return here." `decline` ends the flow with `-32603`, "The sign-in was declined", and `cancel` ends it with `-32800`. After `accept`, the agent sends the probe at once, then every 2 s (every 4 s after 30 s, every 8 s after 2 minutes), for at most 10 minutes. When the probe answers 200, the agent sends `elicitation/complete` with the `elicitationId` and answers `{}`. The whole `authenticate` ends after 10 minutes from its start, so a client that never answers the elicitation cannot hold the request open. A probe that answers 404 or 405 means that the server has no status probe: `authenticate` then answers `{}` when the user accepts the elicitation, and only the 401 of a turn is `login_required`. A timeout is `-32603`. `$/cancel_request` and a new `authenticate` end the flow as for the other methods, and the agent sends `$/cancel_request` for an elicitation that has no answer. The agent reads no token, stores no file, and sends no frame that holds a secret: the page behind the `login_url` runs the login on the server.
- A 401 of an entry with `auth` `injected` fails the turn with the error class `login_required`. A 403 is not `login_required`. A 401 of a model call 2 minutes or more after the last probe that answered 200 fails the turn with the error class `login_required`. A 401 of a model call within 2 minutes after a probe of the same entry answered 200 ends the turn `permanent`, with the message `the server accepted the sign-in check but rejected the model call; sign in again at <login_url>, or check how the server authorizes model calls` (without the URL part for an entry with no `login_url`), and the cached answer of the entry holds "signed in, model calls rejected" until the next `authenticate`. So `authenticate`, a 200 probe, and a 401 of the model call cannot loop: the second pass reports the fault of the server and stops.
- For an entry with `auth` `injected`, `session/new`, `session/load`, and `session/resume` send the status probe and fail with `-32000`, `data.code` `login_required`, when it answers 401. The message holds the `login_url`, so a client without `elicitation.url` can still show it. `session/prompt` sends no probe and reads the cached answer only. The answer is cached for 30 s, and `authenticate` probes again. A failed turn reads the cache only, so a probe never replaces the error of a turn. The status probe of the Anthropic adapter is `GET <base_url>/v1/models` with the header `Anthropic-Version`. A probe that fails or takes more than 3 s is unknown and raises no error. `initialize` never sends a probe (see Injected auth).
- The WebAssembly build offers no method `chatgpt`, no method `claude-code`, and no method of type `terminal`: it has no localhost listener for the browser sign-in, it keeps no login file, and it starts no process. Its only logins are the `injected-<provider>` methods above, so a connection with no `injected` entry that has a `login_url`, or a client without `elicitation.url`, gets an empty `authMethods`; `agentCapabilities` then has no `auth`, and `authenticate` and `logout` are `-32601`. The module reads no login file, whatever `codex_auth_file` says. A request on an entry with `auth` `chatgpt` fails before it is sent, with the error class `login_required`.
- `authenticate` takes a `methodId` that names an advertised method of the agent type. Any other `methodId`, a terminal method among them, is `-32602`. The `chatgpt` method runs the browser sign-in of `harness login codex`: it opens the system browser, listens on `127.0.0.1` port 1455 or 1457, stores the login, and answers `{}`. The `claude-code` method runs `claude auth login` as a child with no terminal, which opens the browser itself (the program is the Claude Code CLI of the entry of the default model, else of the first such entry in name order), and answers `{}` when the CLI exits with status 0 and `claude auth status --json` does not report `loggedIn` false. A failure answers `-32603` with a readable message and no token, and so does a sign-in that lasts more than 10 minutes. `$/cancel_request` stops the sign-in, closes the listener or kills the process group of the child, and answers `-32800`. A new `authenticate` cancels the one in flight, whatever the method, and answers `-32800` to it. Sign-ins run one at a time.
- When the client advertises `auth.terminal`, each method has a second method of type `terminal`, with the id of the first and the suffix `-terminal`. Its `args` are `login codex` or `login claude-code`, which the client appends to the command that starts `harness acp`. The client runs that command in a terminal, and a zero exit status is success. A wrapper script that starts `harness acp` must pass its arguments on (`"$@"`), or the terminal methods fail.
- `agentCapabilities.auth.logout` is `{}` when the method `chatgpt` is offered, and absent otherwise (a host with no login offers none). `logout` removes the stored ChatGPT login under its lock, as `harness logout codex` does. It never runs `claude auth logout`: the CLI owns its login, and `harness logout claude-code` ends it. Without the capability, `logout` is `-32601`.
- Harness never reads the credentials of Claude Code. It runs the CLI and reads the `loggedIn` field of `claude auth status --json`. Harness stores the ChatGPT login in its own file and never prints a token, so no frame and no log line holds one.
- A `session/new` whose default model, or a `session/prompt` whose session model, belongs to a provider that has no login is `-32000`, "Authentication required", with `data.code` `login_required`. The message says which command signs in. A ChatGPT provider has no login when the file of `harness login codex` is missing or empty. A Claude Code provider has no login when `claude auth status --json` reports `loggedIn` false, and only when `initialize` advertised the method `claude-code`: a provider that only a project file adds, in a directory that `initialize` did not see, raises no auth-required error. The result of the CLI is cached for 30 s and read again after `authenticate` and after a failed turn. The adapter calls the CLI on the first request that needs it and never at startup.
- A turn that fails with the error class `login_required`, because the refresh token of the ChatGPT login was rejected, answers `session/prompt` with `-32000`, `data.code` `login_required`, and the error of the turn as the message, without its leading `[class]` tag. So does a turn that failed while the CLI reports `loggedIn` false. Such a prompt leaves the failed turn in the log.

### Sessions

- `session/new` takes `cwd` and `mcpServers`. A `cwd` that is not an absolute path of a directory is `-32602`. A `session/new` with another `cwd` runs on another runtime, which uses the project config of that `cwd`. The session has the origin `acp`. `additionalDirectories` is ignored and not advertised.
- The `mcpServers` of `session/new` are the MCP servers of that session only. `harness.WithSessionMCP` receives them: a stdio entry as the command and its arguments, and an `http` entry as the URL and its headers. They are not stored. A name that two `mcpServers` entries share is `-32602`. So is an `sse` entry. On a host that starts no MCP server, any entry is `-32602`.
- `Config.AllowedTools`, when set, is the `allowed_tools` of every session that `session/new` creates. `harness acp` leaves `Config.AllowedTools` unset, so a session has every tool.
- The connection holds each session that it created. Its event pump is the only reader that turns events into updates. It starts at the head of the log when `session/new` returns, and it ends when the connection ends.

### Load, resume, list, and close

- `session/load` takes `sessionId`, `cwd`, and `mcpServers`. `session/resume` takes `sessionId` and `cwd`, and `mcpServers` when the client has them. A `session/resume` without `sessionId` or `cwd` is `-32602`. The `cwd` selects the runtime, and `mcpServers` follows the rules of `session/new`. A session that the store does not hold is `-32002`. A session whose log names a `work_dir` other than the `cwd` is `-32602`.
- The agent reads one view of the session with `Runtime.Read`. The head of the view is the replay boundary. `session/load` sends the conversation of the view, oldest first, then the `available_commands_update`, opens the session, and answers with the `configOptions` of the session. `session/resume` sends the `available_commands_update` and no conversation.
- The replay sends a user message as `user_message_chunk` updates, and an assistant message, a tool call, and a tool result as the updates that a turn sends, with the same `messageId` and `toolCallId`. The `diff` of a call stays in its result. A part of type `engine_context` or `task_report` is not sent. The summary message of a compaction (a message whose `protocol.Message.IsCompactionSummary` is true) is never a `user_message_chunk`; Compaction says what the replay sends for it. The status text of a typed command that has ended follows the message that the command follows. An image in a tool result is not sent. A tool result whose text is exactly `(no output)` is replayed with no text, because a reader shows an empty result as that text.
- The event pump of a loaded or resumed session starts at the replay boundary, so a record that the log gains between the read and the open reaches the client once, as a live update. A pump that a `session/load` or `session/resume` starts queues its updates until the answer is sent, then sends them in order; the running pump of a held session is not queued. The pump keeps the `diff` of a call that has no result at the boundary, so its `tool_call_update` keeps the `diff`.
- Opening a session resumes the work that its log holds: a turn that ran, a queued input, or a goal. The updates of that work are live updates. When the request starts the pump, they reach the client after the answer; the pump of a held session sends them as they happen. The later `Open` wins across processes: it takes the session, and the earlier owner gets `session_not_owned` at its next prompt. Inside the process, a session with no `work_dir` that another runtime runs is `-32603` with `data.code` `session_not_owned`.
- The agent runs the `session/load`, `session/resume`, and `session/close` requests of one session one after the other, in the order they arrive. A request waits until the answer of the one before it is sent, and the updates that the pump of that request queued. So a load that follows a close opens the session after the release, and two loads of one session start one pump.
- A session that has a parent is a task child of another session. `session/load` and `session/resume` of it are `-32602`.
- A `session/resume` of a session that the connection holds returns the held session and sends nothing. A `session/load` of it waits until the event pump has sent every update through the head of a new view, replays that view, sends no command update, and answers. The replay does not change the state of the pump. A `cwd` other than the one of the held session is `-32602`, and so are `mcpServers` in either request, because the held session keeps the servers it has.
- `session/list` takes an optional `cwd` and an optional `cursor`. It lists the root sessions of the store in creation order. A session that has a parent is a task child of another session and is not listed. A session that has no `work_dir` is not listed. With a `cwd`, only the sessions whose `work_dir` equals it are listed. A `cwd` that is not an absolute path is `-32602`. A listed session has `sessionId`, `cwd`, a `title` that is null, and `updatedAt`. The agent reads 50 sessions of the store for each step and continues until the page holds a session or the store ends. `nextCursor` is present when more sessions can follow. A `cursor` that names no session is `-32602`. One call reads at most 4 steps. A call that reads them all and finds no match answers no session and a `nextCursor`. Without a `cwd`, the agent lists through a runtime that it holds, else through the runtime of the work directory of the process.
- `session/close` cancels the prompts of the session as `session/cancel` does, waits for their answers and for their updates, stops the event pump, and releases the session. The log stays, so the session can be loaded again. A prompt that was in flight answers `cancelled`. A `sessionId` that the connection does not hold is `-32002`.

### Prompts

- A `session/cancel` that follows a `session/prompt` on the wire reaches that prompt. This holds also when the prompt has not started to run.
- `session/prompt` runs one turn and sends the text of the model as `agent_message_chunk` updates before it answers `end_turn`. The reasoning of the model is `agent_thought_chunk` updates. A text delta is a chunk; the rest of an item that the deltas did not cover is one more chunk when the item completes. When a slow reader missed a delta, the chunks sent are not the start of the item text, and ACP version 1 cannot retract a chunk. Then the whole item text is sent as one more message, with the `messageId` `msg_` and the item ID and `_full`. The `messageId` of a message chunk is `msg_` and the item ID, and that of a thought chunk is `thought_` and the item ID.
- The agent mints the ID of the input, and sends the input with the delivery `queue`: a prompt that arrives during a turn waits for the next turn. A prompt block of another type than `text`, `resource_link`, `image`, and `resource` is `-32602`, an `audio` block among them. So is a prompt with no block. A `resource_link` is the text of its name and URI. An `image` block is an attachment of the media type that it names: png, jpeg, gif, or webp, and no other type; a PDF in an `image` block is `-32602`. A `resource` block with `text` is a text part: a `<resource uri="…">` line, the text, and a `</resource>` line, so the model can tell a file or a selection from the words of the user. A `resource` block with `blob` is an attachment of the media type that it names: png, jpeg, gif, webp, or pdf. The agent decodes the base64 `data` or `blob` and sends it as a blob part of the input (Inputs). An `audio` block, an attachment of another media type, and an attachment that is not valid base64 are `-32602` with `data.reason`. A prompt that the runtime refuses with `invalid_request`, an attachment of the wrong content or size and an image for a model without image input among them, is `-32602` through the one error mapping (Error mapping): the message is the text of the runtime error, which names the model in the last case, `data.code` is `invalid_request`, and `data.reason` is that text.
- A required field of a content block or of an `mcpServers` entry that is omitted or null is `-32602`. Two header names that differ only in case are one name.
- A prompt for a session that the connection does not hold answers `-32002`.
- The updates of a turn reach the client before the answer: the answer waits until the event pump has sent every update through the end of the turn. The answer of a prompt that a turn takes follows every update of its turn and precedes every update of a later turn: the pump holds at the end of the turn until the response is queued. The answer of a typed command or of a withdrawn input follows the updates through its final record.
- A turn that starts with no `session/prompt` open, such as a goal turn, sends its updates as notifications.
- The stop reasons are: `completed` and `awaiting_input` are `end_turn` (a client with `elicitation.form` also gets an `elicitation/create` for the question, see Questions); `interrupted` is `cancelled`. After `session/cancel`, the prompt that runs answers `cancelled`, after every update that it has pending. A prompt that a cancel withdraws before a turn takes it answers `cancelled`.
- A turn that fails answers `-32603` with `data.code` `internal` and `data.error_class`; the message is the masked error of the turn. A failure that Authentication names answers `-32000`.
- `$/cancel_request` for a prompt also interrupts its turn.
- A tool call of the model is a `tool_call` update. The adapter sends it when the assistant message that holds the call completes, after the text of that message. It holds the `toolCallId` (the call ID), the `name` of the tool, a `title`, a `kind`, the status `in_progress`, and the arguments as `rawInput`. No frame streams the arguments, so the status `pending` is not used.
- The `kind` of a tool comes from a table of tool names: `read_file` and `read_tool_result` are `read`; `write_file` and `edit_file` are `edit`; `glob`, `grep`, and `ls` are `search`; `bash` and `process` are `execute`; `web_fetch` is `fetch`. A Claude Code session records the harness tools it calls under these same names, without the `mcp__harness__` prefix of the CLI. Any other name, including each MCP tool and plugin tool, is `other`.
- A `web_search` part of an assistant message is a `tool_call` update with the kind `fetch`, the status `completed`, the title `Search: <query>`, the query as `rawInput`, and one `resource_link` content item for each source (see Web search).
- A failed search is the same update with the status `failed` and the error code as text content.
- A session whose `allowed_tools` leaves out `web_search` gets no web search, also when `web.search` is true.
- A file tool adds `locations` to its `tool_call`: the absolute path of the file, and for `read_file` the `line` of its `offset`. A relative path resolves against the `cwd` of the session.
- An `edit_file` call adds one `diff` content item with the absolute `path`, `oldText` as the `old_string`, and `newText` as the `new_string`; a `replace_all` edit sends that pair once. A `write_file` call adds a `diff` with `oldText` null and the content as `newText`.
- The tool message that holds the result is a `tool_call_update` for the same `toolCallId`, sent when the message completes. Its status is `completed`, or `failed` when the result is an error. Its `rawOutput` is the text of the result. Its `content` replaces the content of the call: the `diff` for a call that succeeded and has one, otherwise the text of the result when it is not empty. Each image of the result follows as an `image` content item. A blob that is not an image is not sent.
- A tool call that the cancel of a turn cut short has an error result in the log, so its update is `failed`, and it reaches the client before the `cancelled` answer.
- At the end of the input, the agent releases its sessions first, so a prompt that runs answers `-32603` with `data.code` `session_not_owned`. Then it closes its runtimes, within 5 s, and writes the responses.
- A prompt for a session that another owner took answers `-32603` with `data.code` `session_not_owned`. When another owner takes the session, the prompt that runs answers the same error. So does each later prompt.

### Config options

- `session/new` answers `configOptions` with two `select` options. `model` has the category `model`. Its values are the `provider/model` references that `Runtime.Models` lists, and its current value is the model of the session, which for a session that names none is the default model of Config (the subscription of the user when one exists). `initialize` changes nothing else because of it. The values are in one group for each provider, in the order of `Runtime.Models`. The group id is the provider key, and the group name is the `provider_name` of its models, or the key with each word capitalized when they have none. Each value has the name of the model, or its reference when the model has no name, and a description of the reference, a separator, and the first sentence of the model description, cut as a whole at 100 characters. A model of the session that `Runtime.Models` does not list is added to the group of its provider, or to the group `Other` when the provider lists no model. `effort` has the category `thought_level`. It is a flat list of `default` and the effort levels of the current model, named `Default`, `Off`, `Minimal`, `Low`, `Medium`, `High`, `XHigh`, and `Max`. A model that `Runtime.Models` does not hold offers every level up to `high`. A model that it holds with no `efforts` offers `default` alone. `default` is an effort that is not set. The current effort is always one of the values, also when the model does not list it; a request then sends the nearest listed level (see Reasoning effort).
- `Runtime.Models` fetches the model list of a provider entry with `models_from` `chatgpt` that sets no `models` from the ChatGPT backend on its first call, and keeps it for the life of the process. `session/new`, `session/load`, and `session/resume` wait at most 3 s for it, and `initialize` never waits for it. When the fetch fails, times out, or returns no model, the list is the static list of the `codex` models, and the next call after 5 minutes tries again. A login that the backend rejects with 401 or 403 gives the static list until the stored access token changes. For an entry with `auth` `injected`, a 401 or 403 gives the static list until the entry becomes signed in or 5 minutes pass.
- `session/set_config_option` takes a `configId` and a string `value`. It changes the setting of the session and answers the complete list of options. A `configId` other than `model` and `effort`, a `value` that the option does not list, and a boolean `value` are `-32602`.
- When the model or the effort of the session changes, the agent sends a `config_option_update` with the complete list of options, with the effort levels of the new model. A `settings.changed` event that leaves both as they were sends none. This includes a change by `/model`, by `/thinking`, and by the `model` tool. The update of a `session/set_config_option` reaches the client before the answer.

### Commands

- After the answer to `session/new`, the agent sends an `available_commands_update`. It lists each command that the runtime can run from a typed line. The `description` is the summary of the command. The `input.hint` is its argument hint, and is omitted when the command has none.
- A prompt that is exactly one `text` block whose text starts with `/` is typed text. The runtime runs the slash command. The status text of the command is an `agent_message_chunk`, and the prompt answers `end_turn`. A command that the runtime ends as interrupted answers `cancelled`. A line that names no command goes to the model as text. A prompt that has other blocks next to the text is not typed text.

### Usage and session info

- A `usage_update` has `used` and `size` from the context of the session. The agent sends it when `used`, `size`, or the cost changes at a `context.measured`, `compaction.applied`, or `turn.ended` event. It sends none while the context window is not known. The `cost` is the session cost in `USD`, and is omitted when the session reports none.
- A `session_info_update` with `updatedAt` follows each `turn.ended` event. It has no `title`.

### Compaction

- A compaction reaches the client only through `compaction_update` and `compaction_summary_chunk`, and only when the client has the capability `session.compaction`. Without the capability, the agent sends no compaction update and no compaction message. The `usage_update` of a compaction does not depend on the capability.
- The compaction ID of an update is the ID of the compaction from the `status` frame that starts it, or `cmpsum_<seq>` (the ID of the summary message) when no such frame came before the `compaction.applied` record.
- A `status` frame with `compacting` sends a `compaction_update` with the status `in_progress`. A `compaction.applied` record sends the summary as one `compaction_summary_chunk` with a `text` block, then a `compaction_update` with the status `completed`. A `status` frame with `compaction_failed` sends a `compaction_update` with the status `failed`. A `status` frame with `compaction_canceled` sends one with the status `cancelled`. A frame with `idle` or `running` ends no compaction, because the summary of a compaction of the Claude Code CLI follows its `running` frame. A `compaction.applied` record with no earlier `compacting` frame, such as a compaction inside a turn of the harness loop, sends the chunk and the `completed` update only.
- The replay sends the summary message of a compaction as one `compaction_update` with the status `completed`, the ID of the summary message, and the summary text as one `text` block in `summary`, at the place of the message in the conversation. Without the capability, the replay sends nothing for it.

### Notices

- When another owner takes a session from the connection and the client has the capability `session.notices`, the agent sends one `notice` update with the severity `warning` for that session. Without the capability, the agent sends no notice.

### Questions

- When the client has the capability `elicitation.form`, each runtime that the agent builds sets `Options.AskUserQuestion`, so a session of any backend can ask a question. Without the capability, the agent does not set it, no question opens, and the agent sends no `elicitation/create`.
- A question is a `request.opened` record of kind `question`. When the turn that opened it ends `awaiting_input` and the client has the capability, the agent sends the request `elicitation/create` for it. It sends the request after it queued the answer of the prompt that ran the turn, so that prompt still answers `end_turn` first. A request that was open before the connection opened the session gets no elicitation.
- The `elicitation/create` request has `mode` `form`, the `sessionId`, and the `toolCallId`, which is the request ID, the call ID of the question. Its `message` is the text of each question, one question on each line. Its `requestedSchema` has one property for each question, named `question_1`, `question_2`, and so on, with the `title` of the header of the question and the `description` of its text. A question with options is a `string` with a `oneOf` list: the `const` of an option is its label, and its `title` is the label. The `description` of the option is its `description`, when it has one. When the question allows one answer and the option has a `preview`, the `description` of the option is its description, a blank line, and the preview cut at 200 characters with `…`. A question with options that allows more than one answer is an `array` whose `items` are the same list as `anyOf`, with no preview. A question with options also has the property `question_N_other`: it is an optional `string` with the `title` `Other`, so that the user can type an answer that no option holds. A question with options is not required. A question with no options is a required `string`.
- The `accept` action resolves the request with an answer that maps the text of each question to the label that the user chose, or to the chosen labels joined with `, `. When `question_N_other` has text, that text is the answer; for a question that allows more than one answer, it follows the chosen labels. The answer starts the next turn of the session, whose updates are notifications. An `accept` with no value for any question, the `decline` action, and the `cancel` action dismiss the request.
- A JSON-RPC error from the client, and the end of the connection, leave the request open, because the user made no choice. The next input from a person dismisses it.
- An input from a person that the session admits while the request is open dismisses it (an ACP prompt has the source `typed` or none, so it is such an input). An input of another source, such as a child report, waits in the queue and leaves the request open. The agent then sends `$/cancel_request` for the `elicitation/create` that has no answer yet, and it drops a late answer. `session/close` sends `$/cancel_request` for each `elicitation/create` of the session that has no answer, after the answers of its prompts. So does the release of the session at the end of the input. A dismissal of a question sends a `tool_call_update` with the status `failed` and the dismissal text for its call.

### Error mapping

One function maps a harness error to a JSON-RPC error by its `protocol` code. `data.code` is that code, `data.cause` is the stop cause of a `session_not_owned` error, and `data.reason` is the text of an `invalid_request` error, such as the config error that names the key, the provider, and both files. For `invalid_request` the message is the reason, as in the HTTP body.

| Code | JSON-RPC |
| --- | --- |
| `invalid_request`, `input_conflict`, `request_not_pending` | `-32602` |
| `session_not_found`, `blob_not_found` | `-32002` |
| `session_not_owned`, `session_busy`, `turn_mismatch`, `session_exists`, `draining`, `model_unavailable` | `-32603` |
| `session_unreplayable`, `internal` | `-32603` with the message `Internal error` |
| `payload_too_large`, `unauthorized`, `not_a_repo`, `no_base`, `too_many_changes`, `process_not_found` | `-32603`. No ACP method returns them |

An error with no code is `internal`. `model_unavailable` is not `-32000`: it means that the model has no API key or no catalog entry, and no login resolves it. `-32000` is the auth-required error of Authentication, with `data.code` `login_required`. It is not a `protocol` code: the adapter raises it from the login check and from the error class `login_required`.

## turn and backend

### Backend

```go
package turn

type Backend interface {
	Capabilities(model string) Capabilities // model is a provider/model ref
	Run(ctx context.Context, req Request, out Sink) error
}

type Capabilities struct {
	OwnsLoop        bool     // backend runs tools and multi-step turns
	OwnsContext     bool     // backend compacts its own context
	OwnsMCP         bool     // backend connects Config.MCPServers itself on an unrestricted turn
	Steering        bool     // a backend with OwnsLoop takes steer input mid-turn through Sink.Steer
	ContextWindow   int      // 0 means the backend reports it
	WindowEstimated bool     // ContextWindow is the default for a model that nothing states
	Tools           []string // built-in tools of a delegated backend
	WebSearch       bool     // the backend offers the native web search of the provider
}

type Snapshot struct {
	Head    json.RawMessage   // small; each save replaces it
	Entries []json.RawMessage // grow by appending
}

type Sink interface {
	Item(m protocol.LogMessage) error
	Delta(itemID string, d Delta)
	Alive()
	Telemetry(t Telemetry)
	Steer() ([]protocol.LogMessage, error)
	State(backend string) (Snapshot, error)  // the saved state, or the zero Snapshot
	SaveState(backend string, s Snapshot) error // stores only the entries that the saved ones lack
	Compacted(summary string) error
	Status(f protocol.StatusFrame) // ephemeral frame
	Ask(callID, kind string, payload json.RawMessage) error // open a request on an open tool call
	Resolution(id string) (protocol.RequestResolved, bool)  // the record that closed it
}
```

The turn loop reports through one `Turn`, which the actor binds to the turn. No method takes a turn ID, and a call after the turn is no longer the run of the actor fails with `turn mismatch`:

```go
type Turn interface {
	Sink
	Started() string               // announces an item and returns its ID
	ItemAs(id string, m protocol.LogMessage) error // records m under the ID that Started returned, or under a new ID when id is ""
	Attach(mediaType string, data []byte) (protocol.LogPart, error) // stores a tool-result blob and returns its part
	Settings() (string, protocol.Settings) // the model and settings that the session holds now; "" after the turn stops
	Pin(ns []Notice, at int) []Pin // pins the notices that changed, on the actor goroutine, and returns every pin
	CompactTurn(ctx context.Context) (history []protocol.LogMessage, ok bool, err error)
	Ended(err error)
}

func Run(ctx, step context.Context, b Backend, req Request, src Source, to Turn, lim Limits)
```

`Delta` ignores the backend `itemID`: one harness item can join several backend items, such as reasoning and text. The first delta after an item calls `Started`, which gives the live item a harness ID; the next `Item` of an assistant message records it under that ID, and an `Item` of any other role takes a new ID and leaves the live item. `Discard` removes the live item and clears its ID. An item that a failed call streamed gets no `Item`. Before the loop calls the model again, it calls `Discard`, so subscribers get `item.discarded` with the ID of that item, before the `retrying` status frame, and remove it, and the next delta starts a new item.

- A model API backend runs one model call per `Run`. The loop runs the tools, then takes the queued steer inputs through `Sink.Steer`, appends them to the history, and calls the backend again. This item boundary comes after the tool results of each response, on a path that calls the backend again. A response that ends the turn, or fails it, takes no steer input: that input waits and runs as the next turn. A queued input joins no running turn. Every model API backend steers, because the loop does it, not the backend.
- A delegated backend (`claudecode`) runs the whole turn and reports items.
- `AllowedTools` holds tool names in one namespace: the embedder tools, the runtime tools (`goal`, `process`, `task`, `ask_user_question`), the plugin tools, the MCP tools, and the built-in tools of the model. A model API backend has the file tools of the `WorkDir`, `read_tool_result`, and `web_fetch` while `web.fetch` is true as its built-in tools; an `AllowedTools` list that names `web_fetch` while `web.fetch` is not true offers no such tool, and for a delegated backend it fails `Create`, because `known` is false for the name. A delegated backend has the tools of `Capabilities.Tools`, and any other name fails `Create`. The name `web_search` is known for a model whose `Capabilities.WebSearch` is true, on either kind of backend. A model API backend ignores a name that none of its tools answers, for example `web_search` on an `openai-compat` model. A tool of the runtime or of a plugin with the name of a built-in tool of the backend also fails `Create`. An `Options.Tools` tool with the name of a built-in tool of a `WorkDir` (`read_file`, `write_file`, `edit_file`, `glob`, `grep`, `ls`, or `bash`) replaces that built-in tool in a session whose `AllowedTools` is a list, and the list name selects the embedder tool. Where `AllowedTools` is nil, the built-in tool is allowed, so `Create` and `Open` fail with `invalid_request` and names the tool. A runtime tool or a plugin tool with such a name always fails. One predicate, `known(model, name)`, decides whether a name is a tool of a session at that model. `New`, the plugin start, `Create`, `Update`, and the tool list of a child profile all use it.
- The default set of a host is data that the host chooses. A host that cannot run `bash` or `process` gives its list to the ACP adapter as `Config.AllowedTools`, and the adapter passes it as `AllowedTools` of every session that `session/new` creates. A session records the list, so a reload keeps it. The default set of `sdk/embed` is `read_file`, `write_file`, `edit_file`, `ls`, `glob`, `grep`, `read_tool_result`, `session_info`, and the names of the page tools. It leaves out `bash` and `process`, which fail with `exec: executable file not found`, and `task`, `model`, and `goal`, which start unattended work or switch the provider; a host that wants one lists it in `builtinTools`. A child of a task gets the allowed tools of its parent narrowed by the profile, so a host that lists `task` still does not offer `bash` or `process` to children.
- Retry, the stall watchdog, and compaction read `Capabilities`. No code compares a provider name.
- `modelapi` records a tool call with arguments that are not valid JSON with no arguments.
- A backend marks a failed model call with a `turn` sentinel. `ErrRetryable` (a 429 with `Retry-After`, a 5xx that outlasted the request retries, a transport error except one that declares itself permanent, a truncated stream, a content filter) calls the model again, up to `stream_max_retries` times (default 5), when the call has recorded no item. An HTTP 429 with no `Retry-After` is not retried, because only the advice of the provider makes it retryable (it keeps its class, so a goal still pauses on it). The wait is the `Retry-After` of the provider when the error carries one, else 200 ms that doubles with each retry, with up to 10% jitter. Before that, a model API client sends a request again, up to `request_max_retries` times (default 4), after a 5xx status or a transport error, with the same backoff; it does not send a 429 again, or a 5xx with a `Retry-After` header (the turn waits for that advice, with a `retrying` status frame), and a request that ran out of retries is one `ErrRetryable` of the turn. A transport error with a `Permanent()` method that reports true is not sent again, and the turn does not call the model again for it: `provider.Do` marks it permanent and the turn ends with class `permanent`. A request that the context ended stays an abort. Both keys are at most 100, a negative value fails `Validate`, and `HARNESS_STREAM_MAX_RETRIES` and `HARNESS_REQUEST_MAX_RETRIES` set them. The websocket transport keeps its own reconnect rule. A response with no tool call that the output cap did not cut off ends the turn as `completed`, as in the Codex CLI: the backend records the items that the response had, and a response with no text and no tool call is neither an error nor a retry. A backend with `OwnsLoop` runs its call once, and its retryable error keeps its class, so a goal still pauses on it. `ErrContextOverflow` compacts (see Compaction). `ErrExhausted` ends the turn with cause `provider_exhausted`. Any other error fails the turn.
- The stall watchdog ends a model call that reports nothing for `stream_idle_timeout_s` (default 300). A delta, an item, and `Sink.Alive` each reset it. `modelapi` calls `Alive` for each provider event with no delta, such as a keep-alive or a tool argument that still streams. The stall is retryable. A compaction summary and a goal evaluation have the same watchdog and the same stream retries. A negative value turns it off. A backend with `OwnsLoop` has no watchdog: its own tools can run silently for a long time.
- A response that the output cap cut off (`max_tokens` on the Anthropic wire, `max_output_tokens` on the Responses wire, `length` on the chat-completions wire) is an incomplete response, as in the Codex CLI. `modelapi` records no item for it and fails the call with `ErrRetryable` of class `stream_truncated`, so the retry rule above applies: after the wait, the model gets the same history again, up to `stream_max_retries` times, and after that the turn fails with `error_class` `stream_truncated` (a goal pauses on it). The loop runs no tool call of the response and never asks the model to continue. `Request.AcceptCut` makes the cut response the answer; a compaction summary and a goal evaluation set it.
- A response that the provider refused or filtered, or that the Responses wire reports as incomplete, fails the call as the Codex CLI fails it, and the same retry rule applies. A `response.incomplete` event with the reason `content_filter`, the Anthropic stop reason `refusal`, and the chat-completions finish reason `content_filter` fail with `ErrRetryable` of class `content_filter`. A `response.incomplete` event with any other reason except `interrupted`, or with none, fails with class `stream_truncated` and the message `incomplete response returned, reason: <reason>`. A `response.incomplete` event with the reason `interrupted` ends the response as a complete one. Every other stop reason, such as the Anthropic `pause_turn`, ends the response as complete. A compaction summary and a goal evaluation fail on a filtered response, because `AcceptCut` accepts only a response that the output cap cut off.
- Private backend state is a chain of `backend.state` records. Each record carries the head, the small fields that each save replaces. The entries (the Claude Code transcript mirror) are appended as chunk blobs of whole entries, at most 4 MiB each (an entry over 4 MiB is a chunk of its own), so no `SyncBatch` nears 32 MiB. A chunk key holds the backend, the fence of the owner, and an index that no chunk of that owner used before, so a fenced owner never overwrites the chunk of another owner. A record says whether its chunk continues the current chain or starts a new one (`restart`); a save whose entries do not extend the chain starts a new chain. `Apply` keeps the chunk keys of the current chain, and `Sink.State` reads them in order. A record with `blob_key` and no chain fields reads as a chain of one blob.
- The backend `Capabilities(model)` is the one place that resolves a context window: the configured `context_window_tokens`, else the `modelmeta` table, else the default of 128000 tokens with `WindowEstimated`. The turn, the compaction check, the model list, and the session gauge all ask it; nothing else reads `modelmeta` for a window, and `modelmeta` holds no default. The backend logs one WARN per process and model that runs on the default. A model that no configured provider serves fails with `model_unavailable` at create and at a settings change; an unknown window never does.
- A configured `context_window_tokens` always wins. The runtime does not learn a window from an overflow error: an overflow compacts and calls the model again, as the Compaction rules say.
- `Telemetry` carries the usage of one model call, its context reading, the subscription snapshot that the provider reported with it, and its cost, and the timing and request shape of the call (`Call`).

### Model API backend

`internal/backend/modelapi` is the one backend for every model API. It holds one `provider.Provider` client, and only the client differs between wires. `harness` builds the client from the provider entry. One function maps an entry with no type by its key.

| Entry | Client |
| --- | --- |
| `type: openai`, or the `openai` key with no type | `internal/provider/openai` (Responses, the Codex lane included) |
| `type: openai-compat` | `internal/provider/openaicompat` (chat completions, such as Bifrost) |
| The `anthropic` key with no type | `internal/provider/anthropic` (Messages) |
| `type: claude-code-cli` | `internal/backend/claudecode`, a delegated backend |

`Options.ModelTransport` wraps the HTTP client of each entry, and may supply the credentials of each wire. Each request sends a baseline response cap of 8192 tokens. A reasoning request can raise it (see Reasoning effort). `Close` reaches a client that pools connections through an optional `Close` method. Each provider package keeps its own SSE reader and status classifier, and the policy of a model call retry stays in `turn`. The one policy of a request retry is `provider.Do`.

#### Reasoning effort

An effort is `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`, or unset. `Session.Update` stores the effort as given; a settings change that names another value fails with `invalid_request`. `Runtime.Create` applies the same check to the `effort` of its request, so `POST /sessions` with another value fails with `invalid_request` and creates no session. The backend asks its client for the effort control of the model: the `anthropic` client reads the models.dev facts of `modelmeta` (a Bedrock-style id and a routing namespace read as the model they name), the `openai` client reads them too and prefers the levels of the live ChatGPT model list, and the other clients know none. The backend clamps the effort of each model request to the levels of that control: a listed level is kept, and an unlisted reasoning level becomes the highest listed reasoning level below it, else the lowest listed reasoning level; it becomes `off` only when the model lists no reasoning level and lists `off`. `off` and unset are never changed. A model with no facts, a model that lists no level, and every model of an `openai-compat` entry take at most `high`. `/thinking` says which level a request sends, or that `off` is not offered by the model. The clamped level reaches the wire as follows.

| Client | Wire |
| --- | --- |
| `anthropic` | A model with an `effort` control gets `output_config.effort` with the level, except for `minimal`, which sends the budget alone. A model with that control and no `budget_tokens` control gets `thinking.type` `adaptive`; any other model gets `thinking.type` `enabled` with a budget of 1024, 4096, 8192, or 16384 tokens for `minimal`, `low`, `medium`, or `high`, `xhigh`, and `max`; a model with both controls gets the field and the budget. `max_tokens` is raised to the budget plus 4096 tokens for a budget, and to 10000, 12000, 18000, 25000, 32000, or 40000 tokens for adaptive thinking at `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`; a larger cap is kept. An `off` effort sends the off thinking type of the model when the model has an `effort` control, no `budget_tokens` control, and an `off` level (its thinking default is adaptive), and sends no thinking otherwise. The off thinking type is `disabled`, except for a model that rejects `disabled` and takes `between_tools` in its place (`claude-sonnet-5-5`). An `off` request of such a model sends `thinking.type` `between_tools` and no `output_config.effort`, so the API default effort applies. The model then does not think before it answers, and the short updates that it writes between tool calls come back as thinking blocks. A stored thinking block stays in the history when the request thinks: it sends `thinking` other than `disabled` (`between_tools` included), or it omits the field and the model has an `effort` control and no `budget_tokens` control (adaptive thinking is its default; `claude-opus-4-7` and `claude-opus-4-8` are the exceptions, with thinking off by default). Any other request leaves the stored thinking blocks out |
| `openai` | `reasoning.effort` with the level; `off` sends none |
| `openai-compat` | `reasoning_effort` with the level; `off` sends `off` |
| `claude-code-cli` | `--effort` with the level, where `off` and `minimal` send `low`. The CLI takes `low`, `medium`, `high`, `xhigh`, and `max`, and the backend does not clamp |

#### ChatGPT subscription login

An entry that builds the Responses client can use a ChatGPT subscription in place of an API key. `auth: "chatgpt"` names the credential source. It is valid on an entry with `type: openai` or on the `openai` key, and not with `api_key_env`. A `base_url` of such an entry is `https`, or `http` on a loopback host. The Codex lane is the native `codex` provider, which needs no entry. Its entry is `{"type":"openai","auth":"chatgpt","base_url":"https://chatgpt.com/backend-api/codex","responses_path":"/responses","omit_response_params":["max_output_tokens","temperature","top_p","metadata"],"sanitize_tool_schemas":true,"models_from":"chatgpt"}`. `config` holds it in the one table of native entries, with the entry `{"type":"claude-code-cli"}` of the native `claude-code` provider and the empty entries of `anthropic` and `openai`. A `providers` entry of the same name replaces the native entry as a whole, so a deployment that points the lane at another URL writes the whole entry.

`harness login codex` creates the login with the OAuth client of the Codex CLI: the authorization code flow with PKCE by default, and the device code flow with `--device`. The browser flow prints the sign-in URL (and opens the browser unless `--no-browser`), listens on `127.0.0.1:1455` (else `127.0.0.1:1457`) for the callback at `/auth/callback`, and redirects to `http://127.0.0.1:<port>/auth/callback`. A callback with another `state` or no code answers 400 and the flow keeps waiting; the first callback with the right `state` exchanges the code, and the command exits. The device flow prints the code and `https://auth.openai.com/codex/device`, then polls for at most 15 minutes. When the issuer answers `slow_down`, the command adds 5 seconds to the polling interval and goes on. `harness login codex --status` prints the account, the expiry of the access token, and whether a refresh token exists, and never a token. `harness logout codex` deletes the file under the lock. It does not revoke the tokens at the issuer: there is no server-side revoke. The issuer is `https://auth.openai.com`; the user config key `codex_issuer` replaces it (no `HARNESS_` variable does, so `Validate` sees the value that is used), and a non-default issuer listens on a free port. After a successful sign-in, `harness login codex` prints `Signed in to Codex.`, the line that names the saved default model, and one line `Try: harness run -model codex/<id> -p "..."`, where `codex/<id>` is the default model of the Codex login (see Config). That model is one of the static `codex` list.

The login is the file `codex_auth_file` (default `auth/codex.json` beside `$HARNESS_CONFIG`, else in `~/.harness`; only the user file sets it) with `access_token`, `refresh_token`, and `account_id`. Its directory has mode 0700 and the file 0600. On unix, a load refuses a file that the group or other users can read, with an error that says to run `chmod 600` on it, and a save or a lock refuses an existing directory that the group or other users can reach, with an error that says to run `chmod 700` on it. A write goes to a temporary file in the directory, is synced, and replaces the file by rename, under an exclusive lock on `codex.json.lock`. Harness never reads or writes the files of the Codex CLI. The lock is an exclusive `flock` on unix. On every other platform (Windows) the lock fences nothing: only the goroutines of one process are fenced, so two Harness processes on one login can refresh at the same time, and the second write can leave a refresh token that the issuer rotated away. Sign in again after such a failure.

Startup reads no login. A request to the endpoint of the entry reads the file, keeps it by modification time and size, and sends `Authorization: Bearer <access_token>`, `ChatGPT-Account-ID` (the `chatgpt_account_id` claim of the token, else the stored account), and `originator: harness`. Only a request whose scheme, host, and path equal the Responses URL or the model list URL of the entry carries them, for HTTP and for the websocket handshake alike. The client of such an entry follows no redirect: a 3xx answer ends the request, so no header, and no API key, goes to the target. The entry never reads `OPENAI_API_KEY`. When the `exp` claim is within 5 minutes, or the backend answers 401, the client takes the lock, reads the file again (another process may have refreshed), and if the token is still stale posts `grant_type=refresh_token` to `<issuer>/oauth/token`. A token with no `exp` has an unknown expiry and refreshes only on a 401. The refresh keeps the old refresh token when the answer has none. It runs on its own context with a bound of 30 s (10 s while the access token is still valid), so a cancel of the request that started it never loses a rotated refresh token. The refreshed login stays in memory until the file holds it: when the write fails, later requests use the login in memory and each one tries the write again, until another process writes the file. One refresh serves every concurrent request of the process, and no request waits on a lock of the process while the refresh is on the network. After a transient failure of a refresh while the access token is still valid, the client waits min(30 s, then doubling, half of the remaining validity) before it tries again, and sends the old token meanwhile. Refresh failures have three classes. A transport failure, a 429, and a 5xx are transient: while the access token is valid the old token stays in use under the backoff, and once it has ended the request fails. Any other status (400, 401, 403, a redirect, any other 4xx), and a 200 answer that holds no access token, mean the login is unusable: the request fails with the login error, and the same refresh token is not tried again. A dial error of the websocket that is a login error fails the request too; it does not fall back to HTTP. A 401 sends the request once more with the refreshed token; a second 401 fails. A missing or unreadable file, a file without a refresh token, a refresh the issuer rejects (400, 401, 403), and a second 401 each fail the request with the permanent error `not logged in to Codex; run `harness login codex``, so the turn ends `failed` with error class `login_required`. A transport failure of the refresh while the access token is still valid keeps the old token. No error, log line, or event holds a token.

The model list of an entry with `models_from: "chatgpt"` that sets no `models` is the one catalog that is not static. An entry with `auth: "chatgpt"` and no `models_from` has the static list. An entry that sets `models` lists those ids and makes no fetch; a `models` list in a project file on a user provider with `models_from` also turns the fetch off. The first call of `Runtime.Models` sends `GET <base_url>/models?client_version=<version>` with the headers of the login, waits at most 3 s, and keeps the result for the life of the process. The version is that of a recent Codex CLI, because the backend hides a model that needs a newer client. The body is `{"models":[...]}`. An entry whose `visibility` is not `list` is not listed, as in the Codex CLI. The list is in the order of `priority`, lowest first. Each entry gives the model id (`slug`), its `display_name`, its `description`, and the reasoning levels of `supported_reasoning_levels`: `none` is `off`, and a level that harness has no name for is dropped. `xhigh` and `max` are named levels, so a model that lists them offers them. A name, a description, or levels that the entry lacks come from the models.dev facts of the same id. Callers that ask while a fetch runs share it, and a caller whose context ends leaves with the static list. A fetch that fails, times out, or returns no model gives the static `codex` list and one log line without a secret; a call tries again 5 minutes later. A missing login, and a 401 or 403 answer, give the static list with no log line; a call tries again when the stored access token is not the one of that answer. The fetch is keyed on `models_from`, not on `auth` and not on the provider name. `models_from` is valid on a `type: openai` entry with any auth; any value but `chatgpt` is invalid. Only `Runtime.Models` fetches it; `initialize` and the start of a command never call it.

#### Injected auth

`auth: "injected"` says that a credential-injecting server in front of the endpoint holds the credential and adds it to the request. It is valid on an entry that builds the OpenAI Responses adapter and on the native `anthropic` entry, and not with `api_key_env`. Harness holds no credential for such an entry. The adapter sends no `Authorization`, no `x-api-key`, no `ChatGPT-Account-ID`, no `originator`, and no user identifier. Harness sends no session identifier either: the Responses adapter sends a stable hash of the session id (the first 32 hex characters of its SHA-256) as `prompt_cache_key`, so the cache still routes and the raw id never leaves. The websocket pool keeps the raw id in the process. `extra_headers` still apply, so an entry can carry the placeholders that a server substitutes. A `base_url` of such an entry has no scheme rule, because Harness sends no credential to it. The client of such an entry follows no redirect: a 3xx answer is an error. The entry does not name the server: `base_url` need not point at it, and a forward proxy (`HTTPS_PROXY`) can add the credential.

`login_url` is the page where the user signs in. It is absolute and `https`, or `http` on a loopback host, carries no user information, and is valid only with `auth: "injected"`. It is optional. It appears in error text and in logs, so it holds no secret and no per-user value. Only the user file sets `auth`, `login_url`, and `models_from`; a project file that sets one of them fails to load.

The server answers a request that it cannot authorize with HTTP 401, and never with a redirect to a sign-in page. The adapter sends a 401 request again never and refreshes no token. The error message is `not logged in to <provider>; sign in at <login_url>`, or `not logged in to <provider>` for an entry with no `login_url`. A 401 of the websocket handshake of the Codex lane fails the request and does not fall back to HTTP. Cloudflare challenges, policy denials of the server, and plan limits of the backend give a 403, and a login resolves none of them. Any other status follows the rules for every provider. Authentication gives the error class of each answer.

The status probe is the model list request of the entry, sent with `extra_headers` and no credential: for the Responses adapter `GET <base_url>` with the last segment of `responses_path` replaced by `models` (with `models_from` `chatgpt`, the request of the model list of the backend). For the Anthropic adapter it is the model list request `<base_url>/v1/models` (see Authentication). A 200 is signed in, a 401 is signed out, a 404 or 405 means that the server has no status probe, and anything else is unknown. The probe uses the transport of the model calls of the connection, so a header or a proxy that the host sets for them reaches the probe too. The probe makes no model call. The server authorizes it as a model call: the same caller, the same credential lookup, and no cache of its answer. A model call that the server rejects soon after a 200 probe is a fault of the server, not a missing login (see Authentication). Startup rules hold: `initialize` and the start of a command send no probe.

#### Web search

`web.search` (default false) offers the native web search of the provider of the model. The provider runs the search inside the model call. It is not a Harness tool call: no `Source`, no `Request.Call`, and no hook sees it. A plugin cannot block, rewrite, or log a search with `tool.execute.before` or `tool.execute.after`, and the log holds no `tool_call` for it. An operator who needs a hook on every fetch of a page leaves `web.search` false. The tool name `web_search` in `AllowedTools` turns the search off for a session whose list leaves it out; a nil list keeps it on.

Each client declares the capability. A client that implements `provider.WebSearcher` reports `NativeWebSearch() bool`, and `Capabilities.WebSearch` of the backend carries it, so the session sets `turn.Request.SearchAllowed` when the config and `AllowedTools` agree, and each model call of the turn sets `WebSearch` from that and from the capability of the model of that call, after a settings change and the `chat.params` hooks, so a switch of the model in the middle of a turn changes the next call. No code compares a provider name. The `openai-compat` client has no capability and sends nothing.

| Client | Request | Result |
| --- | --- | --- |
| `anthropic` | The server tool `{"type":"web_search_20260209","name":"web_search","max_uses":5,"allowed_callers":["direct"]}` after the function tools. Without `allowed_callers` the model calls the search from code execution, which Harness does not offer | The `server_tool_use` block and the `web_search_tool_result` block that follows it become one part. `citations_delta` events become the `citations` of the text block. `usage.server_tool_use.web_search_requests` of `message_delta` is `web_search_requests` of the usage |
| `openai` (Responses, the Codex lane included) | The built-in tool `{"type":"web_search"}` after the function tools | The `web_search_call` output item becomes one part, and the `url_citation` annotations of the message become the `citations` of its text. The wire reports no search count |
| `openai-compat` | None | None |
| `claude-code-cli` | `WebSearch` in `--tools` and in `--allowedTools` | The `tool_use` frame of `WebSearch` and its `tool_result` frame become one part when the result arrives. The CLI reports no search count of its own |

The canonical form is the message part `web_search` (`message.WebSearch`, and `protocol.LogPart` of type `web_search`). It holds `call_id`, `query`, `results` (`title` and `url` of each source that the wire reports), an error code in `text` with `is_error` for a failed search, and `provider_data`. A text part may hold `citations` (`url`, `title`, and the cited passage in `quote`). A reader sees the same fields in a `protocol.MessagePart`. The part never holds a wire object, and a transcoder builds the wire request from it each time.

- `provider_data` follows the rule of the `reasoning` part, so it is no exception to the canonical rule: it is opaque, tagged with the provider family, and only the same family replays it, and `ProviderData.Get` bounds it at 256 KiB. Anthropic requires the encrypted result bodies of a search to come back unchanged, so `provider_data["anthropic"]` holds `{"content": <the content of the result block>}` and nothing else. A Responses client keeps the `web_search_call` item as it came. A part of the Claude Code CLI holds none.
- Replay to the same family sends the `server_tool_use` and `web_search_tool_result` blocks, or the `web_search_call` item. A family that finds no entry for it, a search that failed, and the chat-completions client send the part as text, with the engine-context sentinel of the query and the titles neutralized like any other text from the web: `[web search: <query>]`, the line `(search failed: <code>)` for a failed search, and one line `- <title> <url>` for each source, with the url in angle brackets. A search never goes back as a tool call.
- `pause_turn`. When the stop reason of an Anthropic response is `pause_turn`, the client resumes inside the same model call: it sends the request again with the content of the paused response added as an assistant message, as the API sent it, and adds no user message. The content of every leg joins the same message, and its text is streamed once. It resumes at most 3 times, and the usage of all legs adds up. A response that still pauses after the third resume ends the call as an incomplete response with the reason `pause_turn`. The backend records the usage and one item with the finished parts of every leg: the text, and each search that has a result. It drops a search call with no result, because the API must not receive a server call with no result. A resume request that fails at the HTTP level ends the call the same way, with the parts of the legs before it, unless the context of the caller ended. The context reading of that call is the prompt size of the last leg that finished. Then the call fails with `incomplete response returned, reason: pause_turn`, which is not retryable: a new call would run and bill the searches again, so the turn fails with no model call after it.
- Caching. With `web.search` on, the cached prefix of each wire is the same bytes on every model call of a session: the server tool is the last entry of the `tools` array, after the function tools, with fixed fields, so it moves no cache breakpoint (the breakpoints sit on the last system block and the newest block); a Responses request sends the same tools, instructions, and `prompt_cache_key`; and the Claude Code system prompt is the same text on every turn. A replayed search sends its stored `provider_data` unchanged, and the text fallback is a pure function of the part, so the history of a request starts with the bytes of the history before it.
- Citations stay in the log and go back to no provider. The wire needs only the text, and the encrypted index of an Anthropic citation is not canonical data.
- ACP shows a search as one `tool_call` update, sent when the assistant message that holds it completes, with the status `completed` (`failed` for a failed search), the kind `fetch`, the title `Search: <query>`, the raw input `{"query": ...}`, and one `resource_link` content item for each source. No `tool_call_update` follows.
- The context reading of a call is the prompt size of its newest response: the input and cache counts of `message_start`, or those of `message_delta` when `message_start` reports zero (a gateway that moves the usage). The input of `message_delta` also counts every internal step of a server tool, and the legs of a `pause_turn` call each count their own, so the usage keeps those sums for billing and never feeds the gauge, or auto-compaction would fire after a search.
- `web_search_requests` of `protocol.Usage` counts the searches that the wire reports for a call, which Anthropic bills for each request. Only the `anthropic` client reports it. It adds into the usage of the session like the token counts.
- The `get_conversation_history` tool and the evaluator transcript of a goal show a search as its query.

### Third-party harnesses

A third-party harness is a `Backend` with `OwnsLoop`, `OwnsContext`, and `Steering` set, behind one adapter contract. Claude Code is the one built. A new harness adds one package and touches nothing else.

| Concern | Contract | Claude Code |
| --- | --- | --- |
| Turn | One harness turn is one external turn, except the second turn that the external harness runs for a `steer` input it took after its result (see Steer below) | `--resume` print run |
| Items | External items become `item.completed` | stream-json frames |
| Steer | A `steer` input reaches the running turn | stdin |
| Interrupt | Stops the external turn; reports `interrupted` | SIGINT |
| Requests | Questions and approvals become `request.opened`; the resolution goes back | the tool `ask_user_question` of the MCP endpoint |
| State | External session id and transcript mirror are `backend.state` records with appended entry chunks | `--session-mirror` |
| Tools | The external harness runs no built-in tool that acts on the machine or the network. A built-in tool that the CLI refuses (no such tool available) is a failed tool call and the turn goes on; any other result of a built-in tool, an error result also, fails the turn. Harness tools reach it through a harness-hosted MCP endpoint, so each call runs through `Request.Call` and the hooks; `AllowedTools` applies. The endpoint lists a tool with the annotation `readOnlyHint` true when `protocol.ToolSpec.ReadOnly` is true, so that the external harness runs parallel calls of that tool at once. The built-in tools `read_file`, `glob`, `grep`, `ls`, `read_tool_result`, `session_info`, and `get_conversation_history` set `ReadOnly`; an MCP tool sets it from its own `readOnlyHint`; an embedder tool of `Options.Tools` sets it in its `ToolSpec`; a plugin tool and every other tool leave it false. Calls with one key still run one at a time. With `web.fetch` true, `web_fetch` is one of these tools, so Harness makes every fetch and a plugin can deny it by URL. An unrestricted turn gives `Config.MCPServers` to the external harness beside that endpoint. The external harness keeps its built-in tool `ToolSearch`, which only loads the schema of an MCP tool and touches nothing on the machine or the network, so it defers MCP tool schemas and the cost of a turn does not grow with the number of MCP tools. `mcp_tool_loading` sets `ENABLE_TOOL_SEARCH` of the external harness: `eager` is `false`, `lazy` is `true`, and `auto` or an unset value is `auto`. A `tool_loading` of one server does not apply | `--tools` with `ToolSearch`, `ENABLE_TOOL_SEARCH`, `--mcp-config` with `--strict-mcp-config` |
| Prompt | The external harness keeps its own base prompt, tool-batching text, and `AGENTS.md` reading. The backend adds, in one value, a segment that names the harness tools, the `append_system_prompt` entries, the skill segment of the session (`Request.Skills`), and the history directive when it applies | one `--append-system-prompt-file` value |
| Context | The external harness compacts; harness logs `compaction.applied` with `by_backend` | `/compact` |
| Telemetry | Usage, cost, and context window arrive through `Sink.Telemetry` | `result` event |

`internal/backend/external` holds what every adapter shares: process supervision, the line-protocol transport, the mirror writer, and the MCP bridge. `internal/backend/claudecode` holds only the mapping in the table.

- History bridge. A session may move between a backend that owns its loop and any other backend. A stream-json input cannot seed prior history, so each turn of such a backend has the harness tool `get_conversation_history`, which reads the live history page by page and which no `AllowedTools` list hides. When another provider recorded a message after the newest message that this backend saw, the backend adds one line to its system prompt value that tells the CLI to call the tool before it answers. A turn belongs to the provider of its model at its start, even when a settings change moves the model in the middle of it.
- Web search. A turn with `Request.WebSearch` adds `WebSearch` to `--tools` and `--allowedTools`, and the tool-call guard allows it. The CLI runs the search inside its own model call, so no hook sees it. The backend holds the `WebSearch` call until its result frame arrives, then records one assistant item with one `web_search` part: the query of the call and its sources. The sources are the `results[].content` list of the `tool_use_result` of the result frame; when the frame has none, they are the list that follows `Links: ` in the result text. A result with neither gives a part with no sources, and an error result gives a failed part. The segment of tool names tells the CLI that it has no `TodoWrite`, `WebFetch`, or `Skill` tool, and names `WebSearch` among the absent tools only on a run that does not offer the search. A run that does not offer the search fails the turn when the CLI lists or calls `WebSearch`.
- Tool calls. The backend records the assistant item that holds a tool call when the `tool_use` frame arrives, and a tool result when its frame arrives, so `View.Messages` and the event stream show a running tool before its result. A later frame of the same API response starts a new item, so parallel calls of one response are separate items in wire order.
- Text deltas. Claude Code runs with `--include-partial-messages`. The backend forwards each `text_delta` and `thinking_delta` of a `stream_event` frame as an `item.delta` frame, and the `assistant` frame that follows sends only the part that the deltas did not send, so the deltas of a block add up to its text. A reply that the CLI drops and retries starts a new live item, and the dropped one never completes: the backend sends an `item.discarded` frame with the `item_id` of the dropped item before the first delta of the new one, and a client removes that live item. A tool call has no delta. The deltas are live only: the log, the replay, and `View.Messages` hold the finished blocks.
- Placeholder result. The empty zero-turn `result` that the CLI prints for a queued task notification is skipped when the run has recorded no assistant item, and adds no usage. After an assistant item it ends the turn with the usage of that result.
- Questions. The CLI asks with the harness tool `ask_user_question` (see Questions of a model API session). The bridge holds the call until the backend recorded it as an item, then opens the request through `Request.Ask` and never replies. The backend then ends the CLI run with an interrupt and the turn ends `awaiting_input`. The result that the CLI writes for the cut call is not recorded, and neither is the interrupt notice; the call stays open until the answer. A turn that an answer starts sends the text of the recorded result to the CLI as its prompt, in place of an input.
- Steer. The backend writes a `steer` input to stdin, and promotes it, only after the response ends and while a tool call of that response waits for its result: the CLI takes a queued message at the next tool result. A call to `ask_user_question` is not a waiting call, because the question ends the turn and the CLI sends the model no tool result for it; a question call in the response keeps the input queued. A `steer` input that arrives at any other time, such as in a text-only reply, stays queued and unpromoted, and the next turn carries it as it carries a `queue` input; the input joins the turn at the next tool call when one starts before the turn ends. When the CLI ends the turn with a result and no tool result came after the write, it runs the message as a second turn and writes a second `result`. The backend then reads on, records the items of the second turn in the same turn, and ends the turn at that result, with the cost that the first result did not report; a CLI that exits first fails the turn. This keeps the one-turn-one-input-batch event model: the input stays promoted to the turn that took it, so no event un-promotes an input. A tool result that the CLI wrote before it read the steer line still counts as delivery, so in that race the CLI runs the input as a next turn that the backend does not record. A failed first result takes no second result: the turn fails and the promoted input gets no answer.
- Context and cost. A Claude Code turn reads its context gauge and its cost from the `result` frame. The gauge reading is the prompt size of the newest assistant frame, or the usage of the `result` frame when no assistant frame carried usage, with the window of `modelUsage`. A `compact_boundary` with positive `post_tokens` sets the reading until a later assistant frame reports usage. The `result` frame of each turn adds its `total_cost_usd` to `session_cost_usd` of `subscription_usage`, also when it fails the turn. A second `result` of one CLI run adds only the part of its running total that the earlier `result` did not report. A `result` frame sets the reading also when it carries no prompt tokens. The window of a reading stays when a later reading of the same source reports none; a change of model clears it.
- Compaction summary. The adapter logs the summary text of the CLI without the reasoning block, the summary tags, and the continuation framing that the CLI adds for its model (the opening "This session is being continued from a previous conversation" and the closing instructions). A continuation prompt with a known opening and an unknown closing keeps its body. Text with an opening that the adapter does not know stays as the CLI wrote it, because a model that takes over after a switch of backend reads it as the summary. A compact boundary with no summary frame logs the boundary text.
- Failure. A failed Claude Code turn runs the CLI once and fails with the text of the `result` frame, or with the exit of the CLI when it wrote none; `stream_max_retries` does not apply to it, and the error keeps its retryable class for a goal.

### Warm-up

`turn` declares an optional `Warmer` interface, `Warm(ctx context.Context, req Request) error`, and an optional `WarmGate`, `CanWarm(model string) bool`. The session calls `Warm` once on create and on wake, fire-and-forget under the session context. The backend router forwards both to the backend of the model, and only `internal/backend/modelapi` warms, through an optional `Warm` method of the client, for the Codex websocket transport. The first turn waits for an in-flight warm-up. An interrupt of that turn while it waits cancels the warm-up, which logs `cancelled`. No warm-up state lives on the session.

### Log lines of a model call

`turn` logs `turn_metrics` at INFO once for each model call that ended its response and whose backend reported `Telemetry.Call`, so one place owns the line for every backend. A call that failed before it ended its response, a compaction summary, and a goal evaluation log none; a cut, filtered, or incomplete response logs its line. Its keys are `session_id`, `model`, `ttft_ms`, `stream_ms`, `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `system_len`, `tools_count`, and `retry`; a transport that reports a request mode adds `request_mode`, `complete_input_items`, `sent_input_items`, `previous_response_used`, and `chain_recovered`; a backend that does not own the loop adds `service_tier` and `effort` when the session sets them, and `chain_refusal`, `chain_refusal_detail`, and `chain_refusal_item` for a call that did not chain. `session` logs `startup_prewarm` at INFO for a warm-up, with `session_id`, `status` (`started`, then `ready`, `failed`, `timed_out`, or `cancelled`; `consumed` or `stale` on the first model call after a `ready` warm-up), `duration_ms`, and `age_ms`. `harness run` sets a JSON logger as the default, so both lines are JSON on stderr in `run` and in `serve`.

## tool, prompt, and config

### tool

There is no tool registry. The tools of a runtime are a `[]turn.Tool`. The runtime joins them with the tools that can change between model calls into one `turn.Source` for each session.

```go
package turn

type Tool interface { // the same method set as harness.Tool
	Spec() protocol.ToolSpec
	Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error)
}

// A Tool may also implement these. Alone: the call runs with no other call of
// its model call in flight. Keyed: calls with the same non-empty key run one at a time, in call order.
type Alone interface{ Alone() }
type Keyed interface{ Key(c protocol.ToolCall) string }

// Restrict returns the tools that names lists, or every tool when names is nil.
func Restrict(tools []Tool, names []string) []Tool

type Source interface {
	Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, model string) Toolset
}

type Toolset struct {
	Tools    []Tool // described to the model
	Deferred []Tool // not described; the model may call them
	Prompt   string // follows the system prompt
	Notices  []Notice // statuses the model reads inside the conversation, not in the system prompt (MCP tools)
	Hooks    Hooks  // run around each tool call; nil: none
}

type Notice struct {
	Kind    string
	Text    string // the status now; empty: nothing to report
	Cleared string // pinned once when Text turns empty after a status with text
}
```

- `New` builds the tool list from `Options.Tools`, the `goal` tool with `goal_evaluator_model`, and, with a `WorkDir`, the `process` and `task` tools. An empty or repeated name fails `New`. A name of `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `ls`, or `bash` does not fail `New` with a `WorkDir`; see Built-in tools. The built-in tools of a `WorkDir` belong to each session; see Built-in tools.
- The runtime builds one `turn.Source` for each session when it loads the session. In order, it gives the runtime tools, with `goal` and `task` bound to the session ID and no `goal` for a child; `goal` only for a backend that does not own the loop; the file tools of the `WorkDir`, `session_info`, and `web_fetch` while `web.fetch` is true, for every backend; `internal/tool/mcpsrc`, except for a backend with `OwnsMCP` on a turn with no `AllowedTools`; and `internal/tool/pluginsrc`. The actor adds a last `Source`: `get_conversation_history` for a backend that owns the loop, which no `AllowedTools` list hides; and `ask_user_question`, for every backend, on a turn that may ask a question (see Questions of a model API session), then `read_tool_result` with the retention hooks, for every backend. `turn.Sources` joins the `Source`s, and `turn.Run` takes no other tool list. Each `Source` gets the `AllowedTools` of the session for each model call and applies `turn.Restrict` once.
- The tools of a model call keep one order from call to call, because the tools come first in the cached prefix of every provider and a changed byte invalidates the whole prefix. A `Source` returns its tools in a fixed order and never ranges over a map: `mcpsrc` orders the servers by name and the tools of a server by name.
- `turn.Sources` chains the `turn.Hooks` of its `Source`s in order: `Before` runs in order and a deny ends the chain, and `After` runs in order.
- The loop runs the tool calls of a response at the same time, up to 8 at once. Results reach the log in call order: a result is recorded as soon as it and every call before it have ended. A tool that is `Alone` waits for every call before it and holds back every call after it. The calls of a `Keyed` tool with one key run one at a time in call order: `read_file`, `write_file`, and `edit_file` key on the resolved path with symlinks resolved, `process` on the process name, and `task` on the target session. `goal`, `model`, and `mcp` are `Alone`. `Before` and `After` run around each call, and calls that run together run their hooks together, so a hook keeps no state across calls that it does not guard. A panic in a tool is an error result of that call. An interrupt cancels every running call and starts none; every call that ran has its own result recorded in call order, including a call that the interrupt cut short, and a call that did not start gets the result of an interrupted call. When only the step ends, as in a handoff, running calls finish and no call starts. A tool error, or a call to a tool that the model may not call, is an error result that the model sees.
- An embedder tool receives its session, the root of the session tree, and the turn in `harness.Call`. The runtime binds them for each model call, also for `AloneTool.Alone` and `KeyedTool.Key`. The `goal` and `task` tools hold the session ID that the runtime binds when the session starts. `turn.Tool` takes a `protocol.ToolCall` and no session.

Agent profiles name a kind of child, in the agent format of Claude Code so one file serves every backend: `name`, `description`, `tools` (comma separated; omitted allows every tool of the parent; the names `Read`, `Write`, `Edit`, `MultiEdit`, `Glob`, `Grep`, `LS`, and `Bash` of Claude Code read as `read_file`, `write_file`, `edit_file`, `edit_file`, `glob`, `grep`, `ls`, and `bash`, and any other name is left as written), `model` (a ref or an alias; omitted or `inherit` keeps the model of the parent), and a prompt body. `color` is read and ignored.

- `internal/prompt.Profiles` reads `*.md` in each of `agent_defs_dirs` (default `<WorkDir>/.agents`; a relative path joins `WorkDir`) at each spawn, beside the built-in profiles, which a file of the same name replaces. The built-in `general-purpose` allows every tool of the parent. The built-in `explore` and `plan` allow only the read-only file tools (`read_file`, `glob`, `grep`, `ls`) and `session_info`, and `plan` asks for an implementation plan.
- A file with a key that the format does not know is skipped with a WARN log line, unless the file has another error too, and then the load fails with the other error. A line under an unknown key that holds no colon is such an error, so an unknown key with a list or a block of text under it fails the load; a line under it that holds a colon reads as one more unknown key. Any other file that is not valid, such as one with no `name`, fails the load, and the error names the file: the spawn fails its tool call. A name that two files repeat, in one directory or across `agent_defs_dirs`, fails the load the same way, and the error names both files. A session that exists opens as before, with the profile of the first file of that name.
- Tool names differ by backend, so a spawn keeps only the names of a profile that `known` accepts for the model of the child: a runtime tool, a plugin tool, an MCP tool, or a built-in tool of its model. A profile left with no tool logs a WARN line.
- A profile applies through the allowed tools of the child (`turn.Restrict`) and its body as the last segment of the system prompt of the child. The runtime reads the profile once, when it starts the child, and the spawn uses that same read for the model and the tools of the child.

### Built-in tools

<a id="questions-of-a-model-api-session"></a>Questions of a model API session. A turn that may ask a question (`Options.AskUserQuestion` is set, the session is not a child, and it has no active goal) offers the tool `ask_user_question`, with or without a `WorkDir`, on every backend. The rest of this paragraph is the harness loop; a backend that owns the loop gets the call over its tool endpoint (see Questions in the table of third-party harnesses). `AllowedTools` applies to it by name, and a plugin tool or an embedder tool with its name fails. Its input is the input of the `AskUserQuestion` tool of Claude Code: `questions` holds one to four items, each with `question`, `header` (at most 12 characters), `options` (two to four items with `label`, `description`, and an optional `preview`), and `multiSelect`. The question texts of a call are unique, and the labels of a question are unique. The input may also hold `metadata` (with `source`), `answers`, and `annotations`; the loop ignores them. The payload of the request is the input. The loop runs the other calls of the model call first and then gives the first call to the tool to the `tool.execute.before` hooks. The tool is offered again at each model call under the same condition, and a call made when the condition no longer holds (a goal became active) gets an error result. A deny, an input that is not a question (after the hooks rewrote it), and each later call to the tool in the same model call get an error result, which passes the retention hook like any result. A turn whose context or step ended while the hooks ran opens no request. The `tool.execute.after` hooks see the rewritten input. Any other call opens a request with `Sink.Ask` and the rewritten input as the payload, and ends the turn `awaiting_input`; the plugin `tool.execute.after` hooks run on the answer. An answer starts a turn with no input whose first step, before the first model call pins anything, records the result of the call from the questions of the request payload, `User has answered your questions: "<question>"="<answer>", ... You can now continue with the user's answers in mind.`, and then calls the model. A dismissal gives the error result of the Request lifecycle.

With a `WorkDir`, each session gets the built-in tools `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `ls`, `bash`, `read_tool_result`, and `session_info`. A plugin tool with one of these names fails, and so does an embedder tool named `read_tool_result`, `session_info`, or named `model` with a `WorkDir`, whether or not `model_tool` is false, or named `web_fetch` while `web.fetch` is true. An embedder tool named `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `ls`, or `bash` replaces that built-in tool: the session offers one tool of that name, with the schema of the embedder tool. This holds when `AllowedTools` is a list; the name in the list selects the embedder tool. When `AllowedTools` is nil, the built-in tool is allowed too, and `Create` and `Open` fail with `invalid_request` that names the tool.

- `web_fetch`. With a `WorkDir` and `web.fetch` true (see config), each session also has `web_fetch`, for every backend. With `web.fetch` unset or false, `known` is false for the name: no session has the tool, and a plugin tool or an embedder tool may use the name. It is off by default because it sends requests from the machine of the user. A backend that owns the loop gets it through the harness-hosted MCP endpoint like the other built-in tools, so its calls run through `Request.Call`, the hooks, and retention. `tool.execute.before` sees the input `{url, format, max_bytes}` of every fetch and may deny it by URL; `tool.execute.after` and the retention hook see its result like the result of any tool.
  - Input. `url` is an `http` or `https` URL with a host and no user name or password. `format` is `markdown` (default), `text`, or `raw`. `max_bytes` (1 to 10485760, default 200000 when absent; 0 is an error) bounds the text of the result. Another scheme, a bad `format`, or a bad `max_bytes` gives an error result.
  - Request. One `GET`, with no body and no cookie. The only headers are `User-Agent: harness/<version> (web_fetch)` (`harness` when `Options.Version` is empty), `Accept`, and the `Accept-Encoding` of the Go transport. Harness adds no credential; an egress proxy may. A fetch of a host for which the proxy holds a credential is therefore authenticated by the proxy, as the requests of the other Harness clients are. A redirect drops the `Referer`.
  - Proxy. The tool takes the proxy from `http.ProxyFromEnvironment` and the environment of the process, as the other Harness clients do, so `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and the httpoxy guard behave as in the standard library, and `Options.Getenv` plays no part. The choice is made for each request, so each redirect hop makes it again. When a proxy applies to the URL, the request goes through it and the proxy owns the dial and the egress policy. A proxy that intercepts TLS works when its CA is in the system root pool; the tool has no option to skip the verification. Before it sends, the tool still checks what it can: the scheme is `http` or `https`, the URL has no user name or password, a literal IP passes the address rule below, and a host name is resolved and refused when any resolved address is not public. Only a lookup answer of not found (NXDOMAIN) sends the name on, because the proxy may resolve names that this host cannot; any other lookup failure, such as a refused connection or a timeout, refuses the request. This check is best effort. DNS rebinding protection holds only without a proxy, because the proxy resolves and dials for itself and may reach an address that the check did not see. With a proxy, the egress policy belongs to the proxy. A proxy that cannot be reached fails the request, and the tool never falls back to a direct dial. Every limit below still applies. `Options.WebTest` is a test seam for the loopback rule, the resolver, the proxy, and the root CAs; the config has no such key.
  - Addresses. Without a proxy, the tool dials public addresses only. A custom dial resolves the host, refuses the request when any resolved address is not public, and dials the address it checked, so a second DNS answer cannot bypass the check. A literal IP in the URL gets the same check, also with a proxy. Not public are loopback, private (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`), link-local, multicast, unspecified, `0.0.0.0/8`, CGNAT (`100.64.0.0/10`), `192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4`, `2001::/32`, `100::/64`, `64:ff9b:1::/48`, `::/96` (IPv4-compatible), and `fec0::/10`; an IPv4-mapped IPv6 address, a `64:ff9b::/96` address, and a `2002::/16` address count as the IPv4 address that each carries. A refused host gives the error result `web_fetch: refused: <host> is not a public address`. `Options.WebTest.AllowLoopback` lets 127.0.0.0/8 and `::1` pass, and only tests that fetch from a local server set it; every other range stays refused.
  - Redirects. At most 5. Each hop gets the same check (the dial check, or the check before the send with a proxy), and a hop to a scheme other than `http` or `https`, or to a URL with a user name or password, is refused. The sixth redirect gives `web_fetch: stopped after 5 redirects`.
  - Limits. The whole call, redirects included, ends after 30 s. The tool reads at most 10 MiB of the decoded body and says so in the result when the body is larger. The text of the result is cut at `max_bytes`, at a rune boundary, with a note that gives the bytes left. The tool parses the first 2 MiB of an HTML body only, and a result note says when it cut the body there. The HTML converter writes each line once with its quote and list prefix, stops at the first block that passes `max_bytes` (a run of inline text, a link, and a table stop at `max_bytes` too), checks the call context as it goes so the 30 s limit covers it, and reads the text of any subtree nested deeper than 48 levels as one run, so the work and the output stay linear in the page. A page that stops the converter at `max_bytes` gets a note that says the page has more content.
  - Content. The type is the `Content-Type` of the response, else the sniffed type of the body. An HTML page (`text/html`, `application/xhtml+xml`) becomes markdown or plain text from its parsed tree: it drops `script`, `style`, `noscript`, `template`, `svg`, `canvas`, `iframe`, `object`, `embed`, `head`, `nav`, `footer`, `select`, `button`, and any element with `hidden` or `aria-hidden="true"`; keeps headings, paragraphs, lists, code blocks, quotes, and tables; and, for markdown, keeps emphasis and renders each link as `[text](absolute-url)`, which skips a fragment, `javascript:`, and `mailto:` link. `raw` returns the source. Other `text/*` types, JSON, XML, and JavaScript pass through, with invalid UTF-8 replaced by U+FFFD; the tool converts no other charset. The converter drops skipped and hidden elements also from the text of a code block and from the text of a subtree nested deeper than 48 levels, and it escapes `|` in a table cell only for markdown. A PNG, JPEG, GIF, or WebP image that decodes as the format its type declares comes back as the line `image (<type>), <n> bytes, <w>x<h> pixels` and a blob, as `read_file` returns an image. Any other type gives `binary content (<type>), <n> bytes: not shown`.
  - Result. The text starts with `URL: <final URL>`, `Status: <status line>`, `Content-Type: <value>` when the response has one, and `Title: <title>` for an HTML page that has one, then a blank line and the content. A status of 400 or more sets `is_error` and keeps the body. A refused address, a refused scheme, a timeout, and a failed request give an error result with no URL or header in the text.
  - Host. A host with no direct network sets `HostLimits.NoNetwork` (see Host limits), and the runtime then offers no `web_fetch`. The WebAssembly module sets it, because a page has no dial for the address check. A config that sets `web.fetch` stays valid there and gets no tool, and a page that needs a fetch passes its own tool in `tools`. The `!js` build holds the wiring of the tool, so the module links neither the tool nor its HTML parser.
- The `model` tool. With a WorkDir and `model_tool` not false (default true), each session also has the `model` tool: `status` reports the model, the aliases, and the providers, `list` reports the providers and the aliases alone, and `set` takes a ref or one alias and calls `Session.Update`, so the model changes at the next model call. The providers are the registry: the native `anthropic`, `openai`, `codex`, and `claude-code`, and each configured entry. A native provider needs no entry, and a `set` to it builds its client with the key of its default environment variable (`codex` and `claude-code` use a login, not a key). An unserved provider fails with the valid aliases and providers listed, and a session has no action that clears its model. Each provider has a `billing` of `subscription` for `claude-code` and `codex`, and `api` for any other. A backend that owns its loop gets the `list` action alone, because `set` would move the model of the turn that calls it.
- `session_info` takes no argument and reports the model, the effort, the service tier, and the usage of its session from the view; the system prompt that the newest turn sent, with the MCP and plugin segments of its newest model call (a backend that builds its own prompt reports the prompt that it sent through `Request.Sent`, and then the report holds that prompt alone); the tool names of that call; the instructions files and the skills that the session read when it started; and the plugin inventory. It reports what the session loaded and sent, so a file that changes later does not change the report.
- Without a `WorkDir`, a session has none of them, and no result is retained.
- The runtime gives each session its own file tools in its `Source`, because the `write_file` guard belongs to one session: `write_file` overwrites only a file that the session read or wrote, with no change on disk since. The guard is in memory, so after `Open` the model reads a file again before it overwrites it. The tools cannot be one `Options.Tools` list for the runtime.
- The actor builds `read_tool_result` over itself, so the tool gets its dependencies when it is built. `session.Config.Retain` turns this on, and the runtime sets it with a `WorkDir`.
- A backend that owns the loop, such as Claude Code, gets every one of them, with retention. It runs no built-in tool of its own that acts on the machine or the network: the file, search, shell, fetch, and agent tools of the CLI are off. It calls the built-in tools of the `WorkDir` through the harness-hosted MCP endpoint, each call through `Request.Call`, so the hooks see it and the `write_file` guard belongs to the session. `Request.Call` runs the calls of one key (the resolved path of a file tool, the name of a process) one at a time, in the order they arrive, as the batch of a harness loop does, and retains each result in the `Join` step, one result at a time, so a result above the retention size reaches the CLI as its preview and handle and stays under the output limit of the CLI. A blob of a bridge result (the image of `read_file`) is kept by call ID, and the adapter stores it through the turn when the result frame of that call arrives, so the recorded tool result holds the blob part, ACP shows the image, and a model API backend replays it. The adapter fails the turn when the init frame lists a tool that is not an MCP tool or `ToolSearch`, and when a tool of that kind returns a result. The CLI tool `ToolSearch` is allowed on every run: a call to it and its result are logged as a tool call and a tool result, and the result names the tools that it loaded. A call to such a tool whose result is the CLI's own refusal (no such tool available) is a failed tool call under the name that the CLI gave, and the turn goes on. The refusal is an error result whose text is `<tool_use_error>Error: No such tool available: <name>` for the name of the call, followed by `.` and more text for a built-in tool that the CLI disabled, or by `</tool_use_error>` alone for a name that the CLI does not know, such as the bare name of a harness tool that `ToolSearch` has not loaded, in a frame whose `tool_use_result` is a string; a tool that ran gives an object there. Any other result of such a tool, an error result also, fails the turn. An assistant frame, a user frame, or a success result before the init frame fails the turn; an error result before it fails the turn with the text of the result.
- A file-size cap of 20 MiB bounds one file: `read_file`, `edit_file`, and the `write_file` guard read at most that many bytes of a file and fail on a larger one. Because calls run at the same time, a budget of 64 MiB for each session also bounds the file bytes that those three hold at once. A call reserves the size of its file before it reads, waits in order when the budget is full, and a file above the budget takes the whole budget and reads alone.
- `read_file` reads an image as the summary line `image (<type>), <n> bytes, <w>x<h> pixels`, and the image itself as a blob of the result. `bash` adds the variables that the `shell.env` hooks of the plugins return to the environment of its command.
- Tool-result images. A `protocol.ToolResult` holds `Text` and `Blobs`, each blob a media type and its bytes; `read_file` of an image and an MCP result with image, audio, or resource-blob content return blobs. A turn stores each blob with `Store.PutBlob` under `toolblob-<sha256>` before it appends the `item.completed` whose `tool_result` part holds `blobs` (`media_type`, `blob_key`, `bytes`, never the bytes), and `Sync` carries the blob in the batch of that record. A model API backend reads each blob through `turn.Request.Blob` and sends it inside the tool result, so a replay and the next owner send the same image. When the `Store` lacks the bytes of a blob, the backend logs a WARN and sends the text without that image. Retention counts the text of a result only. A reader of the history that shows text (`View.Messages`, `get_conversation_history`, the `log` of `task`) shows no blob.

### Tool-result retention

`internal/toolresult` holds the retention of large tool results and `read_tool_result`.

- A tool result above 16384 bytes has its secrets masked, goes to a blob, and the history holds a header with a `trh_N` handle and the first 16384 bytes. A result that fits after the mask stays inline. `read_tool_result` reads the blob back by line window or literal search, bounded by `max_bytes`. Its own result is never retained.
- The `Source` that the actor adds to an agent turn has a `turn.Hooks` that retains each result after the plugin hooks, so the blob holds the text that the model would see. It retains in the `Join` step, which a batch runs as it records each result in call order, not in the `After` of a call that runs beside others, so handle numbers follow call order and two results never share a handle or a blob. A backend that owns the loop retains each result of `Request.Call` as the call returns, one at a time, so its handle numbers follow the order in which its calls complete. The turn records the preview, and the next model call of the turn never carries the whole result. The blob is written outside the actor, and the record is appended only while the turn runs.
- A `tool_result.retained` record names the blob, and `Sync` carries the blob with the record, as for `backend.state`. The handle numbers count these records, so a replay and the next owner continue the count. The blob key is the handle and the fence seq, so a fenced owner never overwrites the blob of the next owner.
- A turn whose allowed tools omit `read_tool_result` retains nothing, so a preview never names a tool that the model cannot call.
- A result that would take the retained total of the session above 4 MiB keeps its preview with a notice and no handle, and nothing is written. A failed write keeps the whole result.
- The result of a done child in a report is retained the same way when it is larger than 4096 bytes once masked: the preview is 4096 bytes, the tool name is `task`, and the record joins the append of `child.settled`, so a handle always names a blob that exists. The handle takes the next number at that append, and a result of the session that is retained at the same time takes the next free number. The blob key is `report-<child turn id>-<fence seq>`, because the handle has no number before the append.
- Each compaction record holds an index of the newest 32 retained results in its `index` field, and the model of the harness loop reads it after the summary, so a handle stays reachable after its preview folds. A backend that owns the context compacts on its own and never reads that index, so after its compaction a handle is reachable only when its own summary keeps it.

### MCP tools

`internal/tool/mcpsrc` gives the tools of `Config.MCPServers` to each model call. `turn` declares the one seam: a `Source` returns a `Toolset` of described tools, deferred tools, and a prompt segment. The turn reads it before each model call, so a tool that the model loads in a turn is callable on the next request of that turn. A backend that owns the loop gets every tool described. A backend with `OwnsMCP` gets no MCP tool on a turn with no `AllowedTools`: the runtime asks no server, and the backend gives every server to the external harness. With `AllowedTools`, the allowed MCP tools reach it through the harness-hosted MCP endpoint.

- Connection. The first model call of a session that the runtime serves MCP tools for connects every configured server, in parallel, once for each runtime. `connect_timeout_s` (default 15) bounds each attempt. A server that fails its first attempt retries in the background: the wait before the first retry is 1 s, it doubles for each failed retry, and half of each wait is random. After 3 failed retries the server is parked until `mcp(action="connect")` makes one more attempt. One server has one attempt at a time. A connected server is never dialed again. The wait runs on the clock of the process. A unit test fakes the clock with `testing/synctest`. The e2e runtime host calls `mcpsrc.HoldRetries` before it builds a runtime, so a background retry never ends during a scenario; the hold applies to every `Source` of the test process, and the serve binary has no such hook. `Runtime.Close` closes every connection and stops each stdio server. The classified reason of a failed connect is `initialize timed out`, `initialize cancelled`, `connection refused`, `connection failed`, or `initialize failed`.
- Names. Each tool is `mcp__<server>__<tool>`. With `mcp_servers`, an embedder tool named `mcp`, `list_mcp_resources`, `read_mcp_resource`, or `mcp__…` fails `New`. `AllowedTools` restricts these tools and the deferred list. MCP needs no `WorkDir`.
- Deferral. The mode of a server is its `tool_loading`, else `mcp_tool_loading`, else `eager`. `lazy` defers each tool of the server. `auto` defers when the connected servers have more than `mcp_tool_loading_threshold` tools (default 20). The prompt segment lists the first 200 deferred tools with the first line of their descriptions. The model loads a tool with `mcp(action="select")` and finds one with `mcp(action="search")`. A tool is loaded when the history holds a `select` that names it or a call of it, so a replay or the next owner loads the same tools with no new event kind. A compaction that folds the call defers the tool again.
- Instructions. The segment starts with `<mcp_instructions>`: the instructions of each server that the first connect reached, with its allowed tool names, and one line about the resource tools. It never changes after the first connect. The text of one server is cut at 4000 runes, and the tool names of one server stop before they pass 2048 bytes.
- Resources. When a connected server serves resources, `list_mcp_resources` and `read_mcp_resource` are added. A listing stops at 500 resources for each server.
- Results. A result is text and blobs. An image, an audio item, or a binary resource is a blob of the result; a payload that is not base64, or has no bytes, becomes one line with its type. An error that is not the server's own RPC error names a reason, never the endpoint URL or a response body. An error has no `engine:` prefix, and a call to a tool that is not there reads `no such tool available`.
- Status. The `mcp` tool has a `status` action, with or without deferral. Its result is `{"servers":[{"name","connected","attempts","parked","reason"}]}`, one entry for each configured server in name order, read from the live state. `reason` is absent for a connected server; for any other it is the classified reason of the last failed attempt. A server that connects later adds its tools to the next model call, in the place of its name in the fixed order.
- Down notice. While a configured server is not connected, the model reads `[mcp: unavailable — <clauses>. Tools from these servers are temporarily absent and may return later in this session.]` as a message of its own, in `<harness-engine-context>` tags, and the system prompt does not change. A clause is `<server> (<reason>; retrying)`, or `<server> (<reason>; use the mcp tool action "connect" to retry)` for a parked server, in name order. Each model call of a harness-loop turn builds the text from the live state and pins it: when the text differs from the newest MCP notice, the call sends it after the history that is newest at that call, and each later call sends it at the same place, so each request is a prefix of the next. When the last server connects after a notice, the next model call pins `[mcp: every configured server is connected again.]` once. A compaction moves every pin, the banner and each notice, to the end of the history that it leaves, and a pin moves only then, so the cache breakpoints of an Anthropic request stay on the prefix. No log holds a notice and no reader of the history shows it; a new process pins the live state of the notices again at its first model call, after the banner and not after the newest history, so a server that is down before and after a restart gives the same prefix; a change of a notice later in the process follows the history of its call. The banner is no live state: it reads the log alone (see prompt), so a new process sends it with the same text at the same place. The actor owns the pins; a model call pins through `Turn.Pin` on the actor goroutine and the request holds an immutable copy. A backend that owns the loop gets no notice.
- Session servers. `WithSessionMCP` gives one session MCP servers beside `Config.MCPServers`. The keys are server names under the rules of `Config.MCPServers`; a spec that breaks a rule, or a name that a runtime server has, fails `Create` and `Open` with `invalid_request`. So does an embedder tool or a started plugin tool whose name is `mcp`, `list_mcp_resources`, `read_mcp_resource`, or starts with `mcp__`, when the session has session servers. The runtime builds one registry for the session at `Create` or `Open`. It connects lazily at the first model call, with the connection, retry, parking, status, and down-notice rules above, and it holds the runtime servers and the session servers in one `mcp` tool. `Release`, `End`, and `Runtime.Close` stop every stdio server of the session, and each waits for them. Nothing persists: the specs, and the secrets in their `env` and `headers`, are in no event and in no log line, and a log line names a server and nothing else. An `Open` that passes no option has no session server, and a call to one of its tools reads `no such tool available`. An `Open` of a session that the runtime already runs keeps its servers. A backend with `OwnsMCP` gets the session servers with the runtime servers.
- The segment follows the process status line.

### plugins

`internal/tool/pluginsrc` connects `Config.Plugins` and `Options.Plugins` to each session.

- Order. Each hook chain runs the plugins in this order: the plugins of `Config.Plugins` (project plugins, then user plugins), then the in-process plugins of `Options.Plugins` in list order. The most trusted plugins run last, so a user guard sees the final arguments after each project rewrite, and an embedder guard sees them after the user plugins.
- In-process mode. `harness.New` takes `*plugin.Plugin` values in `Options.Plugins`. The host serves each value in this process over an in-memory pipe that runs `plugin.ServeConn`, through the dial seam of a process plugin, so the chain, the deadline, the failure rules, and `deny_on_failure` are those of a process plugin. An in-process plugin has no probe and no cache entry: the host reads its manifest from the value. The pipe and its goroutine start on the first hook or tool call. `New` fails on a nil plugin, an empty name, a `ToolExecuteBefore` with no `Run`, an invalid `HookTimeouts` value, a plugin tool with no name or no `Execute`, a name that repeats the name of a config plugin or of another in-process plugin, and a plugin tool that takes the name of a built-in tool, an `Options.Tools` tool, or another in-process plugin tool; a clash with a tool of a config plugin fails the first `Create` or `Open`, which probes the config plugins. `Runtime.Close` closes each pipe and waits for the serve loops and for the hook and tool calls in flight, bounded by its context: a call that ignores its context holds `Close` until the context of `Close` ends. A process plugin does not wait: `plugin.Serve` returns when its input ends, and the process exit ends the calls. `ProbePlugins` probes config plugins only, and the plugin inventory lists both kinds.
- Start. The first `Create` or `Open` of a runtime reads the manifest of each plugin: from the manifest cache when the plugin has not changed since its last probe, else with one probe, bounded at 30 s, which it stores in the cache. The cache is the file `plugin_cache` (see config). An entry is keyed by the plugin name and a digest of its config, environment, and directory, and holds the size, mtime, and content hash of the executable and of each script argument that is a regular file; a matching size and mtime trust the entry with no read, and only a change of content probes again. A cache file that does not decode, or that carries another `version` than the runtime writes, counts as empty, and a cache that cannot be read or written fails the call. With no `plugin_cache`, the runtime probes at every start. A probe that fails, a manifest whose name is not the config name, or a plugin tool with the name of another tool or of a built-in tool of the model backend fails that call, and the next call tries again. The probe is work that `Runtime.Close` waits for, and a call after `Close` returns `ErrDraining`. A plugin process starts on its first hook or tool call and stays warm until `Runtime.Close`.
- Tools. Each manifest tool runs through `tool/execute`. `AllowedTools` restricts them. The text parts of the output, joined by a newline, are the result text. A `blob` part with inline data is a result blob, which the model reads after the text. A `blob` part with only a URL is the text line `blob: <url> (<media_type>)`. Any other part type fails the call. A plugin needs no `WorkDir`.
- Hooks. A web search (`web.search`) runs inside the model call of the provider and in the CLI, so no hook runs on it (see Web search). A `turn.Toolset` carries `turn.Hooks`, and `runTool` runs each tool call between `Before` and `After`. This covers every tool: embedder, process, MCP, plugin, and built-in tools, and each call that a backend that owns the loop makes through `Request.Call`, so the hooks see each file, search, and shell call of a Claude Code session. A call that an `OwnsMCP` backend makes to a server that it connects itself runs no hook, and neither does a client call that a plugin makes (`client/mcp.call`, `client/generate`). `tool.execute.before` rewrites the arguments or denies the call; a denied call does not run, and its deny is the error result. `tool.execute.after` rewrites the result text. The log records the call as the model made it. Each synchronous hook call has a deadline: the `hook_timeouts` value of the plugin manifest for that hook, from 1 to 600000 ms, or 5 s when the manifest has no entry. The manifest must list the hook in `hooks`, and `event` has no entry. A violation in a probed manifest fails the first `Create` or `Open`, and in an in-process plugin it fails `harness.New`. The timer runs while the hook waits on a client call. When the dispatch of that hook starts the plugin, the `initialize` call has the longer of that deadline and 5 s, and the deadline bounds the hook call alone. Only `initialize` has that bound: the spawn or dial does not end with the context. The host compares `hook_timeouts` with the live manifest at spawn, like the other fields.
- Failure. A hook chain fails open: a plugin that does not start, exits, passes its deadline, returns an error, panics, or sends a response that does not decode is skipped, the chain continues, and `Options.OnError` of the host receives the failure. A plugin can list `tool.execute.before` in the manifest field `deny_on_failure`; the manifest must list that hook in `hooks`, and any other value fails the first `Create` or `Open` for a probed manifest and `harness.New` for an in-process plugin. When a flagged hook fails, the chain stops, the tool does not run, and the error result is `denied: plugin <name> failed: <error>`. A hook is flagged when the cached manifest or the live manifest flags it, so a start failure, a manifest mismatch included, denies each call of that hook. A canceled context is an abort: the call ends and records no deny. At spawn the host compares the live manifest with the cached one (name, protocol version, hooks, `deny_on_failure`, `hook_timeouts`, and tools), and a difference is a start failure. A plugin that fails to start stays failed until `Runtime.Close`, unless the caller canceled the call that started it; the next call starts it again.
- Prompt. Each model call runs `system.transform` with the model of that call, and adds its segments after the MCP segment. A backend that owns the loop runs no `system.transform`; its tool calls still run the hooks.
- Events. `session.Config.Appended` gives each appended event, and the state after it, to the runtime, which gives the events to the plugins of the session. `turn.started` and `turn.resumed` send `session.status` busy. `turn.suspended` and `turn.ended` send idle, and a failed turn first sends `session.error`. The tool hooks send `tool.execute.start` and `tool.execute.end` around each call that runs, and `file.edited`, with an absolute path, after a `write_file` or `edit_file` call that succeeds. Delivery is best effort.
- Client API. `client/session.messages` reads the history of the session from its actor when this runtime runs it, and from the store otherwise, with each attachment as a `blob` part with its bytes; a failed read fails the call. `client/mcp.call` calls a tool of any server of `mcp_servers` and returns `{content, is_error}`. It waits for the first connect of the servers and never dials again: a server that is retrying in the background or parked fails the call at once with its reason, without a new attempt, and only the connect action of the `mcp` tool brings a parked server back. An unknown server, a down server, or a failed transport fails the call, and its text hides the server URL and headers. `client/generate` resolves an empty `model` to the default model and an alias to its model; an unserved provider, or a model whose backend runs its own agent loop, fails the call, and a failure text is masked as a session error is. It makes one request with no tools through the provider layer with the retry limits of the runtime, and returns `{message, usage}` with `usage` in the shape of the turn usage. No session records the call; on success the host logs one line with the plugin name, the model, and the usage. Both calls end when the plugin connection ends or the runtime closes.
- `shell.env`. Before a `bash` command runs, the runtime sends the hook the session, the tool, the command, and the work directory, and adds each returned variable to the environment of the command, in plugin order. Before each model call of a backend that does not own the loop, the runtime sends `chat.params` the session and the model and output cap of the call; a returned model replaces the model of that call, and the returned output cap, `temperature`, and `top_p` go to the provider. The runtime does not dispatch `chat.message`.
- `serve_url` and `run_token` are `Options.ServeURL` and `Options.RunToken`, empty when the embedder sets none.

### prompt

`internal/prompt.Build(cfg, workDir)` returns the system prompt of a session as segments. The runtime joins them with a blank line. It reads them once, when the session is created or opened, and sends them as `turn.Request.Instructions` on each model call. The log never holds them, so the next `Open` reads the files again.

With `Options.WorkDir` empty, the prompt is `append_system_prompt` alone, and no file is read. With a `WorkDir`, the segments are, in order:

1. The base prompt of a coding agent. It ends with the working directory.
2. The `append_system_prompt` entries.
3. The tool-batching segment, which says that harness runs the tool calls of one message concurrently, up to 8 at a time.
4. The `AGENTS.md` chain. Each directory from the git root down to `WorkDir` gives its `AGENTS.md`, or else its `AGENT.md`. Outside a repository, only `WorkDir` counts. `instructions: false` turns the chain off, and `instructions_path` replaces it with one file. Each file over `instructions_max_bytes` (default 64 KiB) keeps the sections that fit whole and gets an outline of the rest, one line for each section with the `read_file` range that reads it. A first section over the cap, and a file with fewer than two sections, is cut and gets a marker that names the file and the byte counts. `instructions_mode: "full"` (or `HARNESS_INSTRUCTIONS_MODE=full`) keeps the head and the marker for every file. A chain over four times `instructions_max_bytes` loses its middle files, nearest the root first. A negative value keeps the whole chain.
5. The skill list: each valid `SKILL.md` below `skills_dirs` (default `.agents/skills`), sorted by name, with its path.

A file that cannot be read, is empty, or is not UTF-8 is skipped, and so is a skill that is not valid or repeats a name. The session starts without it. Each skip and each cut writes a WARN log line.

A backend that owns the loop ignores `Instructions` and builds its own prompt. The runtime sends it the skill segment (segment 5) of the session as `turn.Request.Skills`, read when the session is created or opened, so the skills are those of the session and follow `skills_dirs`. Claude Code gets one `--append-system-prompt-file` value, and the CLI runs in `WorkDir`. The backend writes the value to a private temporary file for each run and removes the file when the run ends, because Linux caps one argument at 128 KiB and a long skill list would fail every turn. `Request.Sent` gets the full text. `extra_args` of a `claude-code-cli` entry may not hold `--append-system-prompt` or `--append-system-prompt-file`, in any form, and `config` load fails when it does. In order, the value holds: a segment that says that the tools are the harness tools, that names `read_file`, `write_file`, `edit_file`, `bash`, `glob`, `grep`, `ls`, and `task` for `Read`, `Write`, `Edit`, `MultiEdit`, `Bash`, `Glob`, `Grep`, `LS`, and `Task`, that names `process` for `BashOutput`, `KillShell`, and `Bash` with `run_in_background`, and that says that there is no `TodoWrite`, `WebFetch`, `WebSearch`, or `Skill` tool, and that a skill is read with `read_file`; the `append_system_prompt` entries; the skill segment; and the history directive when it applies. The tool-name segment comes from the tools that the turn offers (`Request.Tools`), not from config: when the turn offers `web_fetch`, it names `web_fetch` for `WebFetch` and says that there is no `TodoWrite`, `WebSearch`, or `Skill` tool. The CLI has its own base prompt and tool-batching text, and it reads `AGENTS.md` and `CLAUDE.md` itself, so the value holds neither the base prompt, nor the tool-batching segment, nor the `AGENTS.md` chain. The CLI `Skill` tool is off, so a skill loads on Claude Code as on every other backend: the model reads its `SKILL.md` with `read_file`. `session_info` reports the skills of the session on every backend.

Each turn sends the prompt with one process status line after it, built when the turn starts (see processes). Each model call then adds the MCP segment, then the plugin segments. The only ambient messages are the banner and the MCP notice of a server that is down; the system prompt carries neither. With `Options.Version`, each model call of a harness-loop turn sends `[engine: harness <version> · session started <time>]` in `<harness-engine-context>` tags, where `<time>` is the RFC 3339 UTC time of the `session.created` record. The banner follows the first message of the history, and it is the first pin: a notice at the same place follows it. After a compaction it follows the whole history that the compaction left, the place that the live pin moves to (see mcp). The text and the place come from the log alone, so a new process (a restart, a reload, a wake from hibernation, or a handoff) sends the banner unchanged and each request of the session is a prefix of the next, also across processes and after a compaction in the middle of a turn. The banner holds no process start time, and no notice reports a restart. A change of the harness version changes the banner text, so the first request after it rewrites the cache after message 1, by design. A backend that owns its loop gets no banner.

### processes

With a `WorkDir`, the runtime builds one `process.Manager` from `Config.Processes` and adds the `process` tool over that manager. Without a `WorkDir`, no process runs and the model sees no `process` tool. With a `WorkDir`, an embedder tool named `process` fails `New`.

`declare` runs any argv that the model names. A `WorkDir` therefore grants command execution equal to `bash`, even when `Options.Tools` has no shell tool. An embedder that wants only AGENTS.md discovery must not set `WorkDir`.

- The tool description names the configured processes only, so it never changes while the runtime runs. `declare` adds a process in memory until the runtime closes.
- A tool error is the error of the manager, with one `process:` prefix.
- A result reports no elapsed time. The status line names instants instead.
- When a turn starts, the session appends one status line to its system prompt: `[processes: dev ready :3000 since <RFC 3339> log=.harness/proc/dev.log]`, one entry for each process that has started. The line is inside `<harness-engine-context>` tags, so the base prompt marks it as trusted. An instant changes only when a process changes state, so the line is stable for the turn and for each later turn with no process change. The log never holds it.
- Each process change (a start, a restart, ready, a stop, or an exit) changes the system prompt of the next turn. That turn misses the prompt cache for the whole history, and on the OpenAI WebSocket path it sends the full input instead of a suffix. A process that exits during a turn shows in the next turn. The cost is one cache miss for each process change, which keeps one prompt for each turn.
- `Runtime.Close` stops every process after the sessions end. When its ctx ends first, it cancels the turns and kills the processes, and returns without waiting for a turn to end.
- The process tool and the `/processes` routes share the one manager. A `start`, `stop`, or `restart` route is work that `Runtime.Close` waits for, and fails with `draining` (503) after `Close` starts, so no route starts a process that `Close` does not stop. `Runtime.Processes`, `ProcessLogs`, `StartProcess`, `StopProcess`, and `RestartProcess` expose the manager to `httpapi.Handler`. `GET /processes` lists every process with its definition and status, and is `[]` without a `WorkDir`. `start`, `stop`, and `restart` reply with the status. `GET /processes/{name}/logs` replies with `content`, the last `tail` lines (default 50), and `status`. An unknown name, or any name without a `WorkDir`, is `process_not_found`. Any other error of the manager, such as a failed start, is `internal` with the fixed message.

### workspace

`GET /workspace/changes?scope=&dir=` diffs the work tree of a directory. The route exists only with a `WorkDir`.

`internal/workspace` defines the interfaces that it consumes:

```go
type Engine interface {
	Name() string // the value of the engine field
	Open(ctx context.Context, dir, ceiling string) (Repo, bool, error)
}
type Repo interface {
	Root() string
	Head(ctx context.Context) (Head, error)
	Base(ctx context.Context, head Head) (Base, error)
	Diff(ctx context.Context, head Head, base Base, patchCap int) (files []protocol.ChangedFile, patch string, truncated bool, err error)
}
type Head struct {
	Rev   protocol.Rev
	Label string
}
type Base struct {
	Rev protocol.Rev
	Ref string
}
```

- `Open` looks for a work tree that holds `dir`, and stops at `ceiling` (the parent of the `WorkDir` for an explicit `dir`, else none). It returns `ok` false and a nil error when `dir` is not in a work tree of its kind, and `Changes` then asks the next engine. A non-nil error is a failure of the engine, and it ends the request. `Root` is the root directory of the work tree.
- `Head` returns a `Head`: a `Rev` and an optional `Label`. An engine that finds no commit gives the zero `Rev`. `Base` returns the base of the `branch` scope, with a display name in `Ref`. It wraps `ErrNoBase` for a work tree that has no base, and any other error is a failure of the engine. `Diff` lists the changed files and the patch against `base`; a zero `base` means `head`. The patch ends at the last whole file within `patchCap`, and `truncated` reports a cut. `Repo` carries no storage path, so an engine may keep its data in any form.
- `workspace.Changes` owns the scope check, the containment of the paths, and the 28 s budget. It walks one ordered list of engines and asks each to `Open` the directory. The first engine that reports a work tree answers. Each engine decides alone what marks a work tree, and the order of the list does not rely on two engines finding the same marker. A directory that no engine opens is `not_a_repo`. Only `Changes` names the list. It passes the list to `ChangesWith`, which walks it, and a contract test passes the one engine under test. No code branches on the name of an engine.
- `scope` is `branch` (default), against the base of the `Repo`, or `uncommitted`, against its head. A `Repo` with no base for the `branch` scope fails with `no_base`.
- `dir` defaults to the `WorkDir`. Any other `dir`, relative to the `WorkDir`, and its repository must stay under the `WorkDir` after symlinks.
- `files` is complete. An untracked file above 2 MiB is `large` with no hunk. `patch` ends at the last whole file within 1 MiB and sets `truncated`. The patch is not HTML-escaped.
- A read may snapshot the working copy of an engine that keeps one. It adds no operation when nothing changed, and it writes no index, reference, or object that a user could see. Untracked files count as added.
- `engine` is the `Name` of the engine that answered. `branch` is the label of the head, and is empty when the engine has none. `head_rev` is the revision of the head, and is absent for a head with no commit. When `scope` is `branch`, `base.rev` is the revision of the base and `base.ref` is its display name.
- The `git` engine detects a work tree with `git rev-parse --show-toplevel`, below the ceiling that the containment check sets. Its `Head` is the `commit` revision of `HEAD`, and its label is the short name of the checked-out branch. Its `Base` is the merge base of `HEAD` with the first of `origin/HEAD`, `origin/main`, and `origin/master` that resolves. An unborn `HEAD` has no base, and the `uncommitted` scope diffs it against the empty tree. The `git` engine counts an untracked file through a private copy of the index and a private object directory, so the request never writes the real index or object store. It never runs a filter driver, hook, external diff, or textconv, and it ignores the `GIT_*` variables that select another repository.
- A `Rev` is a revision or a change of a work tree. Its text form is `<kind>:<id>`, and its JSON form is that string. `kind` is `branch` (a git branch or a jj bookmark), `tag`, `commit`, `change` (a jj change), or `op` (a jj operation). `id` is not empty and may contain `:`, because the text splits at the first one. Only an engine gives an `id` meaning. `protocol.ParseRev` refuses any other text with `protocol.ErrInvalidRev`. The zero `Rev` has the empty text, and a field tagged `omitzero` leaves it out of the JSON.
- One request has a 28 s budget. A request that passes it, or whose file list passes 32 MiB, fails with `too_many_changes`. Errors: `invalid_request`, `not_a_repo`, and `no_base`.

### config

One `Config` struct. `Defaults` is the one defaults table, and each accessor reads it for an unset key. `Validate` is the one rule set. `LoadProject(userPath, dir)` merges `<dir>/.harness.json` over the file at `userPath` and runs it on the merged config. The `plugins` arrays of the two files join: project plugins first, then user plugins. A plugin name in both files fails with an error that names the plugin and both files. A project file cannot set `auth` or `api_key_env` on any provider, and cannot set `base_url`, `responses_path`, `extra_headers`, or `permission_mode` on a provider that the user config defines or that is built in (`anthropic` and `openai`): `LoadProject` fails with an error that names the key, the provider, and both files. A project file can set `models` on any provider, also on one that the user config defines or on the built-in `anthropic` and `openai`, because the key changes no endpoint and no credential (an entry for `codex` or `claude-code` replaces the native entry as a whole, so a project file that names one gives the whole entry, with its `type`): it only changes the list of models that a client shows, and a model that the list omits can still be set by its reference. A non-empty `models` list of the project file replaces the list of the user file; an empty list or an absent key leaves it. `models` (an array of model ids as the API of the provider names them, without the provider key) holds no empty id and no id twice; an id may contain `/`, and `gateway/anthropic/claude-sonnet-5-5` is then the reference of the id `anthropic/claude-sonnet-5-5`. A project file can define a provider of its own with `base_url`, `responses_path`, `extra_headers`, and `permission_mode`. Only a built-in provider reads an API key from a default variable (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`); a provider that only the project file defines sends no key. `MergeProject` marks such a provider, and `Provider.Keyless()` reports the mark (no JSON key and no exported field sets it). The router passes it to the adapter as `NoKey`: the adapter reads no API key and sends no key header of its own, and an empty key is no error. A header that `extra_headers` of the entry names goes out as given. Only the `openai` and `openai-compat` adapters can build such a provider, because an entry with an empty `type` is valid only for the built-in names. A built-in provider or a user-config provider with no key still fails the request with the no-key error that names the reason. `Parse(name, data)` decodes one config document like `Load`, and `MergeProject(user, name, dir, proxyBase)` is `LoadProject` for a user config that the caller holds. A non-empty `proxyBase` fills the `base_url` of the native `anthropic` and `openai` providers and of each `providers` entry that sets none (see `Config.WithProxyBase`), between the merge and `Validate`; `LoadProject` passes an empty one. `LoadProject` reads no environment variable and no home directory. `New` runs `Validate` on `Options.Config`, and `Validate` never changes the config. `ProcessSpec.Validate` is the per-entry rule that `process.Declare` also uses.

A project config is code. Harness trusts the `.harness.json` of a directory as much as a script that you run from that directory. This holds when a user opens the directory with the CLI, with `harness acp` from an editor, or with a server session whose `cwd` is that directory. `LoadProject` does not limit these keys of a project file, and no prompt asks before they take effect:

- `providers.<name>.binary_path` and `providers.<name>.extra_args` choose the program and the arguments of a `claude-code-cli` entry, also when the user config defines the entry. A project file can set `permission_mode`, the `--permission-mode` of the program, only on a provider that it defines itself, and `bypassPermissions` is valid there.
- `mcp_servers.<name>`: `command`, `env`, and `dir` start a stdio server. `url` and `headers` connect to a Streamable HTTP server.
- `plugins[]`: `command`, `env`, `dir`, and `config` start a plugin process, and its hooks and tools then run in every session.
- `web.fetch`: turns on `web_fetch`, so every session of that directory can make requests to public hosts from the machine of the user. The address check still applies.
- `processes.<name>`: `command`, `env`, and `dir` define a process that the `process` tool and the `/processes` routes start. `ready_http` makes a GET request to its URL.
- `providers.<name>.base_url`, `responses_path`, and `extra_headers` of a provider that only the project file defines choose the endpoint that receives its model requests. That provider reads no API key and sends no key header of its own, because a project file cannot set `api_key_env`; a header that its `extra_headers` names goes out as given. Its requests succeed against an endpoint that needs no credential. A project file cannot set these keys on a provider that the user config defines or on the built-in `anthropic` and `openai`.

A user must not run harness in a checkout that the user does not trust. An embedder that opens untrusted directories passes its own config and isolates the process, for example with a container or a separate user account.

`cmd/harness` resolves `userPath` as `$HARNESS_CONFIG_FILE`, else `$HARNESS_CONFIG`, else `~/.harness/config.json`. `HARNESS_CONFIG_FILE` changes the file that is read and nothing else: the Codex login and the plugin cache default to the directory of `$HARNESS_CONFIG`, else `~/.harness`. `cmd/harness` logs one line at boot that names the file it loaded and counts its processes, MCP servers, and plugins.

`model` names the default model. When the merged config sets no `model`, and no `HARNESS_MODEL` and no `-model` flag does, `ResolveModel` takes the default from one function of `config`. The function reads the field `Config.SubscriptionDefault`, which no file, variable, or flag sets: only the composition root of a command that a person runs sets it. `cmd/harness` sets it for `run` and `acp`. `serve` and the Go API (`harness.New`) leave it false, because a server must not change its default with what is on its `PATH` (a box image has `claude` and sets no model), so they take `anthropic/claude-fable-5`; an embedder that wants the subscription default sets the field itself. With the field set, the order is: the built-in default `anthropic/claude-fable-5` when its credential is set (the variable that the `anthropic` entry names in `api_key_env`, else `ANTHROPIC_API_KEY`), so a user who works with an API key is never moved; else `codex/gpt-6.1-sol` when the provider `codex` is the native entry (or an entry with `auth` `chatgpt`) and the file `codex_auth_file` exists and is not empty; else `claude-code/sonnet` when the provider `claude-code` is an entry of type `claude-code-cli` whose program resolves on `PATH`; else the built-in default. The credential check reads one environment variable. The Codex check is one `stat` of the user's own login file and reads no content. The Claude Code check is `exec.LookPath` and starts no process. Neither makes a network call. The WebAssembly build never selects `claude-code`. A `providers` entry named `codex` or `claude-code` replaces the native entry as a whole, so an entry without the ChatGPT login or the CLI type never gets its subscription default. Every reader of the default takes it from `ResolveModel`, so the ACP model option, `Runtime.Create` with an empty model, and `run` agree. A Claude Code turn that fails while the CLI is not logged in has the error class `login_required` and the message that names `harness login claude-code`; ACP `session/new` and `session/prompt` raise the same error first (see Authentication).

`max_tokens` caps the output tokens of each model call of a turn. 0 is the backend default, and a negative value fails `Validate`. `HARNESS_MAX_TOKENS` sets it.

`request_max_retries` (default 4) and `stream_max_retries` (default 5) bound the retries of a model request and of a model call (see Backend). A nil value is the default, 0 turns the retry off, a value over 100 is 100, and a negative value fails `Validate`.

`Load` rejects a key that `Config` has no field for, with an error that names the key. `session_dir` is for `cmd/harness`; `model_tool` turns the `model` tool off, and `New` reads it.

`web` is an object `{"fetch": bool, "search": bool}`; each field is off when unset, and an unknown field fails the load. `fetch` offers `web_fetch` to the sessions of a runtime with a `WorkDir` (see Built-in tools). `search` offers the native web search of the provider of the model, with or without a `WorkDir` (see Web search). The user file and the project file may both set it, and a project field overrides the user field of the same name, so a field that the project leaves unset keeps the user value. `ApplyEnv` has no variable for it, because it is an object.

`plugin_cache` (a path; `HARNESS_PLUGIN_CACHE`) names the plugin manifest cache. Only the user file sets it: a project file cannot. `cmd/harness` sets it to `plugin_cache.json` beside the user config when no variable names it, and the Go API has no default.

`codex_auth_file` (a path) and `codex_issuer` (a URL) name the Codex login file and its OAuth issuer (see ChatGPT subscription login). Only the user file sets them: a project file cannot. `cmd/harness` sets `codex_auth_file` to `auth/codex.json` beside the user config when no variable names it, and the Go API has no default. `Validate` requires `https`, or `http` on a loopback host, for `codex_issuer`. The provider keys `auth`, `login_url`, and `models_from` are also user-only: a project file that sets one of them fails to load (see above). A project file then cannot send the login to another URL.

`owner_epoch` (a number) and `sync {url, token_file}` are the keys of a box harness, which `cmd/harness` reads and `harness.New` does not; `Validate` requires an `http` or `https` URL with no userinfo, and a token file. Only the user file sets them: a project file cannot. A project file is still read as a file, so an invalid `sync` there fails the load, and a valid one is dropped.

`ApplyEnv` sets each top-level string, number, or bool key from `HARNESS_<KEY>`. An empty variable keeps the key. A map, slice, or struct key has no variable, and neither has `codex_issuer`. A parse error names the variable and never the value. There are no env-only knobs. `cmd/harness` applies it in `serve` and `run`, and a flag wins over a variable.

The package imports only the standard library. The `config-leaf` depguard rule enforces it.

## WebAssembly

The root package and the packages it imports build for `GOOS=js GOARCH=wasm`. `cmd/harness-wasm` is the entry of the module: it exports `harnessConnect` and wires `internal/jshost`, which holds the adapters of the host. Both have the build tag `js`, so a build for another target skips them. A JavaScript host runs the module: a browser page, a Worker, or Node with the `wasm_exec.js` of the same Go version. Harness embeds no JavaScript engine. `GOOS=js` is the only wasm target. The Go scheduler runs on the event loop of the host, so `time` and timers work with no extra interface.

Every capability that the host supplies is a field of `harness.Options`. The module adds no seam.

| Seam | Declared in | On `GOOS=js` |
| --- | --- | --- |
| Model transport | `Options.ModelTransport` | The host sets it. The default `net/http` transport on js cannot cancel a response body, and under Node it is off. |
| Store | `harness.Store` | `store.DiskStore` on the `fs` object of the host (OPFS in a browser, the real `fs` under Node), or `store.MemStore`. |
| Owner | `Options.Owner` | `NewLocalOwner`. `flock` does not exist on js, so the store does not fence two instances (see Store). The host gives one writer to each store: a Web Lock in a browser, `.embed.lock` under Node. |
| Tools | `Options.Tools`, `CreateSession.AllowedTools` | The built-in file and search tools run on the `fs` object. `bash` and `process` fail with `exec: executable file not found`. A host hides them with `AllowedTools` and adds its own tools in `Options.Tools`, also a tool named `bash`; `sdk/embed` hides them by default (`AllowedTools`). The wasm host does not call `Runtime.WorkspaceChanges`, which runs `git`. It sets `NoNetwork`, so it offers no `web_fetch` (see Host limits). |
| Host limits | `Options.Host` | `HostLimits{NoProcess, NoLogin, NoNetwork}`, all true. With `NoProcess` a provider that runs a child process, such as the native `claude-code`, has no backend, so it is not listed and a model of it is unavailable. With `NoLogin` the host has no file login: a provider with `auth` `chatgpt`, such as the native `codex`, is not listed, reads no login file, and fails a request before it is sent with the error class `login_required`; the ACP adapter offers no `chatgpt` method. With `NoNetwork` the host has no direct network, so the runtime offers no `web_fetch`, also when `web.fetch` is true: the config is valid and the tool is absent, and `known` is false for the name. The wasm build links neither the tool nor its HTML parser. A native host sets none of them. |
| Logger | `Options.Logger` | Any `slog.Logger`. |
| Getenv and config | `Options.Getenv`, `Options.Config` | Host closures and a `config.Config` value. |

### harnessConnect

The module exports `globalThis.harnessConnect(options)`, which opens one ACP connection and returns `{send, close}`. One connection speaks one ACP version. The options are:

| Option | Meaning |
| --- | --- |
| `config` | A `config.Config` object, the user config. `session/new` merges the `.harness.json` of its `cwd` over it. Keys do not go in it. |
| `storeDir` | The directory of a `store.DiskStore` on the `fs` object of the host. Absent: a `store.MemStore`. |
| `defaultDir` | An existing directory on the `fs` of the host: the work directory for a `session/list` with no `cwd`. Absent: the module advertises no `list`. |
| `env` | The only variables that `Options.Getenv` returns. |
| `tools` | The JavaScript tools: `{name, description, schema, run(input, {signal})}`. `run` returns a string, `{text, isError}`, or a promise of either. The signal aborts when the call context ends. A rejection is an error result. |
| `builtinTools` | The `allowed_tools` of every session that `session/new` creates. Absent: every tool. |
| `proxy` | `{headers, baseURL, providers}`. `headers` are added to a request that does not set them and that goes to the scheme and host of `baseURL`; without `baseURL` no request gets them, so a credential never reaches another origin. `baseURL` sends all model traffic to the proxy: it is the `base_url` of the native `anthropic` and `openai` providers when the config has no entry for them, and of each `providers` entry that sets none, except a Claude Code CLI entry. The module fills `base_url` before it validates the merged config, so an entry of type `openai` with no `base_url` is valid when `baseURL` is set. `providers` maps a provider name to `{baseURL, loginURL}`, each optional, which set `base_url` and `login_url` of the entry. For a native provider such as `codex` that the config does not define, `baseURL` is required (the native `base_url` is the public endpoint, which would get the request with no credential), and the module copies the native entry and sets `auth` `injected` too; for a `providers` entry of the config it sets the two fields only, and the config sets `auth`. The `base_url` of such an entry wins over `baseURL`. A name that is no provider fails the first `session/new`. The merged config is validated as usual, so a `loginURL` that is not absolute and `https` (or `http` on a loopback host) fails it. |
| `modelFetch` | A function `(url, init)` that sends the model requests. `sdk/embed` sets it from its `fetch` option, and the module replaces no global. Every model request, and the status probe, follows no redirect (`redirect: "manual"`), and a redirect that the host hides is an error: a server answers `401`, never a redirect to a sign-in page. |
| `onFrame`, `onLog` | `onFrame(line)` receives each ACP message of the agent. `onLog(line)` receives each JSON log line. |

Every option that is present must have its type. A bad option does not throw: the first error fails the first `session/new` as an invalid request. The module advertises no MCP transport and refuses `mcpServers`, because it starts no MCP server.

The frame channel is two in-memory pipes between the host and `acp.Serve`. `send(frame)` writes `frame` and a newline to an unbounded queue that a goroutine drains into the input pipe, so it never blocks the event loop. A goroutine reads the output pipe by line and calls `onFrame`. `close()` ends the input, and its promise settles when `acp.Serve` has released the sessions and `onFrame` has received the last frame. `send` after the end is ignored.

The model transport is `FetchRT` of `internal/jshost`, a `RoundTripper` over the global `fetch`. It aborts the fetch when the request context ends, also after the response headers, and each `js.Func` that it creates releases itself after it fires. A fetch that gets no response returns a `*FetchError` with the method, the origin, the name and message of the JavaScript error, and `navigator.onLine` at the failure. The error text is `fetch <method> <origin>: <name>: <message>` and a statement that the browser blocked the request or the host is unreachable. It holds no query, header, or body, and never an object marker: a value that is not an Error gives its text for a string, a number, or a boolean, and a fixed text that names its type for any other value. `FetchError.Permanent()` is true while `navigator.onLine` is true, so `provider.Do` does not retry and the turn ends `permanent` (see Turn). A host that reports offline, or reports nothing, keeps both retry layers. A failure after the response headers is an ordinary read error.

The Codex websocket transport cannot dial on js, because a browser `WebSocket` cannot set request headers. The build tag of `ws_dial.go` and `ws_dial_js.go` in `internal/provider/openai` separates the dial. On js the dial fails, and the request goes over HTTP.

### Model access from a page

A page holds no model credential. A server holds it and adds it to the request. The page reaches a model in one of three ways.

An entry with `auth` `injected` (see Injected auth). The page sets `proxy.providers`, for example `{codex: {baseURL: "/codex", loginURL: "/settings/codex"}}`, or the same two fields in a `providers` entry of the config with `auth: "injected"`. The module sends the request to the `base_url` with no credential header and no user identifier, and the server adds the credential. The server answers `401` for a caller that has no credential, which the module maps to `login_required`; the client of the page offers the method `injected-<provider>` to the user (see Authentication), and the page behind the `login_url` runs the sign-in on the server. The module reads no token, stores no login file, and sends no frame that holds a secret.

The server knows the caller from the session cookie of the page origin. `fetch` sends the cookies of the origin, and the module never sets `credentials: "omit"`. The server also defends against cross-site requests: `SameSite` cookies, a check of `Sec-Fetch-Site`, or a custom header that the page sets with `proxy.headers`. It authorizes the status probe exactly like a model call, with the same session and no cache of its answer. The base URL is a path of the page origin, because a cross-origin server needs a CORS preflight for each request. An XSS on the page can still call the server as the user and spend quota within the limits of the server, and can start the server-side sign-in and finish it with another account. The login page binds the flow to the session of the user and defends against cross-site requests on its start and finish routes.

A same-origin proxy that holds the credential for a provider without `auth` `injected`. The page sets `proxy.baseURL` to a path of its own origin, for example `/model-proxy`, and puts no key in the config or in the page. The proxy is a server that serves the module and `wasm_exec.js` and forwards `/model-proxy/<path>` to the model backend. Same origin and CORS are not authorization: any HTTP client can call the proxy and spend the credential. The proxy authenticates and authorizes each request with the session of the application, before it removes page credentials such as the session cookie. It also applies abuse controls: rate limits and a list of allowed paths. Then it removes the headers that the browser adds (`Origin`, `Referer`, `Cookie`, `Sec-*`) and any credential header from the page, and adds the authentication headers of the backend. The same origin needs no CORS preflight. The credential never reaches the page, so a reader of the page cannot take it.

The direct Anthropic demo is for demonstration only. The `anthropic` provider entry sets `extra_headers` to `{"anthropic-dangerous-direct-browser-access": "true", "x-api-key": <key>}`, so the key is in the page. Anthropic then answers the preflight of the browser, and the request passes. Anyone who loads the page can read the key. The adapter does not send the header by itself. Direct OpenAI access is not supported.

### Size budget

`go run ./internal/wasmsize`, from the repository root, builds `cmd/harness-wasm` with `-trimpath -ldflags="-s -w"` and measures the module raw and with `gzip -9`. It fails when either size is above its number in `testdata/wasm-size-budget.txt`. Each number is the measured size plus 3%, rounded up to 50,000 bytes. A change that grows the module raises the number in the same diff, and its PR body gives the reason.

## Tests and guardrails

### Contract suite

- `harnesstest` is a public package. `NewAnthropic`, `NewChat`, and `NewOpenAI` serve scripted Anthropic Messages, OpenAI chat-completions, and Codex Responses (SSE and websocket) endpoints, with errors, usage, overflow, and blocking steps; `RateLimits.Extra` adds Codex rate-limit windows by limit name. See its godoc.
- A test double that imitates a wire is checked against a real recording in `testdata/wire` (see the Wire doubles gate). `internal/wirerecord` makes the recordings and runs only with `HARNESS_RECORD_WIRE=1`; `internal/wirescrub` removes ids, tokens, emails, and paths first.
- A scenario is a model script and a list of actions. Its golden holds the normalized model requests and the final transcript of each session, not event shapes. An action that returns a result records its status and body under `calls`, keyed `<action>.<alias>`, with `#2`, `#3` on a repeat. Ids in a body become `ses:<alias>`, `msg#N`, and so on, and a scenario binds each child with `bindChild` before it lists sessions, so no id gets a run-dependent number. `scenario.config` adds top-level keys to the served config, and `scenario.chat` serves the model through `NewChat` as provider `bifrost`.
- Scenarios run the real `harness serve` binary over loopback, in its own process group, through a driver interface. Each wait has a bound. Every scenario checks the invariants of the log: unique message ids, one result for each tool call, and contiguous event sequence numbers.
- The ACP table (`e2e/acp_scenarios_test.go`) is separate from the driver rows. A row is a list of steps, run on the real `harness acp` binary over stdio and on `acp.Serve` over pipes in process. Both hosts match one golden in `e2e/testdata/acp`. The golden holds the normalized model requests, the final transcript of each session (read from the store after the connection closes, not through ACP), and the frames in order, because the frames are the ACP contract. Consecutive chunks of one message are joined in the frames. The test client checks each frame in both directions against the pinned ACP schema. Each row cites a sentence of the ACP section, and `TestACPRows` checks that the sentence is there.
- The ACP TCK job (`.github/workflows/acp-tck.yml`) runs the third-party `acp-tck` v1 suite, pinned to one commit, against the built `harness acp`. `go run ./internal/acptck` serves the scripted model of `harnesstest` on loopback, writes a config that points the `anthropic` provider at it, and starts the TCK, which launches one `harness acp` process for each test. A prompt that holds the cancel text keeps its turn open until the client cancels. The command prints the failing requirements of each tier and exits non-zero when a MANDATORY or CAPABILITY requirement fails (or a MANDATORY one is untested) and `testdata/acp-tck-allow.txt` does not list it, when that file lists a requirement that passes, or when the scripted model gets a request that no step matches. Each line of the file is a requirement id and a reason. The job runs on `main`, nightly, and on every pull request. A failure fails the job, and the `TCK` check is required to merge a pull request into `main`.
- The Node wasm host runs the ACP table on the module under Node 24, with `HARNESS_E2E_NODE=1`: `cmd/harness-wasm/node/host.mjs` speaks ACP on stdio, over a `DiskStore` in the temporary session directory, which one process holds through `.embed.lock`. A row that carries `needsExec` or `needsMCP` does not run there. The other rows match the golden of the native hosts, except that the initialize answer advertises no MCP and the js file system writes the ENOENT text with a capital letter, which the host rewrites before the compare. With the variable set, a missing `node` fails the test. The Node job of CI sets it. The Node job also runs `node --test sdk/embed/test/*.test.mjs`: the tests start the module in a Worker and drive a tool turn, the page fetch bridge, `proxy.baseURL`, the store directory, and an `injected` codex entry through `proxy.providers` through `createHarness`, and check that the first model request lists the default tool set and the page tools, and no `bash` or `process`. A row that carries `jsTool` gives the host the page tool `js_echo` and runs on the wasm hosts only; its golden comes from the Node host. Tests of the Node host (`e2e/acp_node_injected_test.go`) drive the sign-in of an `injected` codex entry that `proxy.providers` configures, against a scripted server that answers 401 until the sign-in, and check that the module offers no `chatgpt` method, lists no model of an entry with `auth` `chatgpt`, and fails a request on it before it sends it.
- The browser job runs `TestBrowserRows` (`e2e/browser_test.go`) and `TestACPBrowserScenarios` with `HARNESS_E2E_BROWSER=1`; without the variable the test skips, and with it a missing `node`, Playwright, or Chromium fails the test. The test builds the module, serves the page, the `sdk/embed` sources, the module, and `wasm_exec.js` on one origin, and serves the scripted model of `harnesstest` on a second origin behind a CORS wrapper that records each request and controls the preflight answer. `sdk/embed/test/browser.mjs` (Playwright, headless Chromium, pinned in `sdk/embed/package.json`) opens one context for each row, runs `sdk/embed/test/browser/page.mjs` in it, and prints the results as JSON; the Go test asserts on the results and on the requests that the fake origin saw. The page reaches the module through `createHarness` with relative module and `wasm_exec.js` URLs, so each row also proves that the module Worker starts, that the Worker loads `wasm_exec.js` by `import()`, and that relative URLs resolve against the page. The rows: a tool turn with a page tool, which makes one preflight whose requested headers the request sends; a streamed text turn, after which `session/list` with no `cwd` lists the one session; a cancel mid-stream, after which the model server sees the connection close; a CORS block, which makes one preflight, no request, and a permanent error that names the block; an offline turn, which retries after the first failed request; headers that a browser forbids, which arrive as the browser values while a custom header passes; the direct Anthropic header from `extra_headers`, which reaches the model server; and a proxy header, which goes to the origin of `proxy.baseURL` and to no other origin. A row on a throttled link (50 Mbps, 40 ms) bounds the cold boot at 15 s and logs the module size. The Worker has the file system of `sdk/embed` (memory, and OPFS below `/persist`). The browser host of the ACP table runs the rows that carry `browser`: `sdk/embed/test/browser-host.mjs` speaks ACP on stdio and runs the module through `createHarness` with the default tool set, the scripted model behind the CORS wrapper, and the work directory `/work`. A streamed text turn, a tool turn, and a page tool round trip through the Worker give the frames and the model requests of the golden of the native hosts. The tool list of its requests differs from a native host by design: the row checks that it lists the default set and the page tool, and no `bash` or `process`. The test cannot read the sessions of the Worker, so the compare leaves out the transcript. A row can open more pages in its context, its tabs, and read the persistent store of the origin from a tab that holds the store lock. The store rows: a reload at once, which ends the page without a `close()` while the old Worker can still hold the store lock, gets the store and keeps the records of the session, `session/list` with no `cwd` lists the session, and `session/resume` continues it with its history; a second tab on a held store waits 2 s and rejects with `store_in_use` while the first tab writes on and loses no record; a second tab that boots after the first closes sees every record and leaves the first session unchanged; two tabs that start at once give the store to one and a memory store to the other; a tool result above the inline limit is one blob, with no temporary file, that `read_tool_result` reads back and that survives a reload; a Worker kill after an acknowledged turn loses no record. A probe program in Go runs on the real `wasm_exec.js` and the file system of `sdk/embed`, in memory and in OPFS, and checks each call that Go's `syscall` layer makes: relative paths, error codes, `O_EXCL`, `O_APPEND`, `O_TRUNC`, a directory opened for write, rename over an existing file, rename onto a destination that a reader holds open, concurrent exclusive creates of one path, `readdir`, partial reads, `Seek`, `ReadAt`, `WriteAt`, truncate, and two descriptors of one file. A Worker that ends after an `fsync` and a later unsynced write leaves the synced bytes and a prefix of the rest. The file tool rows write, read, edit, list, glob, and grep in `/work`, and a second session there gets the `AGENTS.md` that the first one wrote. The shell row seeds a file through `h.fs`, then one turn runs `write_file` and `cat` and `grep -c … | sort` over it, a redirect of the shell that `read_file` reads, `jq` over the seeded JSON file, a `curl` that does not exist, and an endless loop that stops at the command limit; the page then reads the redirect target through `h.fs` and runs each `h.fs` call. The model sees one `bash` tool: the shell replaces the built-in.
- The browser soak job (`.github/workflows/browser-soak.yml`) runs on a schedule and on `workflow_dispatch`, never on a pull request. It runs `TestBrowserSoak` (`e2e/browser_soak_test.go`) with `HARNESS_E2E_BROWSER=1` and `HARNESS_E2E_SOAK=1`; without `HARNESS_E2E_SOAK` the test skips. One tab of the browser fixture runs 300 scripted turns on the persistent store, each with a tool call, so that two automatic compactions apply. The tab then reloads, loads the session with `session/load`, seeds 3000 files in `/work`, and runs a `grep` over them. After each phase the row reads the JS heap of the page, the linear memory of the module, the longest main-thread task, and the longest event-loop lag; it also times each turn, each compaction, the replay, and the `grep`. The test writes the table to the job summary and asserts only that no turn fails, that a compaction applies, that the replay holds every turn and takes under 10 s, that the `grep` finds every match, and that each memory number stays under 512 MB.
- The runtime host runs the rows on `harness.Runtime` in process. With `HARNESS_E2E_RUNTIME=1`, each row also runs through `httpapi.Handler`, on a `DiskStore`, with the config that serve reads. A restart closes the runtime and opens a new one on the same store. A kill copies the store, closes the old runtime on its own directory under an ended context, and opens the new runtime on the copy, so nothing that the old runtime does after the kill reaches the next owner. The driver reads the transcript from the session events, as a reader sees it. A wait for an idle child also waits until its parent records `child.settled` and is idle, because the report of the child can start a turn of the parent.
- Each row has one disposition in `e2e/runtime_rows_test.go`: a contract golden under `e2e/testdata/contract` that both hosts match; a runtime golden under `e2e/testdata/runtime` and no contract golden; runtime only, for a row whose determinism needs an in-process seam, with a contract golden that only the runtime host matches and one reason id of `e2e/testdata/runtime-reasons.txt` (the serve host skips it, and it is never a re-golden row); deleted, for a behavior that the contract does not have, with no golden; or pending on a fix, with a contract golden. Each row cites a sentence of this document, or an id of the closed list `e2e/testdata/runtime-reasons.txt` for a reason that this document cannot hold; an unknown cite fails, and so does a listed id that no row cites. A runtime-only row cites exactly one reason id. A re-golden row cites each sentence or reason that accounts for a difference. A pending row runs in a child test process as an expected failure: the run fails when a pending row matches its serve golden, so it must become a same row. `TestRuntimeRows` checks that each row has the goldens that its disposition names and no other, and that each cite list fits its kind of row. CI runs the runtime host in a step that gates.
- The contract client checks every HTTP response of a host except a CORS preflight, on both drivers. A body must match the schema that `protocol/openapi.json` gives its route and status, and a field that the schema does not list fails. A schema keyword that the check does not implement fails. An SSE frame must match `Event`, and an `error` frame must match `ErrorBody`. A status of 400 or more must have the `ErrorBody` envelope and the status that the Errors table gives its code. A route, a status, or a content type that the document lacks fails the row.
- `HARNESS_E2E_COVER=1 go test -race -count=1 ./e2e/` builds an instrumented binary, runs the e2e package, and prints the statement coverage by package from `TestMain`, and to `$GITHUB_STEP_SUMMARY` when that variable is set. CI sets it in the Contract job, which runs the e2e package on the serve driver. A failure of the report exits the e2e package non-zero and fails CI.

### Unit tests

Unit tests cover pure code only: `Apply`, `internal/acp`, wire transcoders (`internal/provider/*/` and the `internal/backend` wire mapping files), `config`, `internal/message`. TDD means writing the failing contract row in `e2e/` first, seeing it fail for the named reason, then implementing. A file outside the contract suite and the pure-code packages may add test lines only when `testdata/test-exceptions.txt` lists it with a reason. A bug fix adds a table row, not a file. No test reads unexported state across packages.

### CI gates

| Gate | Threshold |
| --- | --- |
| Whole-line comments per file | Warn above 15%; fail above 25%. The `doc.go` package comment does not count |
| History markers in comments | Fail on issue numbers, dates, "previously", "no longer", "red-verified", "confirmed live", "an earlier version", "before this change", "round N", review or fix rounds, and "copilot" |
| File size | Fail above 800 lines |
| Function size | Fail when the closing brace is more than 80 lines below the opening brace |
| Test lines vs code lines per package | Fail above 1.5 test lines per code line, unless the ratio does not rise over the merge base. A new package has no base, but a moved package compares with the package it came from. A change that removes code and adds no test lines always passes |
| Test lines outside the contract suite | Fail when a changed `_test.go` file adds test lines (code lines inside `Test`, `Benchmark`, `Fuzz`, and `Example` functions and package-level `var` declarations; top-level helpers, fakes, and types do not count) outside `e2e/`, `internal/eventlog`, `internal/acp`, `internal/provider/*/`, `config`, `internal/message`, `internal/gates`, and the wire mapping test files `internal/backend/modelapi/convert_test.go` and `internal/backend/claudecode/frames_test.go`, unless `testdata/test-exceptions.txt` lists the file with a reason. The count is net per file, so a change that deletes and adds the same number of test lines passes. A change that deletes test lines always passes. A listed file still meets the test:code ratio row |
| `time.Sleep`, `time.After` in tests | Fail in a test file. `internal/testpoll` is not a test file |
| `AGENTS.md` length | Fail above 80 lines at the root and 25 lines in a scoped file |
| Merge-base diff | `TestRepository` checks only files and packages that differ from `git merge-base HEAD origin/main`, or from `$GATES_BASE_REF`. A new file meets each limit above. A changed file may not cross a limit that it met, and may not get worse on a limit that it already broke. A renamed file compares with its old path. A change that only deletes code always passes. An unchanged file is not checked. No baseline file exists |
| Imports | `depguard`: the import graph of "Internal packages" |
| Structure | `internal/gates` parses the Go files and fails on: a public package that the Public packages table does not list (a dated allow list holds the exceptions, and an entry that stops being a violation fails); and each concept that the spec gives one resolver or one writer: the context window, `Store.Append`, `Store.PutBlob`, `State.Apply`, the error body, the route mounts, `message.EngineContext`, the `git` process (started only by the git helpers of `internal/workspace` and `internal/gates`), and the engine list of `internal/workspace` (named only by `Changes`); and a comparison with a provider or backend name (rule 3) outside `config`, the router, the provider wires, and the `modelmeta` tables, where a dated allow list holds the exceptions and an entry that stops being a violation fails; and a reference to a removed package path (`engine/`, `server/`, `mcpserver`, `provider/claudecode`, `harness-migrate`, an `engine.Name` symbol, or the module path of a package that lives under `internal/`) in any Go file, code or comment. `internal/gates/structure.go` is the one list of owners, by function |
| Routes and errors | `internal/gates` reads the route list, the read routes, the Errors table, and the Public packages table of this document, and compares them with the code (see Contract source and Errors) |
| Wire doubles | `internal/gates` fails when a test double emits a kind, a field, or a JSON type that `testdata/wire` lacks, or a parser reads a field that no recording holds, unless `testdata/wire/allowed.txt` lists it with a reason; it fails on a listed exception that no check needs, and on a recording that holds text that `internal/wirescrub` would replace |
| Wire clients | `internal/gates` fails when a file of `e2e/` makes an HTTP client that does not come from `wireClient`, `wireClientFor`, or `wireClientReadOnly`; the last also requires the refusals of the read-only host (405 for a route that changes something, 404 for a GET it does not serve) |
| WebAssembly size | The WebAssembly job runs `go run ./internal/wasmsize`. The build of `cmd/harness-wasm` for `GOOS=js GOARCH=wasm` must pass, and the module must stay at or below the raw and `gzip -9` sizes of `testdata/wasm-size-budget.txt` (see WebAssembly) |
| Test time | The Unit, Contract, Runtime contract, and Gates jobs pipe `go test -json` into `go run ./internal/testtime`. It fails when a test with no subtests, or a package, takes longer than its number in `testdata/test-time-budget.txt`, and it lists the slowest tests in the job summary. A package number is 1.5 times the slowest CI run, with a floor of 10 s; a package that runs in two jobs can have one number for each (`PATH@JOB`). The default for a test with no subtests is 10 s, and a slower test has its own line with a reason. A change that makes a test slower raises its number in the same diff, and its PR body gives the reason |
| Lint | `govet`, `staticcheck`, `errcheck`, `unused`, `revive` |

The size and test gates compare a branch with its merge base. The route, structure, wire-client, and wire-double gates check the whole tree, with a dated allow list. The protocol drift gate regenerates `protocol/` and fails on a diff.

### Machine-read parts of this document

`internal/gates` and `e2e` parse four parts of this document. Keep their format when you edit them.

- The fenced block under `### Routes`: one route per line or several joined by `·`, each `METHOD /path`. `POST /processes/{name}/{action}` stands for the three actions that the processes section names in its sentence on the replies with the status.
- The paragraph on `httpapi.ReadHandler(store, opts)`: the backticked `METHOD /path` tokens before "through the code of" are the read routes.
- The rows of the Public packages table whose first cell is a backticked package that starts with `harness`.
- The rows of the Errors table, each a backticked code and a three-digit status between pipes.

## Boxes integration

Boxes is a control plane that embeds and hosts harness. The two share one pattern: a resource is one ordered log with one owner, every change is a command, every seam belongs to its consumer, and the contract is generated from Go types.

- Two session hosts. A box session runs in the harness of its box. A home session runs in an embedded `harness.Runtime` inside the control plane, under a lease. Boxes forwards `/v1/boxes/{id}/sessions/…` to one `SessionHost` seam: a reverse proxy to the box, or the local `httpapi.Handler(rt)`, or a proxy to the replica that holds the lease. The console cannot tell them apart.
- One store for every session. `pgstore` is one Postgres `harness.Store`, proven by `storetest`. Home sessions write it directly. Box sessions replicate into it through `Sync`. `OpenView` over it serves reads of a box that is not running, and `httpapi.ReadHandler` mounted over it serves them as HTTP, with the undelivered commands of the box as its `Queue`.
- One pinned commit. Boxes builds each box image from an exact harness commit that it pins (`ARG HARNESS_REF`), never from harness `main` at its head. Harness reaches boxes only through an explicit bump PR on boxes that moves that pin. A harness change that merges to `main` ships to no box until a bump.
- One epoch. The box `claim_epoch` is the `Ownership.Epoch`. It is a plain counter that boxes increments when it admits a Spawn. It is recorded in `owner.acquired`, carried by every `SyncBatch`, and checked by boxes. A stale box learns it is fenced from the rejection. Boxes writes one `BootConfig` file that embeds `harness/config`, validates it once, and sends post-boot values as an update to that same file.
- The seams that boxes uses: `OpenView` (read-only open), `harness/harnesstest` (public scripted model), `Sync` and `protocol.SyncBatch` (the pump contract), `harness/config` (a config without server dependencies), `protocol.Session.HeadSeq` (durable head per session), the `crashed` cause (open after a forced stop), `Models(ctx)` (no sessions needed; `New` does no I/O), `Ownership.Epoch` with `ErrStaleEpoch` (an external lease that fences a stale epoch), `ReadEvents` (events after a cursor, with no replay), and `ReadHandler(store, opts)` (the read routes of a box that is not running, with queued commands).
- `Runtime.Close` and `Session.Release` return only after `Sync` acknowledges every record through the handoff, and `View` reports `SyncedSeq`. `Sync.Deliver` returns `SyncAck{head}` on every reply. The home chat is one per person.
