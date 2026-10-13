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
- Every error code is owned in four places: a `protocol` constant, a `protocol.HTTPStatus` entry, a row of the Errors table with the same status, and a value of the `Error.code` enum of `protocol/openapi.json`. A sentinel error backs every code except `internal`, `payload_too_large`, `unauthorized`, and `not_a_git_repo`.
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
| `sdk/embed` | `createHarness`, `wasm`: the JavaScript client of the WebAssembly module. It starts the module in a Worker, bridges the tools, the fetch, and the proxy of the page to it, and gives the page an ACP stream | web pages, Node programs |

`cmd/harness` is the binary. `sdk/typescript` is the TypeScript SDK for plugins: it speaks the plugin protocol and has no dependency.

`sdk/embed` is plain JavaScript with no dependency. `createHarness({transport: wasm({module, wasmExec, worker: true}), config, proxy, fetch, tools, builtinTools, store, log})` starts the module in a dedicated Worker, a browser Worker or a Node `worker_threads` Worker, and calls `harnessConnect` there. It returns `{stream, close}`. `stream` is a `{readable, writable}` pair of ACP messages as parsed JSON, which an ACP client reads and writes. The page and the Worker exchange ACP frames as `{frame}` messages. A message moves data, never a function, so every option is serializable. The bridges run on the same rule, one request message from the Worker and the answers of the page:

- A tool call of the module arrives at the page as `{tool: {tid, name, args}}`. The page runs `run(input, {signal})` of the tool and answers `{toolResult}`; `{toolAbort}` aborts the signal. A rejection is an error result.
- When the page passes `fetch`, the Worker replaces its global `fetch` with a bridge. A request goes to the page as a `{fetch}` message with the method, the URL, the headers, and the body. The page answers the status and the headers, then each body chunk as a transferred `ArrayBuffer`, then an end message or an error message. An abort of the request, or a cancel of the response body, posts `{fetchAbort}` and the page aborts its `AbortController`. Without `fetch`, the Worker calls its own `fetch`.
- `proxy.headers` and `proxy.baseURL` go to `harnessConnect`. A relative `baseURL` resolves against the page.
- `store.dir` under Node is a directory that one process holds through `.embed.lock`; a second `createHarness` on it fails. Without `store.dir` the store is in memory. A browser page cannot ask for a persistent store.

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
	// AskUserQuestion lets a backend that owns its loop ask the user a question,
	// which the embedder answers with Session.Resolve.
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
func (r *Runtime) Models() []protocol.Model
func (r *Runtime) Commands(ctx context.Context) (protocol.Commands, error) // the slash-command menu; the cause of ctx when it has ended
// The box: health, the work directory, the logger, and the processes of the box.
func (r *Runtime) Health() protocol.Health
func (r *Runtime) WorkDir() string // "" without a WorkDir
func (r *Runtime) WorkspaceChanges(ctx context.Context, dir, scope string) (protocol.WorkspaceChanges, error) // the git diff of the work tree; invalid_request without a WorkDir
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
| `internal/acp/cwdruntimes` | The `Runtimes` of the adapter: one `harness.Runtime` for each work directory, over one `Store` and one `Owner`. The host injects the config loading and the shared `Store` and `Owner`. It imports `harness`, `config`, and `internal/acp` |
| `internal/acp/rpc` | The newline-delimited JSON-RPC 2.0 connection of the adapter: request IDs, notifications, `$/cancel_request`, and one writer. It imports the standard library only |
| `internal/jshost` | The adapters of a JavaScript host for `GOOS=js`: the fetch `RoundTripper` and `FetchError`, JavaScript tools, the frame channel, and the readers of host options. It builds only for js and imports the standard library, `harness`, and `protocol` |
| `internal/listing` | The page of sessions in creation order, shared by `Runtime.List` and `httpapi.ReadHandler` |
| `internal/storecheck` | The name and record rules that `store.CheckName` and `store.CheckRecords` report and the runtime applies |
| `internal/eventlog` | The record codec, `Apply`, and `Check`. The event schema is in `protocol` |
| `internal/session` | Session actor, mailbox, state machines, views, replication, and the child commands |
| `internal/tree` | The child tree: limits, spawn, report, recovery, the tree interrupt, and the `task` tool. It reads the runtime through its own `Sessions` interface |
| `internal/selftool` | The `goal`, `model`, and `session_info` tools, which let a model inspect and change its own session. They reach the runtime through their own `Sessions` interface |
| `internal/turn` | One agent loop; declares `Backend`, `Tool`, and `Source`; `Restrict` |
| `internal/backend` | The `turn.Backend` of a runtime: it builds a backend for each provider of the registry (the native `anthropic` and `openai`, and each configured entry) and routes each model ref to one |
| `internal/backend/modelapi` | The one model API backend, for every provider wire |
| `internal/provider` | The wire adapters `anthropic`, `openai` (Responses, the Codex lane included), and `openaicompat`, and the typed `provider.Error`. `provider.Do` sends a request again after a transport error, and marks a transport error that declares itself permanent with `MarkPermanent`. Only `internal/backend` builds them |
| `internal/backend/external`, `claudecode` | Third-party harness backends: the shared adapter code, and Claude Code |
| `internal/tool/mcpsrc` | A `turn.Source` that gives MCP tools to each model call |
| `internal/tool/pluginsrc` | A `turn.Source` and `turn.Hooks` that give the plugin tools, hooks, and events to each session |
| `internal/tool/proc` | The `process` tool and the process status line |
| `internal/tool/builtin` | The file, search, and shell tools of a coding agent |
| `internal/toolresult` | Large-result retention and `read_tool_result` |
| `internal/admit` | Checks each part of an input and returns the `input.admitted` record with the blobs of its attachments |
| `internal/prompt` | System-prompt segments and agent profiles, and the engine-context sentinel: `RenderEngineContext` and `NeutralizeEngineContextSentinel` |
| `internal/workspace` | The git diff of the work tree for `GET /workspace/changes`. It shells out to git and cannot reach the runtime or any session |
| `internal/message` | The provider-wire conversation model |
| `internal/modelref` | `ModelRef` ("provider/model") and `Effort`, with their parsers. A leaf with no harness imports |
| `internal/backoff` | The wait between two attempts of a retried model request or model call. A leaf with no harness imports |
| `internal/modelmeta` | Context-window table from models.dev; only the model API backend reads it for a window. `Runtime.Models` and `GET /models` list the chat models of each configured `anthropic`, `openai`, `codex`, and `claude-code` provider; a dated snapshot id is not listed when its undated alias is in the table |
| `internal/mcp`, `plugin`, `skill`, `command`, `process`, `imageclamp` | The MCP client and server roles, the plugin host (`internal/plugin`, with the JSON-RPC transport in `internal/plugin/wire`), skill discovery, the slash-command registry, the process manager, and image clamping |
| `internal/wirerecord` | The program that records the real wire streams of `testdata/wire` and scrubs them. It makes real model calls only with `HARNESS_RECORD_WIRE=1` and reads keys from the environment |
| `internal/wirescrub` | Replaces ids, tokens, emails, home and temporary paths, and socket and pid paths in a recording with stable placeholders, and lists what a recording still holds |
| `internal/acptck` | The program that runs the ACP Test Compatibility Kit against `harness acp` with a scripted model and judges the report against `testdata/acp-tck-allow.txt` |
| `internal/wasmsize` | The program that builds `cmd/harness-wasm` for `GOOS=js` and compares the module size with `testdata/wasm-size-budget.txt` |
| `internal/testtime` | The program that reads `go test -json`, prints the test log, and fails a test or package over `testdata/test-time-budget.txt` |
| `internal/gates` | The repository checks of "CI gates" |
| `internal/testpoll` | Polling for cross-process waits in tests |

The `depguard` rules of `.golangci.yml` freeze the graph: `eventlog` imports `protocol` only, `turn` imports `backoff`, `eventlog`, and `protocol`, `internal/selftool` imports only `modelref`, `process`, `prompt`, `session`, `tool/proc`, `toolresult`, `turn`, and `protocol`, no package of `internal/backend` or `internal/tool` imports `session`, `tree`, or `internal/server`, only `internal/backend` builds a provider wire, only `internal/backend`, `internal/provider`, `internal/plugin`, `internal/prompt`, `internal/imageclamp`, and `internal/tool/pluginsrc` import `internal/message`, `internal/acp` imports the standard library, `harness`, `config`, `protocol`, and `internal/acp/rpc` only, `internal/acp/cwdruntimes` imports the standard library, `harness`, `config`, and `internal/acp` only, `internal/acp/rpc` imports the standard library only, `internal/jshost` imports the standard library, `harness`, and `protocol` only, `config` imports the standard library only, the public `plugin` package imports the standard library, `protocol`, and `internal/plugin/wire` only, and no internal package imports `cmd`.

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

- `State.History` holds the summary of the newest compaction and the messages after it. `State.ModelHistory` is `State.History` with each pinned segment (see Settle) at its place, and the model reads it.
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
| `Resolve(requestID, resolution)` | Append `request.resolved`; an answer starts a turn with no input, and a dismissal starts none. The receipt `protocol.Resolved` holds the `seq` of `request.resolved` and the `status`: `started` for an answer, `dismissed` for a dismissal |

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

### State machines

Session status derives from the turn and the open requests:

| Status | Condition |
| --- | --- |
| `idle` | No turn is running |
| `running` | A turn is running |
| `waiting` | A turn ended `awaiting_input`; a request is open |

`retrying` is not a session status. A turn that waits to call the model again sends an ephemeral `status` frame with `retrying`, `attempt`, and `next_at`, and the session stays `running`.

A compaction that the actor runs as its own run, automatic or from `Compact`, sends an ephemeral `status` frame with `compacting` when it starts, and one frame when it ends: `idle` when the summary is appended, or `compaction_failed` when it is not. Each of the three frames carries the compaction ID in `turn_id`. The session stays `idle` during the run, and the frames are never stored. A compaction inside a turn sends no such frame, except a compaction of the Claude Code CLI: when the CLI reports that it compacts, the turn sends a `status` frame with `compacting`, then `running` when the CLI reports the compact boundary (before the summary is appended as `compaction.applied`), or `compaction_failed` when the CLI reports a compact result that is not a success or ends the turn with no compact boundary. The session stays `running`, and these frames carry the turn ID in `turn_id`.

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

`turn.ended` types the cause in `cause`: `stopped`, `ended`, `goal_cleared`, `crashed`, or `provider_exhausted` (`handoff` is the cause of `turn.suspended`). `error` holds only the masked, capped message of a failure; a turn that ended by its cause alone has no `error`. A `provider_exhausted` turn keeps the provider message in `error` and the provider `RecoverHint` in `recover_hint`. Any other failed turn types its error in `error_class`: `permanent`, `timed_out`, `rate_limited` (a rate limit that outlasted the retries), the class name of another retryable error (`overloaded`, `server_error`, `stream_truncated`, `content_filter`, `connection_failed`), or `unrecovered`. Every reader branches on `stop_reason` and `cause` and never parses the text of `error`.

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

- `SetGoal` on a session with no running turn and no queued input admits the condition as an input with `source: goal`, which starts a turn through the normal input events. `SetGoal` while a turn runs, or with queued input, starts nothing; the next turn that ends is the first one evaluated. When nothing drives the goal, it is `blocked` (see below).
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

A backend opens a request with `Sink.Ask` on an open tool call of its turn, with the call ID as the request ID. The turn then ends `awaiting_input`, keeps the call open, and the session reads `waiting`. `Resolve` closes the request. A dismissal closes the call with an error result that says the user dismissed the question. An answer leaves the call open, and the turn that it starts records the result. That turn has no input; a dismissal starts none. A question takes an answer that maps each question to text. An input admitted while a request is open dismisses it first, and so does a queued input that starts after the turn. A settings change to a model of another provider dismisses it too. Claude Code asks with `AskUserQuestion` when `Options.AskUserQuestion` is set and the session is not a child and has no active goal.

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
- Each compaction summary ends with an index of the newest 32 retained results (see Tool-result retention).

### Children

A child is a session with `parent_id`. The `task` tool starts it in the background, and the runtime adds the tool only with a `WorkDir`. The parent holds the child ID, not the child's state. `protocol.Session.agent` is the agent profile name that the child was spawned with, from `agent` of its `session.created`; a root session has none.

- Spawn. The tool reads the agent profile (default `general-purpose`) and checks the tree limits. The parent actor appends `child.spawned`. Then the runtime creates the child in one append: `session.created` with `parent_id`, `agent`, the service tier of the parent, the effort of the call, the model of the call, else of the profile, else of the parent, and the allowed tools of the parent narrowed by the profile; the task as an input with `source: parent`; and the `turn.started` of that input. An effort that is not `off`, `minimal`, `low`, `medium`, or `high` fails the call, and so does a model that is not `provider/model` (an alias is not looked up) or that no provider of the registry serves. The tool returns the child ID at once. A child that fails to start settles `failed` with no input, and the tool call fails.
- Settle. When a turn of a child ends, the child reports to its parent: `done` for a completed turn, `failed` for a failed or crashed turn, `canceled` for a stopped turn. A completed turn with a queued input does not report; the next turn reports. The report names the child, its agent, the outcome, and the reason, and holds the last assistant text of the child. The parent appends `child.settled`, with the child turn ID as `result_ref`, and the report as an input with `source: child`, in one append. A child settles once, so a later report changes nothing until a `send` of the `task` tool spawns it again. The runtime opens a parent that it does not run, so a later input to a settled child opens its parent again.
  - Delivery. An idle parent starts a turn with the trigger sentence (see State machines, Input), and the segment follows it, so the text of the child reaches the model once, in the segment. A busy parent takes the report by its backend. On a model API backend the report is a `steer` input at the next item boundary. On a backend with `OwnsLoop`, the report is a `queue` input, and the next turn that starts reads it with every other report that waits. The choice reads the capability and never a backend name.
  - Segment. The model reads the report as a message of its own, not under the `OPERATOR MESSAGES` heading: `[tasks:`, a line `<child> (agent=<agent>) done: <text> (usage: <in> in / <out> out)` for each report that joins together, then `]`, in `<harness-engine-context>` tags on a model API backend. On Claude Code a turn that another input starts reads the segment after the text of that input (`\n\n[tasks: …]`).
  - Pins. The segment is pinned: the model reads it at the same place in each later model call, and it is in no history that a reader sees (`View.Messages`, the `log` of `task`, `get_conversation_history`, `client/session.messages`). Replay rebuilds a pin from the log, so a pin survives a restart. A compaction moves every pin to the end of the history that it leaves, so a pin never falls between a tool call and its result. A compaction inside a turn settles the pins there at once. A compaction outside a turn leaves them at the end for the model, and the next turn start settles them after the inputs of that turn.
  - Reason. A failed child reads `failed: <reason>`, and a stopped turn reads `failed: canceled`. The reason is a fixed prefix by `error_class` (`permanent`, `unrecovered`, another retryable class, `provider_exhausted`, `rate_limited`) followed by the error text; `timed_out` reads `timed out` alone, an error with no class reads as it is, a turn with no `error` reads its `cause`, and a crashed turn reads `lost to restart: turn was in flight when the process last stopped`. The error text is the `error_detail` of the turn end, or its `error` when there is none, masked and cut at 500 runes with `… [truncated]`; the recover hint is masked and cut at 120 runes. The `status` and `log` actions of the `task` tool show this reason. For `provider_exhausted` and `rate_limited` the line ends with guidance not to spawn a replacement and to resume the child with `task send` after the recover hint.
  - Result size. A result of at most 4096 bytes, after masking, stays whole in the report. A larger result is masked, and the report holds its first 4096 bytes and ` … [full result retained — read the rest with read_tool_result(handle="trh_N")]`: the parent retains the masked result as `tool_result.retained` in the append of `child.settled` (see Tool-result retention). When the parent cannot read the handle back (its backend owns the loop, its allowed tools omit `read_tool_result`, the retained total would pass the budget, or the blob write fails), the preview ends `… [truncated; full result unavailable]`. A runtime with no retention cuts the result at 4000 runes with `… [truncated]`. A newline in the text becomes a space. The `usage` is the total of the child.
- Crash and handoff. Each session follows its own rules. A child that a handoff suspended resumes when it opens, and a crashed child turn ends `crashed` and settles `failed`. When a parent opens, it settles each unsettled child that has ended, because a crash can come between the `turn.ended` of the child and the `child.settled` of the parent. A child whose last turn the end of a session stopped (cause `ended`) settles `canceled` with no report input; every other child that has ended settles with its outcome and its report, except a child whose routed report is being delivered. A child whose last turn a task cancel stopped (`task_cancel` on its `turn.ended`) reports to the nearest ancestor that has not ended its work, else to the root, read from the `session.created` records of the ancestors. When that ancestor is not the opening parent, the parent relays the report to it and settles the child with no report input; a relay that fails leaves the child unsettled for the next open, and the receiver of a relay keeps one report for each child and turn. No tool call runs again.
- Limits. One `internal/tree.Tree` holds `max_task_depth` (default 3) and `max_concurrent_tasks` (default 20) for each root. A negative value fails `Validate`. The runtime reads the root and the depth of a session once, when it loads the session: from its parent when this runtime runs the parent, else from the `session.created` records of the ancestors. The count is the unsettled children in the views of the sessions of the tree that this runtime runs, so a child that a parent opens after a restart counts at once. No ledger holds the count apart from the logs. A lock for each tree makes the count and the append of `child.spawned` one step. `max_tree_tokens` (default 0, no limit) is the token budget of a tree: a spawn fails once the root and its descendants have used that many tokens, input, output, and cache summed, read from the usage in each log of the tree. A session at `max_task_depth` has no `task` tool. A spawn past `max_concurrent_tasks` or `max_tree_tokens` fails the tool call with `engine: tree concurrency limit reached` or `engine: task tree token budget exceeded`, and a spawn that a race lets past the depth fails with `engine: task depth limit reached`. `HARNESS_MAX_TASK_DEPTH`, `HARNESS_MAX_CONCURRENT_TASKS`, and `HARNESS_MAX_TREE_TOKENS` set the keys through `ApplyEnv`.
- Actions. `action` defaults to `spawn`. `cancel`, `status`, `send`, and `log` take a `session_id` that the caller spawned, directly or through its own children; the tool walks the parents of the target to check it.
  - `status` reads the log of the target: its status (`running` until its last turn ends, then its outcome), parent, depth, children, agent, final text, failure, and usage.
  - `log` returns the newest `tail` messages (default 20, at most 100) as text entries, each cut at 2000 runes, in order, keeping the newest entries that fit in 20000 runes (always the newest one).
  - `send` admits the text as a steer input with `source: parent`, so a running child takes it at its next item boundary and an idle child starts a turn. When the parent has settled the child, the parent appends `child.spawned` again first, so the child reports the new turn.
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

A part is `{type: "text", text}` or an attachment `{type: "blob", media_type, data}` with its bytes in `data` as base64. Attachments are png, jpeg, gif, webp, and pdf. An attachment is at most 20 MiB, its bytes must be the type that it claims (an image decodes as that image, a PDF begins with `%PDF-`), and a request body is at most 32 MiB (8 MiB for any other route). A part that fails one check is `invalid_request` and leaves no trace. The runtime stores the bytes with `Store.PutBlob` under `attachment-<sha256>` before it appends `input.admitted`, whose blob part holds `media_type`, `blob_key`, and `bytes` and never the bytes. `Sync` carries the blob in the batch of that record, as it carries `backend.state`. A turn reads a blob through `turn.Request.Blob`. `modelapi` sends it as an image or file part of the wire, and `claudecode` sends an `image` or `document` content block on stdin after the text block. A steer input keeps its blob parts. The blob is stored before the input is appended, so an input that then fails (a conflict, a turn mismatch, or no ownership) leaves its blob in the store; a retry with the same bytes reuses it.

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
- The IDs derive from the log: `msg_<input_id>` for an input that a turn started with or that a steer promoted, `msg_<item_id>` for an item, `msg_resolved_<request_id>` for the result of a dismissed request, and `cmpsum_<seq>` for the summary.
- A message holds `id`, `role`, `parts`, and `created_at`, the envelope time `t` of the record that completed the message: the `turn.started` record for a user message that an input started, the newest `input.promoted` record for a user message of steer inputs, the `item.completed` record for an item, the `request.resolved` record for the result of a dismissed request, and the `compaction.applied` record for the summary.
- A user message also holds the `source`, `source_id`, and `source_label` of the input that started its turn, also when the runtime admitted that input itself (`parent`, `child`, `goal`, or `harness`), so a reader tells it from the input of a person. A user message of steer inputs that joined a running turn (the `OPERATOR MESSAGES` block) holds no source of its own and an `operator_batch` instead: one entry of `id`, `text`, `source`, `source_id`, and `source_label` for each input, in the order that the block numbers them.
- A tool result holds its text in `content`, and a blob part names its media type, size, and `key` (see Inputs).
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
| `not_a_git_repo` | 409 |
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

Each code except `internal`, `payload_too_large`, `unauthorized`, and `not_a_git_repo` is a sentinel error, and every code is a `protocol` constant of type `protocol.Code`. A sentinel of the Go API is a `*protocol.Error`, and `Error.Is` matches another `*protocol.Error` with the same code, so an error that a client decodes from a body matches the sentinel of its code. A stop cause (`ErrSessionHeld`, `ErrSessionReleased`, `ErrSessionEnded`, `ErrOwnershipLost`) is a `session_not_owned` error with a cause in its details, and it matches only its own cause and `ErrSessionNotOwned`. The body carries that cause in `details.cause`, so an error that a client decodes matches the same sentinel. The Sync receiver codes are not in the table, so `protocol.HTTPStatus` answers 500 for them, and a receiver chooses its status from the sync wire contract. `protocol.CodeOf(err)` returns the code of the first `*protocol.Error` in the chain of `err`, or `internal`, and `protocol.HTTPStatus(code)` returns the status of the table, or 500 for an unknown code. `New` returns a plain wrapped error for a bad `Options`, never `ErrInvalidRequest`. A log that does not replay fails with `ErrUnreplayable`, code `session_unreplayable`, and its body message is fixed. `cmd/harness serve` writes `unauthorized` in its token check, before the request reaches `internal/server`. The session codes are sentinels in `harness`, and `blob_not_found` is `ErrBlobNotFound`. `process_not_found` is `ErrProcessNotFound`, and `not_a_repo`, `no_base`, and `too_many_changes` are sentinels of `internal/workspace`. `not_a_git_repo` is a deprecated name of `not_a_repo`, has no sentinel, and is never sent: the server answers `not_a_repo` for a directory that is outside a repository. `internal/server` takes the code of an error from `protocol.CodeOf`, after its own dependencies' sentinels. A body above its limit fails with `payload_too_large`: 32 MiB for `POST /sessions/{id}/inputs`, and 8 MiB for any other route. Any other error is `internal`, and its message is a fixed string. A path or method that no route serves answers 404 or 405 with `invalid_request`.

The actor masks and bounds each error text that it writes to the log (`turn.ended` error, `goal.changed` reason): recognized credential shapes are redacted (best effort: a free-form text can hold a shape that the fixed patterns miss), and a text longer than 256 runes keeps its first 256 runes and ends with `...[truncated]`. A failed `turn.ended` also holds the message as a report to a parent reads it, in `error_detail`, when that differs from `error`: masked by the secret patterns and cut at 500 runes with `… [truncated]` (see Settle).

### Slash commands

`Session.Prompt` resolves an input with `source: typed` and one text part through the `command` package. Any other input is never a command.

- Not a command: the input is admitted as is. `//x` is admitted as the text `/x`.
- An unknown name: the prompt command of that name under `commands_dirs` (default `<WorkDir>/.agents/commands`) is admitted as its expanded text with `source: command` and the typed line, cleaned as `source_label` is, as its `source_label`; the client's `source_id` stays. With no such file, or no `WorkDir`, the line is admitted as text.
- Bad arguments: one `command.recorded` with `failed` and the error of `Resolve`. Nothing runs.
- A frontend command, or a control command with no operation here (`queue-clear`): `unsupported`, "/<name> is not available in this client".
- A control command that is not `available_during_task` while a run is on: `refused`, "/<name> cannot run while a turn is running; send it again after the turn ends".
- Any other control command records `accepted`, runs after `Prompt` returns as work that `Runtime.Close` waits for, and records one more status. After `Close` starts, such a command fails with `draining` and records nothing. The statuses: `succeeded` ("/<name> succeeded", with the JSON result up to 16 KiB), `failed` (the error text of a sentinel error, or "/<name> failed: internal error", also for a panic of the operation), `refused` (a `session_busy` error), or `interrupted` (the runtime stopped).
- The operations are the Go API: `abort` is `Interrupt`, `compact` is `Compact`, `goal` is `SetGoal`, `goal-clear` is `ClearGoal`, `model`, `thinking`, and `tier` are `Update`, `status` is `View`, `queue` is the queued input IDs, and `processes` is the process list of `GET /processes`. `/compact [keep_turns]` passes `keep_turns` to `Compact`. Its result is the `protocol.Compacted` of `Compact`. A compaction with no turns to fold fails with "/compact did nothing: the session does not have enough turns yet to fold".
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
- `acp` serves ACP over stdin and stdout (see ACP). It takes no flag. It reads no config file, makes no network call, and builds no runtime until the first `session/new`, `session/load`, `session/resume`, or `session/list`. Then `internal/acp/cwdruntimes` builds one `harness.Runtime` for each `cwd` that such a request names, and one for the work directory of the process for a `session/list` with no `cwd` when it holds none, with `config.LoadProject` of the user config and that `cwd` and `WorkDir` set to that `cwd`, over one `DiskStore` and one `NewLocalOwner`. It takes the session directory and the owner epoch from the config of the first `cwd`. It sets no `Sync`. A `cwd` that is not a directory is `-32602`. A `SIGINT` or `SIGTERM` ends it with status 0.
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
- Two numbers that are equal name one id, so `1` and `1.0` are the same id. A request whose id names a request that still runs is answered with `-32600`.
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
| `authenticate`, `logout` | `-32601`. The agent advertises no authentication |
| Any other method | `-32601` |

### Initialize

- `initialize` answers the agent info, the agent capabilities, and an empty `authMethods`.
- The agent info is `name` `harness`, `title` `Harness`, and the harness version.
- When the client version is not 1, the answer is version 1, the latest version of the agent. When it is 1, the answer is 1.
- `initialize` without a `protocolVersion` that is an integer from 0 to 65535 is `-32602`.
- The agent capabilities are `loadSession` true, `mcpCapabilities` with `http` true and `sse` false on a host that starts MCP servers (`harness acp` is one), and `sessionCapabilities` with `resume` and `close`, and with `list` on a host that has a work directory for a `session/list` with no `cwd` (`harness acp` is one). Without `list`, `session/list` is `-32601`. `session/delete` is not advertised. They hold no `promptCapabilities`: the agent advertises a capability only when the method that uses it works. A prompt takes the baseline content: `text` and `resource_link`.
- The agent never sends `session/request_permission`: harness has no permission system.
- The agent reads one client capability, `session.notices`. A `notices` that is an object turns on the lost-ownership `notice` (Notices). Any other value, and a missing `session`, leave it off.

### Sessions

- `session/new` takes `cwd` and `mcpServers`. A `cwd` that is not an absolute path of a directory is `-32602`. A `session/new` with another `cwd` runs on another runtime, which uses the project config of that `cwd`. The session has the origin `acp`. `additionalDirectories` is ignored and not advertised.
- The `mcpServers` of `session/new` are the MCP servers of that session only. `harness.WithSessionMCP` receives them: a stdio entry as the command and its arguments, and an `http` entry as the URL and its headers. They are not stored. A name that two `mcpServers` entries share is `-32602`. So is an `sse` entry. On a host that starts no MCP server, any entry is `-32602`.
- `Config.AllowedTools`, when set, is the `allowed_tools` of every session that `session/new` creates. `harness acp` leaves `Config.AllowedTools` unset, so a session has every tool.
- The connection holds each session that it created. Its event pump is the only reader that turns events into updates. It starts at the head of the log when `session/new` returns, and it ends when the connection ends.

### Load, resume, list, and close

- `session/load` takes `sessionId`, `cwd`, and `mcpServers`. `session/resume` takes `sessionId` and `cwd`, and `mcpServers` when the client has them. A `session/resume` without `sessionId` or `cwd` is `-32602`. The `cwd` selects the runtime, and `mcpServers` follows the rules of `session/new`. A session that the store does not hold is `-32002`. A session whose log names a `work_dir` other than the `cwd` is `-32602`.
- The agent reads one view of the session with `Runtime.Read`. The head of the view is the replay boundary. `session/load` sends the conversation of the view, oldest first, then the `available_commands_update`, opens the session, and answers with the `configOptions` of the session. `session/resume` sends the `available_commands_update` and no conversation.
- The replay sends a user message as `user_message_chunk` updates, and an assistant message, a tool call, and a tool result as the updates that a turn sends, with the same `messageId` and `toolCallId`. The `diff` of a call stays in its result. A part of type `engine_context` or `task_report` is not sent. The status text of a typed command that has ended follows the message that the command follows. An image in a tool result is not sent. A tool result whose text is exactly `(no output)` is replayed with no text, because a reader shows an empty result as that text.
- The event pump of a loaded or resumed session starts at the replay boundary, so a record that the log gains between the read and the open reaches the client once, as a live update. The pump keeps the `diff` of a call that has no result at the boundary, so its `tool_call_update` keeps the `diff`.
- Opening a session resumes the work that its log holds: a turn that ran, a queued input, or a goal. The updates of that work are live updates, and they can reach the client before or after the answer. The later `Open` wins across processes: it takes the session, and the earlier owner gets `session_not_owned` at its next prompt. Inside the process, a session with no `work_dir` that another runtime runs is `-32603` with `data.code` `session_not_owned`.
- The agent runs the `session/load`, `session/resume`, and `session/close` requests of one session one after the other, in the order they arrive. A request waits until the answer of the one before it is sent. So a load that follows a close opens the session after the release, and two loads of one session start one pump.
- A session that has a parent is a task child of another session. `session/load` and `session/resume` of it are `-32602`.
- A `session/resume` of a session that the connection holds returns the held session and sends nothing. A `session/load` of it waits until the event pump has sent every update through the head of a new view, replays that view, sends no command update, and answers. The replay does not change the state of the pump. A `cwd` other than the one of the held session is `-32602`, and so are `mcpServers` in either request, because the held session keeps the servers it has.
- `session/list` takes an optional `cwd` and an optional `cursor`. It lists the root sessions of the store in creation order. A session that has a parent is a task child of another session and is not listed. A session that has no `work_dir` is not listed. With a `cwd`, only the sessions whose `work_dir` equals it are listed. A `cwd` that is not an absolute path is `-32602`. A listed session has `sessionId`, `cwd`, a `title` that is null, and `updatedAt`. The agent reads 50 sessions of the store for each step and continues until the page holds a session or the store ends. `nextCursor` is present when more sessions can follow. A `cursor` that names no session is `-32602`. One call reads at most 4 steps. A call that reads them all and finds no match answers no session and a `nextCursor`. Without a `cwd`, the agent lists through a runtime that it holds, else through the runtime of the work directory of the process.
- `session/close` cancels the prompts of the session as `session/cancel` does, waits for their answers and for their updates, stops the event pump, and releases the session. The log stays, so the session can be loaded again. A prompt that was in flight answers `cancelled`. A `sessionId` that the connection does not hold is `-32002`.

### Prompts

- A `session/cancel` that follows a `session/prompt` on the wire reaches that prompt. This holds also when the prompt has not started to run.
- `session/prompt` runs one turn and sends the text of the model as `agent_message_chunk` updates before it answers `end_turn`. The reasoning of the model is `agent_thought_chunk` updates. A text delta is a chunk; the rest of an item that the deltas did not cover is one more chunk when the item completes. When a slow reader missed a delta, the chunks sent are not the start of the item text, and ACP version 1 cannot retract a chunk. Then the whole item text is sent as one more message, with the `messageId` `msg_` and the item ID and `_full`. The `messageId` of a message chunk is `msg_` and the item ID, and that of a thought chunk is `thought_` and the item ID.
- The agent mints the ID of the input, and sends the input with the delivery `queue`: a prompt that arrives during a turn waits for the next turn. A prompt block other than `text` and `resource_link` is `-32602`. So is a prompt with no block. A `resource_link` is the text of its name and URI.
- A required field of a content block or of an `mcpServers` entry that is omitted or null is `-32602`. Two header names that differ only in case are one name.
- A prompt for a session that the connection does not hold answers `-32002`.
- The updates of a turn reach the client before the answer: the answer waits until the event pump has sent every update through the end of the turn.
- A turn that starts with no `session/prompt` open, such as a goal turn, sends its updates as notifications.
- The stop reasons are: `completed` and `awaiting_input` are `end_turn`; `interrupted` is `cancelled`. After `session/cancel`, the prompt that runs answers `cancelled`, after every update that it has pending. A prompt that a cancel withdraws before a turn takes it answers `cancelled`.
- A turn that fails answers `-32603` with `data.code` `internal` and `data.error_class`; the message is the masked error of the turn.
- `$/cancel_request` for a prompt also interrupts its turn.
- A tool call of the model is a `tool_call` update. The adapter sends it when the assistant message that holds the call completes, after the text of that message. It holds the `toolCallId` (the call ID), the `name` of the tool, a `title`, a `kind`, the status `in_progress`, and the arguments as `rawInput`. No frame streams the arguments, so the status `pending` is not used.
- The `kind` of a tool comes from a table of tool names: `read_file` and `read_tool_result` are `read`; `write_file` and `edit_file` are `edit`; `glob`, `grep`, and `ls` are `search`; `bash` and `process` are `execute`; the file and shell tools of Claude Code are `read`, `edit`, `search`, and `execute`, its `WebFetch` and `WebSearch` are `fetch`, and its `TodoWrite` is `think`. Any other name, including each MCP tool and plugin tool, is `other`.
- A file tool adds `locations` to its `tool_call`: the absolute path of the file, and for `read_file` the `line` of its `offset`. A relative path resolves against the `cwd` of the session.
- An `edit_file` call adds one `diff` content item with the absolute `path`, `oldText` as the `old_string`, and `newText` as the `new_string`; a `replace_all` edit sends that pair once. A `write_file` call adds a `diff` with `oldText` null and the content as `newText`.
- The tool message that holds the result is a `tool_call_update` for the same `toolCallId`, sent when the message completes. Its status is `completed`, or `failed` when the result is an error. Its `rawOutput` is the text of the result. Its `content` replaces the content of the call: the `diff` for a call that succeeded and has one, otherwise the text of the result when it is not empty. Each image of the result follows as an `image` content item. A blob that is not an image is not sent.
- A tool call that the cancel of a turn cut short has an error result in the log, so its update is `failed`, and it reaches the client before the `cancelled` answer.
- At the end of the input, the agent releases its sessions first, so a prompt that runs answers `-32603` with `data.code` `session_not_owned`. Then it closes its runtimes, within 5 s, and writes the responses.
- A prompt for a session that another owner took answers `-32603` with `data.code` `session_not_owned`. When another owner takes the session, the prompt that runs answers the same error. So does each later prompt.

### Config options

- `session/new` answers `configOptions` with two `select` options. `model` has the category `model`. Its values are the `provider/model` references that `Runtime.Models` lists, and its current value is the model of the session. A model of the session that `Runtime.Models` does not list is added to the values. `effort` has the category `thought_level`. Its values are `default`, `off`, `minimal`, `low`, `medium`, and `high`. `default` is an effort that is not set.
- `session/set_config_option` takes a `configId` and a string `value`. It changes the setting of the session and answers the complete list of options. A `configId` other than `model` and `effort`, a `value` that the option does not list, and a boolean `value` are `-32602`.
- When the model or the effort of the session changes, the agent sends a `config_option_update` with the complete list of options. A `settings.changed` event that leaves both as they were sends none. This includes a change by `/model`, by `/thinking`, and by the `model` tool. The update of a `session/set_config_option` reaches the client before the answer.

### Commands

- After the answer to `session/new`, the agent sends an `available_commands_update`. It lists each command that the runtime can run from a typed line. The `description` is the summary of the command. The `input.hint` is its argument hint, and is omitted when the command has none.
- A prompt that is exactly one `text` block whose text starts with `/` is typed text. The runtime runs the slash command. The status text of the command is an `agent_message_chunk`, and the prompt answers `end_turn`. A command that the runtime ends as interrupted answers `cancelled`. A line that names no command goes to the model as text. A prompt that has other blocks next to the text is not typed text.

### Usage and session info

- A `usage_update` has `used` and `size` from the context of the session. The agent sends it when `used`, `size`, or the cost changes at a `context.measured`, `compaction.applied`, or `turn.ended` event. It sends none while the context window is not known. The `cost` is the session cost in `USD`, and is omitted when the session reports none.
- A `session_info_update` with `updatedAt` follows each `turn.ended` event. It has no `title`.

### Notices

- When another owner takes a session from the connection and the client has the capability `session.notices`, the agent sends one `notice` update with the severity `warning` for that session. Without the capability, the agent sends no notice.

### Error mapping

One function maps a harness error to a JSON-RPC error by its `protocol` code. `data.code` is that code, and `data.cause` is the stop cause of a `session_not_owned` error.

| Code | JSON-RPC |
| --- | --- |
| `invalid_request`, `input_conflict`, `request_not_pending` | `-32602` |
| `session_not_found`, `blob_not_found` | `-32002` |
| `session_not_owned`, `session_busy`, `turn_mismatch`, `session_exists`, `draining`, `model_unavailable` | `-32603` |
| `session_unreplayable`, `internal` | `-32603` with the message `Internal error` |
| `payload_too_large`, `unauthorized`, `not_a_git_repo`, `not_a_repo`, `no_base`, `too_many_changes`, `process_not_found` | `-32603`. No ACP method returns them |

An error with no code is `internal`. `model_unavailable` is not `-32000`: the agent advertises no authentication.

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
	Started() string               // announces an item and returns its ID; the next Item records under it
	Announce() string              // Started for a keyed thread; Item does not use the ID
	ItemAs(id string, m protocol.LogMessage) error // records m under the ID that Announce returned
	Attach(mediaType string, data []byte) (protocol.LogPart, error) // stores a tool-result blob and returns its part
	Settings() (string, protocol.Settings) // the model and settings that the session holds now; "" after the turn stops
	Pin(ns []Notice, at int) []Pin // pins the notices that changed, on the actor goroutine, and returns every pin
	CompactTurn(ctx context.Context) (history []protocol.LogMessage, ok bool, err error)
	Ended(err error)
}

func Run(ctx, step context.Context, b Backend, req Request, src Source, to Turn, lim Limits)
```

`Delta` ignores the backend `itemID`: one harness item can join several backend items, such as reasoning and text. The first delta of each model call calls `Started`, which gives the item a harness ID; the next `Item` records the message under it. An item that a failed call streamed gets no `Item`, and the next `Started` replaces it.

- A model API backend runs one model call per `Run`. The loop runs the tools, then takes the queued steer inputs through `Sink.Steer`, appends them to the history, and calls the backend again. This item boundary comes after the tool results of each response, on a path that calls the backend again. A response that ends the turn, or fails it, takes no steer input: that input waits and runs as the next turn. A queued input joins no running turn. Every model API backend steers, because the loop does it, not the backend.
- A delegated backend (`claudecode`) runs the whole turn and reports items.
- `AllowedTools` holds tool names in one namespace: the embedder tools, the runtime tools (`goal`, `process`, `task`), the plugin tools, the MCP tools, and the built-in tools of the model. A model API backend has the file tools of the `WorkDir` and `read_tool_result` as its built-in tools. A delegated backend has the tools of `Capabilities.Tools`, and any other name fails `Create`. A tool of the runtime or of a plugin with the name of a built-in tool of the backend also fails `Create`. One predicate, `known(model, name)`, decides whether a name is a tool of a session at that model. `New`, the plugin start, `Create`, `Update`, and the tool list of a child profile all use it.
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

`Options.ModelTransport` wraps the HTTP client of each entry, and may supply the credentials of each wire. Each request sends a baseline response cap of 8192 tokens. A reasoning request can raise it. `Close` reaches a client that pools connections through an optional `Close` method. Each provider package keeps its own SSE reader and status classifier, and the policy of a model call retry stays in `turn`. The one policy of a request retry is `provider.Do`.

### Third-party harnesses

A third-party harness is a `Backend` with `OwnsLoop`, `OwnsContext`, and `Steering` set, behind one adapter contract. Claude Code is the one built. A new harness adds one package and touches nothing else.

| Concern | Contract | Claude Code |
| --- | --- | --- |
| Turn | One harness turn is one external turn, except the second turn that the external harness runs for a `steer` input it took after its result (see Steer below) | `--resume` print run |
| Items | External items become `item.completed` | stream-json frames |
| Steer | A `steer` input reaches the running turn | stdin |
| Interrupt | Stops the external turn; reports `interrupted` | SIGINT |
| Requests | Questions and approvals become `request.opened`; the resolution goes back | `AskUserQuestion` defer |
| State | External session id and transcript mirror are `backend.state` records with appended entry chunks | `--session-mirror` |
| Tools | Harness tools reach the external harness through a harness-hosted MCP endpoint; `AllowedTools` applies. An unrestricted turn gives `Config.MCPServers` to the external harness beside that endpoint | `--mcp-config` with `--strict-mcp-config` |
| Context | The external harness compacts; harness logs `compaction.applied` with `by_backend` | `/compact` |
| Telemetry | Usage, cost, and context window arrive through `Sink.Telemetry` | `result` event |

`internal/backend/external` holds what every adapter shares: process supervision, the line-protocol transport, the mirror writer, and the MCP bridge. `internal/backend/claudecode` holds only the mapping in the table.

- History bridge. A session may move between a backend that owns its loop and any other backend. A stream-json input cannot seed prior history, so each turn of such a backend has the harness tool `get_conversation_history`, which reads the live history page by page and which no `AllowedTools` list hides. When another provider recorded a message after the newest message that this backend saw, the backend adds one line to its `--append-system-prompt` value that tells the CLI to call the tool before it answers. A turn belongs to the provider of its model at its start, even when a settings change moves the model in the middle of it.
- Tool calls. The backend records the assistant item that holds a tool call when the `tool_use` frame arrives, and a tool result when its frame arrives, so `View.Messages` and the event stream show a running tool before its result. A later frame of the same API response starts a new item, so parallel calls of one response are separate items in wire order.
- Text deltas. Claude Code runs with `--include-partial-messages`. The backend forwards each `text_delta` and `thinking_delta` of a `stream_event` frame as an `item.delta` frame, and the `assistant` frame that follows sends only the part that the deltas did not send, so the deltas of a block add up to its text. The main thread and each subagent have a live item of their own. A reply that the CLI drops and retries starts a new live item, and the dropped one never completes: the backend sends an `item.discarded` frame with the `item_id` of the dropped item before the first delta of the new one, and a client removes that live item. A tool call has no delta. The deltas are live only: the log, the replay, and `View.Messages` hold the finished blocks.
- Subagent frames. Claude Code runs with `--forward-subagent-text`. The text, the tool calls, and the tool results of a subagent become items, and `Message.ParentCallID` names the tool call that started the subagent, so a client nests them. History hands the model the parts alone.
- Placeholder result. The empty zero-turn `result` that the CLI prints for a queued task notification is skipped when the run has recorded no assistant item, and adds no usage. After an assistant item it ends the turn with the usage of that result.
- Questions. A defer hook parks each `AskUserQuestion` call and the CLI answers its result with `stop_reason: tool_deferred`; the backend opens the request. An answer with no input answers the call over the control channel, with the answers added to the tool input. Any other resolution denies the call with an interrupt in a run of its own, which records nothing, before the prompt runs. A run that answers a question takes no steer input: the input waits for the next turn.
- Steer. The backend writes a `steer` input to stdin, and promotes it, only while a tool call of the main thread waits for its result: the CLI takes a queued message at the next tool result. A `steer` input that arrives at any other time, such as in a text-only reply, stays queued and unpromoted, and the next turn carries it as it carries a `queue` input; the input joins the turn at the next tool call when one starts before the turn ends. When the CLI ends the turn with a result and no tool result came after the write, it runs the message as a second turn and writes a second `result`. The backend then reads on, records the items of the second turn in the same turn, and ends the turn at that result, with the cost that the first result did not report; a CLI that exits first fails the turn. This keeps the one-turn-one-input-batch event model: the input stays promoted to the turn that took it, so no event un-promotes an input. A tool result that the CLI wrote before it read the steer line still counts as delivery, so in that race the CLI runs the input as a next turn that the backend does not record. A failed first result takes no second result: the turn fails and the promoted input gets no answer.
- Context and cost. A Claude Code turn reads its context gauge and its cost from the `result` frame. The gauge reading is the prompt size of the newest assistant frame of the main thread, or the usage of the `result` frame when no assistant frame carried usage, with the window of `modelUsage`. A `compact_boundary` with positive `post_tokens` sets the reading until a later main-thread assistant frame reports usage. The `result` frame of each turn adds its `total_cost_usd` to `session_cost_usd` of `subscription_usage`, also when it fails the turn. A second `result` of one CLI run adds only the part of its running total that the earlier `result` did not report. A `result` frame sets the reading also when it carries no prompt tokens. The window of a reading stays when a later reading of the same source reports none; a change of model clears it.
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

- `New` builds the tool list from `Options.Tools`, the `goal` tool with `goal_evaluator_model`, and, with a `WorkDir`, the `process` and `task` tools. An empty or repeated name fails `New`. The built-in tools of a `WorkDir` belong to each session; see Built-in tools.
- The runtime builds one `turn.Source` for each session when it loads the session. In order, it gives the runtime tools, with `goal` and `task` bound to the session ID and no `goal` for a child; `goal` only for a backend that does not own the loop; the file tools of the `WorkDir`, for a backend that does not own the loop; `internal/tool/mcpsrc`, except for a backend with `OwnsMCP` on a turn with no `AllowedTools`; and `internal/tool/pluginsrc`. The actor adds a last `Source`: `get_conversation_history` for a backend that owns the loop, which no `AllowedTools` list hides, and `read_tool_result` with the retention hooks for a harness loop. `turn.Sources` joins the `Source`s, and `turn.Run` takes no other tool list. Each `Source` gets the `AllowedTools` of the session for each model call and applies `turn.Restrict` once.
- The tools of a model call keep one order from call to call, because the tools come first in the cached prefix of every provider and a changed byte invalidates the whole prefix. A `Source` returns its tools in a fixed order and never ranges over a map: `mcpsrc` orders the servers by name and the tools of a server by name.
- `turn.Sources` chains the `turn.Hooks` of its `Source`s in order: `Before` runs in order and a deny ends the chain, and `After` runs in order.
- The loop runs the tool calls of a response at the same time, up to 8 at once. Results reach the log in call order: a result is recorded as soon as it and every call before it have ended. A tool that is `Alone` waits for every call before it and holds back every call after it. The calls of a `Keyed` tool with one key run one at a time in call order: `read_file`, `write_file`, and `edit_file` key on the resolved path with symlinks resolved, `process` on the process name, and `task` on the target session. `goal`, `model`, and `mcp` are `Alone`. `Before` and `After` run around each call, and calls that run together run their hooks together, so a hook keeps no state across calls that it does not guard. A panic in a tool is an error result of that call. An interrupt cancels every running call and starts none; every call that ran has its own result recorded in call order, including a call that the interrupt cut short, and a call that did not start gets the result of an interrupted call. When only the step ends, as in a handoff, running calls finish and no call starts. A tool error, or a call to a tool that the model may not call, is an error result that the model sees.
- An embedder tool receives its session, the root of the session tree, and the turn in `harness.Call`. The runtime binds them for each model call, also for `AloneTool.Alone` and `KeyedTool.Key`. The `goal` and `task` tools hold the session ID that the runtime binds when the session starts. `turn.Tool` takes a `protocol.ToolCall` and no session.

Agent profiles name a kind of child, in the agent format of Claude Code so one file serves every backend: `name`, `description`, `tools` (comma separated; omitted allows every tool of the parent), `model` (a ref or an alias; omitted or `inherit` keeps the model of the parent), and a prompt body. `color` is read and ignored.

- `internal/prompt.Profiles` reads `*.md` in each of `agent_defs_dirs` (default `<WorkDir>/.agents`; a relative path joins `WorkDir`) at each spawn, beside the built-in profiles, which a file of the same name replaces. The built-in `general-purpose` allows every tool of the parent. The built-in `explore` and `plan` allow only the read-only file tools of both kinds of backend (`read_file`, `glob`, `grep`, `ls`, `Read`, `Glob`, `Grep`) and `session_info`, and `plan` asks for an implementation plan.
- A file with a key that the format does not know is skipped with a WARN log line, unless the file has another error too, and then the load fails with the other error. A line under an unknown key that holds no colon is such an error, so an unknown key with a list or a block of text under it fails the load; a line under it that holds a colon reads as one more unknown key. Any other file that is not valid, such as one with no `name`, fails the load, and the error names the file: the spawn fails its tool call. A name that two files repeat, in one directory or across `agent_defs_dirs`, fails the load the same way, and the error names both files. A session that exists opens as before, with the profile of the first file of that name.
- Tool names differ by backend, so a spawn keeps only the names of a profile that `known` accepts for the model of the child: a runtime tool, a plugin tool, an MCP tool, or a built-in tool of its model. A profile left with no tool logs a WARN line.
- A profile applies through the allowed tools of the child (`turn.Restrict`) and its body as the last segment of the system prompt of the child. The runtime reads the profile once, when it starts the child, and the spawn uses that same read for the model and the tools of the child.

### Built-in tools

With a `WorkDir`, each session of the harness loop gets the built-in tools `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `ls`, `bash`, `read_tool_result`, and `session_info`. An embedder tool or a plugin tool with one of these names fails, and so does an embedder tool named `model` with a `WorkDir`, whether or not `model_tool` is false.

- The `model` tool. With a WorkDir and `model_tool` not false (default true), each session also has the `model` tool: `status` reports the model, the aliases, and the providers, `list` reports the providers and the aliases alone, and `set` takes a ref or one alias and calls `Session.Update`, so the model changes at the next model call. The providers are the registry: the native `anthropic` and `openai`, and each configured entry. A native provider needs no entry, and a `set` to it builds its client with the key of its default environment variable. An unserved provider fails with the valid aliases and providers listed, and a session has no action that clears its model. Each provider has a `billing` of `subscription` for `claude-code` and `codex`, and `api` for any other. A backend that owns its loop gets the `list` action alone, because `set` would move the model of the turn that calls it.
- `session_info` takes no argument and reports the model, the effort, the service tier, and the usage of its session from the view; the system prompt that the newest turn sent, with the MCP and plugin segments of its newest model call; the tool names of that call; the instructions files and the skills that the session read when it started; and the plugin inventory. It reports what the session loaded and sent, so a file that changes later does not change the report.
- Without a `WorkDir`, a session has none of them, and no result is retained.
- The runtime gives each session its own file tools in its `Source`, because the `write_file` guard belongs to one session: `write_file` overwrites only a file that the session read or wrote, with no change on disk since. The guard is in memory, so after `Open` the model reads a file again before it overwrites it. The tools cannot be one `Options.Tools` list for the runtime.
- The actor builds `read_tool_result` over itself, so the tool gets its dependencies when it is built. `session.Config.Retain` turns this on, and the runtime sets it with a `WorkDir`.
- A backend that owns the loop, such as Claude Code, gets none of the others and has no retention. It has its own tools.
- A file-size cap of 20 MiB bounds one file: `read_file`, `edit_file`, and the `write_file` guard read at most that many bytes of a file and fail on a larger one. Because calls run at the same time, a budget of 64 MiB for each session also bounds the file bytes that those three hold at once. A call reserves the size of its file before it reads, waits in order when the budget is full, and a file above the budget takes the whole budget and reads alone.
- `read_file` reads an image as the summary line `image (<type>), <n> bytes, <w>x<h> pixels`, and the image itself as a blob of the result. `bash` adds the variables that the `shell.env` hooks of the plugins return to the environment of its command.
- Tool-result images. A `protocol.ToolResult` holds `Text` and `Blobs`, each blob a media type and its bytes; `read_file` of an image and an MCP result with image, audio, or resource-blob content return blobs. A turn stores each blob with `Store.PutBlob` under `toolblob-<sha256>` before it appends the `item.completed` whose `tool_result` part holds `blobs` (`media_type`, `blob_key`, `bytes`, never the bytes), and `Sync` carries the blob in the batch of that record. A model API backend reads each blob through `turn.Request.Blob` and sends it inside the tool result, so a replay and the next owner send the same image. When the `Store` lacks the bytes of a blob, the backend logs a WARN and sends the text without that image. Retention counts the text of a result only. A reader of the history that shows text (`View.Messages`, `get_conversation_history`, the `log` of `task`) shows no blob.

### Tool-result retention

`internal/toolresult` holds the retention of large tool results and `read_tool_result`.

- A tool result above 16384 bytes has its secrets masked, goes to a blob, and the history holds a header with a `trh_N` handle and the first 16384 bytes. A result that fits after the mask stays inline. `read_tool_result` reads the blob back by line window or literal search, bounded by `max_bytes`. Its own result is never retained.
- The `Source` that the actor adds to an agent turn has a `turn.Hooks` that retains each result after the plugin hooks, so the blob holds the text that the model would see. It retains in the `Join` step, which a batch runs as it records each result in call order, not in the `After` of a call that runs beside others, so handle numbers follow call order and two results never share a handle or a blob. The turn records the preview, and the next model call of the turn never carries the whole result. The blob is written outside the actor, and the record is appended only while the turn runs.
- A `tool_result.retained` record names the blob, and `Sync` carries the blob with the record, as for `backend.state`. The handle numbers count these records, so a replay and the next owner continue the count. The blob key is the handle and the fence seq, so a fenced owner never overwrites the blob of the next owner.
- A turn whose allowed tools omit `read_tool_result` retains nothing, so a preview never names a tool that the model cannot call.
- A result that would take the retained total of the session above 4 MiB keeps its preview with a notice and no handle, and nothing is written. A failed write keeps the whole result.
- The result of a done child in a report is retained the same way when it is larger than 4096 bytes once masked: the preview is 4096 bytes, the tool name is `task`, and the record joins the append of `child.settled`, so a handle always names a blob that exists. The handle takes the next number at that append, and a result of the session that is retained at the same time takes the next free number. The blob key is `report-<child turn id>-<fence seq>`, because the handle has no number before the append.
- Each compaction summary ends with an index of the newest 32 retained results, so a handle stays reachable after its preview folds.

### MCP tools

`internal/tool/mcpsrc` gives the tools of `Config.MCPServers` to each model call. `turn` declares the one seam: a `Source` returns a `Toolset` of described tools, deferred tools, and a prompt segment. The turn reads it before each model call, so a tool that the model loads in a turn is callable on the next request of that turn. A backend that owns the loop gets every tool described. A backend with `OwnsMCP` gets no MCP tool on a turn with no `AllowedTools`: the runtime asks no server, and the backend gives every server to the external harness. With `AllowedTools`, the allowed MCP tools reach it through the harness-hosted MCP endpoint.

- Connection. The first model call of a session that the runtime serves MCP tools for connects every configured server, in parallel, once for each runtime. `connect_timeout_s` (default 15) bounds each attempt. A server that fails its first attempt retries in the background: the wait before the first retry is 1 s, it doubles for each failed retry, and half of each wait is random. After 3 failed retries the server is parked until `mcp(action="connect")` makes one more attempt. One server has one attempt at a time. A connected server is never dialed again. The wait runs on the clock of the process. A unit test fakes the clock with `testing/synctest`. The e2e runtime host calls `mcpsrc.HoldRetries` before it builds a runtime, so a background retry never ends during a scenario; the hold applies to every `Source` of the test process, and the serve binary has no such hook. `Runtime.Close` closes every connection and stops each stdio server. The classified reason of a failed connect is `initialize timed out`, `initialize cancelled`, `connection refused`, `connection failed`, or `initialize failed`.
- Names. Each tool is `mcp__<server>__<tool>`. With `mcp_servers`, an embedder tool named `mcp`, `list_mcp_resources`, `read_mcp_resource`, or `mcp__…` fails `New`. `AllowedTools` restricts these tools and the deferred list. MCP needs no `WorkDir`.
- Deferral. The mode of a server is its `tool_loading`, else `mcp_tool_loading`, else `eager`. `lazy` defers each tool of the server. `auto` defers when the connected servers have more than `mcp_tool_loading_threshold` tools (default 20). The prompt segment lists the first 200 deferred tools with the first line of their descriptions. The model loads a tool with `mcp(action="select")` and finds one with `mcp(action="search")`. A tool is loaded when the history holds a `select` that names it or a call of it, so a replay or the next owner loads the same tools with no new event kind. A compaction that folds the call defers the tool again.
- Instructions. The segment starts with `<mcp_instructions>`: the instructions of each server that the first connect reached, with its allowed tool names, and one line about the resource tools. It never changes after the first connect. The text of one server is cut at 4000 runes, and the tool names of one server stop before they pass 2048 bytes.
- Resources. When a connected server serves resources, `list_mcp_resources` and `read_mcp_resource` are added. A listing stops at 500 resources for each server.
- Results. A result is text and blobs. An image, an audio item, or a binary resource is a blob of the result; a payload that is not base64, or has no bytes, becomes one line with its type. An error that is not the server's own RPC error names a reason, never the endpoint URL or a response body. An error has no `engine:` prefix, and a call to a tool that is not there reads `no such tool available`.
- Status. The `mcp` tool has a `status` action, with or without deferral. Its result is `{"servers":[{"name","connected","attempts","parked","reason"}]}`, one entry for each configured server in name order, read from the live state. `reason` is absent for a connected server; for any other it is the classified reason of the last failed attempt. A server that connects later adds its tools to the next model call, in the place of its name in the fixed order.
- Down notice. While a configured server is not connected, the model reads `[mcp: unavailable — <clauses>. Tools from these servers are temporarily absent and may return later in this session.]` as a message of its own, in `<harness-engine-context>` tags, and the system prompt does not change. A clause is `<server> (<reason>; retrying)`, or `<server> (<reason>; use the mcp tool action "connect" to retry)` for a parked server, in name order. Each model call of a harness-loop turn builds the text from the live state and pins it: when the text differs from the newest MCP notice, the call sends it after the history that is newest at that call, and each later call sends it at the same place, so each request is a prefix of the next. When the last server connects after a notice, the next model call pins `[mcp: every configured server is connected again.]` once. A compaction moves every pin, the banner and each notice, to the end of the history that it leaves, and a pin moves only then, so the cache breakpoints of an Anthropic request stay on the prefix. No log holds a notice and no reader of the history shows it; a restart pins the live state again at its first model call. The actor owns the pins; a model call pins through `Turn.Pin` on the actor goroutine and the request holds an immutable copy. A backend that owns the loop gets no notice.
- Session servers. `WithSessionMCP` gives one session MCP servers beside `Config.MCPServers`. The keys are server names under the rules of `Config.MCPServers`; a spec that breaks a rule, or a name that a runtime server has, fails `Create` and `Open` with `invalid_request`. So does an embedder tool or a started plugin tool whose name is `mcp`, `list_mcp_resources`, `read_mcp_resource`, or starts with `mcp__`, when the session has session servers. The runtime builds one registry for the session at `Create` or `Open`. It connects lazily at the first model call, with the connection, retry, parking, status, and down-notice rules above, and it holds the runtime servers and the session servers in one `mcp` tool. `Release`, `End`, and `Runtime.Close` stop every stdio server of the session, and each waits for them. Nothing persists: the specs, and the secrets in their `env` and `headers`, are in no event and in no log line, and a log line names a server and nothing else. An `Open` that passes no option has no session server, and a call to one of its tools reads `no such tool available`. An `Open` of a session that the runtime already runs keeps its servers. A backend with `OwnsMCP` gets the session servers with the runtime servers.
- The segment follows the process status line.

### plugins

`internal/tool/pluginsrc` connects `Config.Plugins` and `Options.Plugins` to each session.

- Order. Each hook chain runs the plugins in this order: the plugins of `Config.Plugins` (project plugins, then user plugins), then the in-process plugins of `Options.Plugins` in list order. The most trusted plugins run last, so a user guard sees the final arguments after each project rewrite, and an embedder guard sees them after the user plugins.
- In-process mode. `harness.New` takes `*plugin.Plugin` values in `Options.Plugins`. The host serves each value in this process over an in-memory pipe that runs `plugin.ServeConn`, through the dial seam of a process plugin, so the chain, the deadline, the failure rules, and `deny_on_failure` are those of a process plugin. An in-process plugin has no probe and no cache entry: the host reads its manifest from the value. The pipe and its goroutine start on the first hook or tool call. `New` fails on a nil plugin, an empty name, a `ToolExecuteBefore` with no `Run`, an invalid `HookTimeouts` value, a plugin tool with no name or no `Execute`, a name that repeats the name of a config plugin or of another in-process plugin, and a plugin tool that takes the name of a built-in tool, an `Options.Tools` tool, or another in-process plugin tool; a clash with a tool of a config plugin fails the first `Create` or `Open`, which probes the config plugins. `Runtime.Close` closes each pipe and waits for the serve loops and for the hook and tool calls in flight, bounded by its context: a call that ignores its context holds `Close` until the context of `Close` ends. A process plugin does not wait: `plugin.Serve` returns when its input ends, and the process exit ends the calls. `ProbePlugins` probes config plugins only, and the plugin inventory lists both kinds.
- Start. The first `Create` or `Open` of a runtime reads the manifest of each plugin: from the manifest cache when the plugin has not changed since its last probe, else with one probe, bounded at 30 s, which it stores in the cache. The cache is the file `plugin_cache` (see config). An entry is keyed by the plugin name and a digest of its config, environment, and directory, and holds the size, mtime, and content hash of the executable and of each script argument that is a regular file; a matching size and mtime trust the entry with no read, and only a change of content probes again. A cache file that does not decode, or that carries another `version` than the runtime writes, counts as empty, and a cache that cannot be read or written fails the call. With no `plugin_cache`, the runtime probes at every start. A probe that fails, a manifest whose name is not the config name, or a plugin tool with the name of another tool or of a built-in tool of the model backend fails that call, and the next call tries again. The probe is work that `Runtime.Close` waits for, and a call after `Close` returns `ErrDraining`. A plugin process starts on its first hook or tool call and stays warm until `Runtime.Close`.
- Tools. Each manifest tool runs through `tool/execute`. `AllowedTools` restricts them. The text parts of the output, joined by a newline, are the result text. A `blob` part with inline data is a result blob, which the model reads after the text. A `blob` part with only a URL is the text line `blob: <url> (<media_type>)`. Any other part type fails the call. A plugin needs no `WorkDir`.
- Hooks. A `turn.Toolset` carries `turn.Hooks`, and `runTool` runs each tool call between `Before` and `After`. This covers every tool: embedder, process, MCP, and plugin tools, and each call that a backend that owns the loop makes through `Request.Call`. A call that an `OwnsMCP` backend makes to a server that it connects itself runs no hook, and neither does a client call that a plugin makes (`client/mcp.call`, `client/generate`). `tool.execute.before` rewrites the arguments or denies the call; a denied call does not run, and its deny is the error result. `tool.execute.after` rewrites the result text. The log records the call as the model made it. Each synchronous hook call has a deadline: the `hook_timeouts` value of the plugin manifest for that hook, from 1 to 600000 ms, or 5 s when the manifest has no entry. The manifest must list the hook in `hooks`, and `event` has no entry. A violation in a probed manifest fails the first `Create` or `Open`, and in an in-process plugin it fails `harness.New`. The timer runs while the hook waits on a client call. When the dispatch of that hook starts the plugin, the `initialize` call has the longer of that deadline and 5 s, and the deadline bounds the hook call alone. Only `initialize` has that bound: the spawn or dial does not end with the context. The host compares `hook_timeouts` with the live manifest at spawn, like the other fields.
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

A backend that owns the loop ignores `Instructions` and builds its own prompt. Claude Code gets `append_system_prompt` as one `--append-system-prompt` value, and the CLI runs in `WorkDir`.

Each turn sends the prompt with one process status line after it, built when the turn starts (see processes). Each model call then adds the MCP segment, then the plugin segments. The only ambient messages are the banner and the MCP notice of a server that is down; the system prompt carries neither. With `Options.Version`, each model call of a harness-loop turn sends `[engine: harness <version> · engine started <time>]` in `<harness-engine-context>` tags after the history message that was newest when the first request of the session left. Each request is a prefix of the next, also after a compaction in the middle of a turn, and a compaction moves the place to the end of the history that it leaves. A backend that owns its loop gets no banner.

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

`GET /workspace/changes?scope=&dir=` diffs the git work tree. The route exists only with a `WorkDir`.

- `scope` is `branch` (default), against the merge base with the first of `origin/HEAD`, `origin/main`, and `origin/master` that resolves, or `uncommitted`, against `HEAD`. An unborn `HEAD` diffs against the empty tree.
- `dir` defaults to the `WorkDir`. Any other `dir`, relative to the `WorkDir`, and its repository must stay under the `WorkDir` after symlinks.
- Untracked files count as added, through a private copy of the index and a private object directory. The request never writes the real index or object store, never runs a filter driver, hook, external diff, or textconv, and ignores the `GIT_*` variables that select another repository.
- `files` is complete. An untracked file above 2 MiB is `large` with no hunk. `patch` ends at the last whole file within 1 MiB and sets `truncated`. The patch is not HTML-escaped.
- `engine` names the version-control engine of the work tree (`git`). `branch` is the short name of the checked-out branch. `head_rev` is the `commit` revision of the head, and is absent for an unborn head. When `scope` is `branch`, `base.rev` is the `commit` revision of the merge base. `head` and `base.sha` are the commit IDs of `head_rev` and `base.rev`; both are deprecated, and a client reads `head_rev` and `base.rev`.
- A `Rev` is a revision or a change of a work tree. Its text form is `<kind>:<id>`, and its JSON form is that string. `kind` is `branch` (a git branch or a jj bookmark), `tag`, `commit`, `change` (a jj change), or `op` (a jj operation). `id` is not empty and may contain `:`, because the text splits at the first one. Only an engine gives an `id` meaning. `protocol.ParseRev` refuses any other text with `protocol.ErrInvalidRev`. The zero `Rev` has the empty text, and a field tagged `omitzero` leaves it out of the JSON.
- One request has a 28 s budget. A request that passes it, or whose file list passes 32 MiB, fails with `too_many_changes`. Errors: `invalid_request`, `not_a_repo`, and `no_base`.

### config

One `Config` struct. `Defaults` is the one defaults table, and each accessor reads it for an unset key. `Validate` is the one rule set. `LoadProject(userPath, dir)` merges `<dir>/.harness.json` over the file at `userPath` and runs it on the merged config. The `plugins` arrays of the two files join: project plugins first, then user plugins. A plugin name in both files fails with an error that names the plugin and both files. `Parse(name, data)` decodes one config document like `Load`, and `MergeProject(user, name, dir)` is `LoadProject` for a user config that the caller holds. `LoadProject` reads no environment variable and no home directory, and `New` runs it on `Options.Config`. It never changes the config. `ProcessSpec.Validate` is the per-entry rule that `process.Declare` also uses.

`cmd/harness` resolves `userPath` as `$HARNESS_CONFIG`, else `~/.harness/config.json`, and logs one line at boot that names the file it loaded and counts its processes, MCP servers, and plugins.

`max_tokens` caps the output tokens of each model call of a turn. 0 is the backend default, and a negative value fails `Validate`. `HARNESS_MAX_TOKENS` sets it.

`request_max_retries` (default 4) and `stream_max_retries` (default 5) bound the retries of a model request and of a model call (see Backend). A nil value is the default, 0 turns the retry off, a value over 100 is 100, and a negative value fails `Validate`.

`Load` rejects a key that `Config` has no field for, with an error that names the key. `session_dir` is for `cmd/harness`; `model_tool` turns the `model` tool off, and `New` reads it.

`plugin_cache` (a path; `HARNESS_PLUGIN_CACHE`) names the plugin manifest cache. Only the user file sets it: a project file cannot. `cmd/harness` sets it to `plugin_cache.json` beside the user config when no variable names it, and the Go API has no default.

`owner_epoch` (a number) and `sync {url, token_file}` are the keys of a box harness, which `cmd/harness` reads and `harness.New` does not; `Validate` requires an `http` or `https` URL with no userinfo, and a token file. Only the user file sets them: a project file cannot. A project file is still read as a file, so an invalid `sync` there fails the load, and a valid one is dropped.

`ApplyEnv` sets each top-level string, number, or bool key from `HARNESS_<KEY>`. An empty variable keeps the key. A map, slice, or struct key has no variable. A parse error names the variable and never the value. There are no env-only knobs. `cmd/harness` applies it in `serve` and `run`, and a flag wins over a variable.

The package imports only the standard library. The `config-leaf` depguard rule enforces it.

## WebAssembly

The root package and the packages it imports build for `GOOS=js GOARCH=wasm`. `cmd/harness-wasm` is the entry of the module: it exports `harnessConnect` and wires `internal/jshost`, which holds the adapters of the host. Both have the build tag `js`, so a build for another target skips them. A JavaScript host runs the module: a browser page, a Worker, or Node with the `wasm_exec.js` of the same Go version. Harness embeds no JavaScript engine. `GOOS=js` is the only wasm target. The Go scheduler runs on the event loop of the host, so `time` and timers work with no extra interface.

Every capability that the host supplies is a field of `harness.Options`. The module adds no seam.

| Seam | Declared in | On `GOOS=js` |
| --- | --- | --- |
| Model transport | `Options.ModelTransport` | The host sets it. The default `net/http` transport on js cannot cancel a response body, and under Node it is off. |
| Store | `harness.Store` | `store.DiskStore` on the `fs` object of the host, or `store.MemStore`. |
| Owner | `Options.Owner` | `NewLocalOwner`. `flock` does not exist on js, so the store does not fence two instances (see Store). The host gives one writer to each store. |
| Tools | `Options.Tools`, `CreateSession.AllowedTools` | The built-in file and search tools run on the `fs` object. `bash` and `process` fail with `exec: executable file not found`. A host hides them with `AllowedTools` and adds its own tools in `Options.Tools`. The wasm host does not call `Runtime.WorkspaceChanges`, which runs `git`. |
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
| `proxy` | `{headers, baseURL}`. `headers` are added to a request that does not set them and that goes to the scheme and host of `baseURL`; without `baseURL` no request gets them, so a credential never reaches another origin. `baseURL` sends all model traffic to the proxy: it is the `base_url` of the native `anthropic` and `openai` providers when the config has no entry for them, and of each `providers` entry that sets none, except a Claude Code CLI entry. |
| `onFrame`, `onLog` | `onFrame(line)` receives each ACP message of the agent. `onLog(line)` receives each JSON log line. |

Every option that is present must have its type. A bad option does not throw: the first error fails the first `session/new` as an invalid request. The module advertises no MCP transport and refuses `mcpServers`, because it starts no MCP server.

The frame channel is two in-memory pipes between the host and `acp.Serve`. `send(frame)` writes `frame` and a newline to an unbounded queue that a goroutine drains into the input pipe, so it never blocks the event loop. A goroutine reads the output pipe by line and calls `onFrame`. `close()` ends the input, and its promise settles when `acp.Serve` has released the sessions and `onFrame` has received the last frame. `send` after the end is ignored.

The model transport is `FetchRT` of `internal/jshost`, a `RoundTripper` over the global `fetch`. It aborts the fetch when the request context ends, also after the response headers, and each `js.Func` that it creates releases itself after it fires. A fetch that gets no response returns a `*FetchError` with the method, the origin, the name and message of the JavaScript error, and `navigator.onLine` at the failure. The error text is `fetch <method> <origin>: <name>: <message>` and a statement that the browser blocked the request or the host is unreachable. It holds no query, header, or body, and never an object marker: a value that is not an Error gives its text for a string, a number, or a boolean, and a fixed text that names its type for any other value. `FetchError.Permanent()` is true while `navigator.onLine` is true, so `provider.Do` does not retry and the turn ends `permanent` (see Turn). A host that reports offline, or reports nothing, keeps both retry layers. A failure after the response headers is an ordinary read error.

The Codex websocket transport cannot dial on js, because a browser `WebSocket` cannot set request headers. The build tag of `ws_dial.go` and `ws_dial_js.go` in `internal/provider/openai` separates the dial. On js the dial fails, and the request goes over HTTP.

### Size budget

`go run ./internal/wasmsize`, from the repository root, builds `cmd/harness-wasm` with `-trimpath -ldflags="-s -w"` and measures the module raw and with `gzip -9`. It fails when either size is above its number in `testdata/wasm-size-budget.txt`. Each number is the measured size plus 3%, rounded up to 50,000 bytes. A change that grows the module raises the number in the same diff, and its PR body gives the reason.

## Tests and guardrails

### Contract suite

- `harnesstest` is a public package. `NewAnthropic`, `NewChat`, and `NewOpenAI` serve scripted Anthropic Messages, OpenAI chat-completions, and Codex Responses (SSE and websocket) endpoints, with errors, usage, overflow, and blocking steps; `RateLimits.Extra` adds Codex rate-limit windows by limit name. See its godoc.
- A test double that imitates a wire is checked against a real recording in `testdata/wire` (see the Wire doubles gate). `internal/wirerecord` makes the recordings and runs only with `HARNESS_RECORD_WIRE=1`; `internal/wirescrub` removes ids, tokens, emails, and paths first.
- A scenario is a model script and a list of actions. Its golden holds the normalized model requests and the final transcript of each session, not event shapes. An action that returns a result records its status and body under `calls`, keyed `<action>.<alias>`, with `#2`, `#3` on a repeat. Ids in a body become `ses:<alias>`, `msg#N`, and so on, and a scenario binds each child with `bindChild` before it lists sessions, so no id gets a run-dependent number. `scenario.config` adds top-level keys to the served config, and `scenario.chat` serves the model through `NewChat` as provider `bifrost`.
- Scenarios run the real `harness serve` binary over loopback, in its own process group, through a driver interface. Each wait has a bound. Every scenario checks the invariants of the log: unique message ids, one result for each tool call, and contiguous event sequence numbers.
- The ACP table (`e2e/acp_scenarios_test.go`) is separate from the driver rows. A row is a list of steps, run on the real `harness acp` binary over stdio and on `acp.Serve` over pipes in process. Both hosts match one golden in `e2e/testdata/acp`. The golden holds the normalized model requests, the final transcript of each session (read from the store after the connection closes, not through ACP), and the frames in order, because the frames are the ACP contract. Consecutive chunks of one message are joined in the frames. The test client checks each frame in both directions against the pinned ACP schema. Each row cites a sentence of the ACP section, and `TestACPRows` checks that the sentence is there.
- The ACP TCK job (`.github/workflows/acp-tck.yml`) runs the third-party `acp-tck` v1 suite, pinned to one commit, against the built `harness acp`. `go run ./internal/acptck` serves the scripted model of `harnesstest` on loopback, writes a config that points the `anthropic` provider at it, and starts the TCK, which launches one `harness acp` process for each test. A prompt that holds the cancel text keeps its turn open until the client cancels. The command prints the failing requirements of each tier and exits non-zero when a MANDATORY or CAPABILITY requirement fails (or a MANDATORY one is untested) and `testdata/acp-tck-allow.txt` does not list it, when that file lists a requirement that passes, or when the scripted model gets a request that no step matches. Each line of the file is a requirement id and a reason. The job runs on `main`, nightly, and on a pull request that changes `internal/acp`, `cmd/harness/acp*`, `internal/acptck`, `testdata/acp-tck-allow.txt`, or the workflow file. It does not fail the workflow run.
- The Node wasm host runs the ACP table on the module under Node 24, with `HARNESS_E2E_NODE=1`: `cmd/harness-wasm/node/host.mjs` speaks ACP on stdio, over a `DiskStore` in the temporary session directory, which one process holds through `.embed.lock`. A row that carries `needsExec`, `needsMCP`, or `schedulerOrder` does not run there. The other rows match the golden of the native hosts, except that the initialize answer advertises no MCP and the js file system writes the ENOENT text with a capital letter, which the host rewrites before the compare. With the variable set, a missing `node` fails the test. The Node job of CI sets it. The Node job also runs `node --test sdk/embed/test/*.test.mjs`: the tests start the module in a Worker and drive a tool turn, the page fetch bridge, `proxy.baseURL`, and the store directory through `createHarness`.
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
| Structure | `internal/gates` parses the Go files and fails on: a public package that the Public packages table does not list (a dated allow list holds the exceptions, and an entry that stops being a violation fails); and each concept that the spec gives one resolver or one writer: the context window, `Store.Append`, `Store.PutBlob`, `State.Apply`, the error body, the route mounts, and `message.EngineContext`; and a comparison with a provider or backend name (rule 3) outside `config`, the router, the provider wires, and the `modelmeta` tables, where a dated allow list holds the exceptions and an entry that stops being a violation fails; and a reference to a removed package path (`engine/`, `server/`, `mcpserver`, `provider/claudecode`, `harness-migrate`, an `engine.Name` symbol, or the module path of a package that lives under `internal/`) in any Go file, code or comment. `internal/gates/structure.go` is the one list of owners, by function |
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
- The seams that boxes uses: `OpenView` (read-only open), `harness/harnesstest` (public scripted model), `Sync` and `protocol.SyncBatch` (the pump contract), `harness/config` (a config without server dependencies), `protocol.Session.HeadSeq` (durable head per session), the `crashed` cause (open after a forced stop), `Models()` (no sessions needed; `New` does no I/O), `Ownership.Epoch` with `ErrStaleEpoch` (an external lease that fences a stale epoch), `ReadEvents` (events after a cursor, with no replay), and `ReadHandler(store, opts)` (the read routes of a box that is not running, with queued commands).
- `Runtime.Close` and `Session.Release` return only after `Sync` acknowledges every record through the handoff, and `View` reports `SyncedSeq`. `Sync.Deliver` returns `SyncAck{head}` on every reply. The home chat is one per person.
