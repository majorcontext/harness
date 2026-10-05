# Harness architecture

The re-architecture of harness, as built and as planned: a session is an append-only event log, one goroutine owns each session, and every seam is a small interface owned by its consumer.

Phases 1 to 4 are built, except the quiesced cutover with boxes (see Migration). `cmd/harness` runs `serve`, `run`, `sessions`, and `plugin probe` on `harness.Runtime`, and imports neither `engine` nor `server`. `engine` and `server` stay in the tree until phase 6, and nothing in `cmd` calls them. A statement that names a later phase describes planned work.

## Problem

Harness grew by accretion. Each fix added a field, a record kind, a route, or a repair site. Measured at `be8bf57`:

| Symptom | Evidence |
| --- | --- |
| God object | `engine.Session`: ~92 fields, one `sync.Mutex`, 182 `mu.Lock` calls in `engine/` |
| Three "sessions" | `engine.Session`, `engine.sessionNode`, `server.sessionState` each hold status |
| Four status vocabularies | `SessionStatus`, wire `idle/busy/goal-running`, `session.status` event, `compositeState` |
| Six folds of one log | `LoadSession`, snapshot, index, `readSessionInfo`, `messagepage`, server goal fold |
| Two journals | `<id>.jsonl` and box-global `events.jsonl`; boot reconciles them by full load |
| Memory, then disk | `durableDebt`, `deferPersist`, `deferredQueueRecords` patch the write order |
| I/O under locks | `writeRecord`, fsync, and `events.jsonl` writes run under `s.mu` or `Server.mu` |
| Policy in transport | Goal re-arm, run slot, queue dispatch, pause state live in `server/handlers.go` |
| Backend by name | `"claude-code"` checks in engine dispatch, `modelmeta`, billing, compaction |
| Three tool paths | Built-in, MCP, and plugin dispatch differ in shape, concurrency, and errors |
| Seven read models | `/message`, `/journal`, `/event`, `stream_from`, transcript, console-bootstrap, boxes mirror |
| Comment bloat | 44% of all source lines are `//`; `engine` is 51%; 150+ history comments |
| Tests on internals | 127k test lines vs 70k source; ~70% assert unexported state |

## Goals and non-goals

Goals:

- One owner and one source of truth for each piece of state.
- Every public interface is designed from what its consumer needs. Nothing is kept because it exists.
- A public Go API small enough to read in one sitting: `harness`, `harness/config`, `harness/protocol`, `harness/storetest`, `harness/harnesstest`. `harness/migrate` is public only until phase 6 deletes it.
- One HTTP contract, built from the same Go types as the Go API.
- Pluggable storage and ownership, so a session survives process loss.
- Delivery as PRs on `main`. Each PR ships alone.

Non-goals:

- Backward compatibility of any kind: routes, on-disk formats, config, Go API. We control every consumer and change it in the same release.
- Permission systems, plan mode, a JS runtime, a web UI.
- Multi-writer sessions. One owner writes each session log.
- Durable token deltas. Deltas stay ephemeral.

## Four rules

1. A session is an append-only event log. One `Apply` function builds state from it, live and on replay.
2. One goroutine owns each session's state. Other code sends it commands and reads published, immutable views.
3. Every seam is a small interface declared by the package that uses it. Nothing branches on a backend or provider name.
4. Each interface is designed from its consumer's need. No shape, name, or field survives only because it exists today.

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

## Architecture

Three consumers drive one `harness.Runtime`: boxes over HTTP and SSE, the meta home chat through the Go API, and the CLI. The runtime writes through `harness.Store` and runs a session only while `harness.Owner` grants it.

```
  boxes control plane     meta home chat (in-process)     harness CLI
         │ HTTP + SSE            │ Go API                     │
         ▼                       ▼                            ▼
  ┌─────────────────────────── harness.Runtime ───────────────────────────┐
  │ server    http.Handler over the runtime                               │
  │ session   one goroutine per session; runs only while it owns it       │
  │ turn      agent loop → Backend (capabilities) · []Tool · Source       │
  └──────────────┬─────────────────────────────────────┬──────────────────┘
                 │ Append(id, expectedSeq, records…)   │ Acquire(id) → Ownership
           harness.Store                          harness.Owner
   DiskStore · MemStore · Postgres (boxes)    local · lease (boxes)
```

The session layer is the one owner of state. The store write is the fence: a stale owner's append fails with `ErrConflict`.

### Public packages

These are the compatibility surface.

| Package | Owns | Consumer |
| --- | --- | --- |
| `harness` | `Runtime`, `Options`, `Session`, `OpenView`, `View`, `Store`, `Owner`, `Ownership`, `Sync`, `ApplySync`, `Tool`, `DiskStore`, `MemStore`, sentinel errors | meta home chat, boxes control plane, CLI |
| `harness/config` | `Config`, `Defaults`, `Validate`, `ApplyEnv`; imports only the standard library | boxinit `BootConfig` |
| `harness/protocol` | Data types shared by the Go API and HTTP, including `SyncBatch`; source of the generated OpenAPI and TS | boxes server, web, boxctl |
| `harness/storetest` | Conformance suite for a `Store` | boxes `pgstore` |
| `harness/harnesstest` | Scripted model server for contract suites | harness and boxes contract suites |
| `harness/migrate` | The one-time conversion of engine journals, and box archives; phase 6 deletes it | boxes cutover |

The Go API:

```go
package harness

func New(opts Options) (*Runtime, error) // no I/O; sessions load on Create, Open, or List

type Options struct {
	Store  Store         // required
	Owner  Owner         // nil: the local process owns every session
	Sync   Sync          // nil: no replication, unless the config key sync is set
	Config config.Config // New checks it with Validate
	// ModelTransport returns the HTTP transport for a model provider.
	// nil, or a nil result: the default transport.
	ModelTransport func(provider string) http.RoundTripper
	Tools          []Tool // embedder tools, beside the built-in, process, task, MCP, and plugin tools
	// WorkDir is the directory of a coding agent. Each session gets the
	// built-in tools, bash among them, so it grants command execution.
	// Empty: no file is read, no process runs, no built-in tool exists,
	// and the system prompt is append_system_prompt alone.
	WorkDir string
	// Version is the build version that the engine banner names. Empty: no banner.
	Version string
	// AskUserQuestion lets a backend that owns its loop ask the user a question,
	// which the embedder answers with Session.Resolve.
	AskUserQuestion bool
	// ServeURL and RunToken go to each plugin in its initialize call. A token needs a URL.
	ServeURL, RunToken string
	// MaxTokens caps the response of each model call of a turn. 0: the backend default.
	MaxTokens int
}

// A Runtime hosts many sessions. Each runs only while its Ownership holds.
func (r *Runtime) Handler() http.Handler
func (r *Runtime) Create(ctx context.Context, req protocol.CreateSession) (*Session, error)
func (r *Runtime) Open(ctx context.Context, id string) (*Session, error) // acquire, fence, replay, resume
func (r *Runtime) End(ctx context.Context, id string) error // end and unload; session_busy while a turn or a control command runs; the log stays
func (r *Runtime) List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error) // creation order
func (r *Runtime) Models() []protocol.Model
func (r *Runtime) Commands() (protocol.Commands, error) // the slash-command menu
// Close hands off every session, then returns once Sync has acknowledged
// every record through each handoff, or, when ctx ends first, once the
// turns that it cancels have ended. A session that Sync rejected for good
// does not fail Close; SyncStopped reports it.
func (r *Runtime) Close(ctx context.Context) error

// CatchUp replicates every stored session through Sync, opening none. A box harness calls it at start.
func (r *Runtime) CatchUp(ctx context.Context) error
// SyncStopped reports whether a Sync rejected a batch for good since New. Sync may then lack records of a session that the runtime ran.
func (r *Runtime) SyncStopped() bool
// ProbePlugins reads the manifest of each configured plugin, as the first Create or Open does, and returns each plugin with its tools and hooks.
func (r *Runtime) ProbePlugins(ctx context.Context) ([]protocol.Plugin, error)

func (s *Session) View() protocol.Session // includes HeadSeq and SyncedSeq
func (s *Session) Submit(ctx context.Context, in protocol.Input) (protocol.Admitted, error) // Admitted.Repeat: the input was admitted before
func (s *Session) Interrupt(ctx context.Context, req protocol.Interrupt) error
func (s *Session) Resolve(ctx context.Context, requestID string, res protocol.Resolution) (protocol.Resolved, error)
func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)
func (s *Session) SetGoal(ctx context.Context, g protocol.Goal) error
func (s *Session) ClearGoal(ctx context.Context) error
func (s *Session) Compact(ctx context.Context, req protocol.Compact) (protocol.Compacted, error)
func (s *Session) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
func (s *Session) Release(ctx context.Context) error // hand off, flush Sync, release ownership

// OpenView reads a session from any Store without owning it: no Acquire, no appends.
func OpenView(ctx context.Context, st Store, id string) (*View, error)
func (v *View) Session() protocol.Session
func (v *View) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
func (v *View) Messages(ctx context.Context, before uint64, limit int) (protocol.MessagePage, error)
// Resumable reports whether Open would resume something: a running or suspended turn, a queued input, an active or paused goal, a command that no owner finished, or a child that has not settled.
func (v *View) Resumable() bool

// Sync replicates each session's records elsewhere, in seq order.
type Sync interface {
	// Deliver returns the receiver's head on success and on a seq mismatch;
	// the sender resends from Head+1. ErrStaleEpoch, ErrConflict, or
	// ErrSyncRejected stops the session and releases its Ownership.
	Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error)
}

// ApplySync appends b to st by the receiver rules of a Sync (see Events).
func ApplySync(ctx context.Context, st Store, b protocol.SyncBatch) (protocol.SyncAck, error)

type Tool interface {
	Spec() protocol.ToolSpec
	// Run receives call.ID, stable across a resumed turn, for idempotent effects.
	Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error)
}
```

The HTTP handler is a thin mapping of these methods. An embedder and a remote client see the same types and the same errors.

### Internal packages

Free to change.

| Package | Owns |
| --- | --- |
| `internal/server` | HTTP mapping of the Go API |
| `internal/eventlog` | Event schema, record codec, `Apply`, and `Check` |
| `internal/session` | Session actor, mailbox, state machines, views, replication, and the child commands |
| `internal/tree` | The child tree: limits, spawn, report, recovery, the tree interrupt, and the `task` tool. It reads the runtime through its own `Sessions` interface |
| `internal/turn` | One agent loop; declares `Backend`, `Tool`, and `Source`; `Restrict` |
| `internal/backend` | The `turn.Backend` of a runtime: it builds a backend for each provider of the registry (the native `anthropic` and `openai`, the default `openrouter`, and each configured entry) and routes each model ref to one. In the new runtime, only this package and the provider packages import a provider wire |
| `internal/backend/modelapi` | The one model API backend, for every provider wire |
| `internal/backend/external`, `claudecode` | Third-party harness backends; phase 5 adds `codexcli` |
| `internal/tool/mcpsrc` | A `turn.Source` that gives MCP tools to each model call |
| `internal/tool/pluginsrc` | A `turn.Source` and `turn.Hooks` that give the plugin tools, hooks, and events to each session |
| `internal/tool/proc` | The `process` tool, the process manager of a runtime, and the process status line |
| `internal/tool/builtin` | The file, search, and shell tools of a coding agent |
| `internal/toolresult` | Large-result retention and `read_tool_result`, at parity with the engine |
| `internal/admit` | Checks each part of an input and returns the `input.admitted` record with the blobs of its attachments |
| `internal/prompt` | System-prompt segments and agent profiles |
| `internal/workspace` | The git diff of the work tree for `GET /workspace/changes` |

The `depguard` rules of `.golangci.yml` freeze the graph: `eventlog` imports `protocol` only, `turn` imports `eventlog` and `protocol`, no package of `internal/backend` or `internal/tool` imports `session`, `tree`, or `server`, no internal package imports `engine`, and, in the new runtime, only `internal/backend` builds a provider wire; `engine`, `server`, `cmd`, and `harnesstest` still import the wires until phase 6.

Phase 6 moves the leaf packages to `internal/`: `message` (conversation types), `modelmeta` (context-window table from models.dev; exposed only through `Runtime.Models` and `GET /models`), and `mcp`, `plugin`, `skill`, `command`, and `process`, as is.

`internal/workspace` serves `GET /workspace/changes`. It shells out to git and cannot reach the runtime or any session. Harness is the only HTTP server in a box, so box-level reads live here, isolated. See "workspace".

Phase 6 deletes `engine`, `server`, `provider/claudecode`, `mcpserver` (merged into `internal/mcp`), and `imageclamp` and `typeid` (merged into their one consumer). It also deletes the config keys that `New` refuses (the phase 4 switch stops reading them), their `Defaults` entries, and `harnesstest.SinkReceiver`, and splits `config/config.go` into files of at most 800 lines.

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

- `seq` starts at 1 and has no gaps. `Head` returns the last seq, or 0 for an empty session. `expectedSeq` is the last seq the writer has seen, so it equals the record count.
- A stale writer gets `ErrConflict` and stops. This holds across `Store` instances on one storage, in one process or in many: the fence is in the storage, not in an instance.
- Any other append error also stops the session actor, with no retry. The next `Open` fences and replays from the store, so an append that landed is in the replay, and one that did not land is not.
- Compare-and-append alone does not fence a lease handoff: an old owner's in-flight append can still hold the current `expectedSeq`. The new owner therefore appends `owner.acquired` before it replays or runs (see Ownership). Any later append from the old owner conflicts. A shared store may also check its lease in the same transaction as the append.
- `Append` is durable on return. `DiskStore` writes the records of one `Append` with one `fsync`. `MemStore` is for tests.
- `DiskStore` takes an exclusive `flock` of the session log for each append, from its first byte to its `fsync` or rollback, and compares `expectedSeq` with the head read under that lock. Each `Head` takes a shared `flock`, so it never reads bytes that a failed append cuts again, and scans again the bytes that another instance appended since the last scan, which a change of the file size shows. A wait for a lock ends with the context of the call. Where `flock` does not exist, only the instance fences its appends.
- There are no checkpoints. `Open` and `OpenView` replay the whole log. `List` reads the view of a session that this runtime runs, and replays the log of any other session. Add a checkpoint only when a measurement shows that replay costs too much.
- `storetest.Run(t, newStore)` is the conformance suite. Every `Store` runs it. `storetest.RunInstances(t, newStorage)` checks the fence across instances: for each case, `newStorage` returns an opener of new storage, and each call of the opener returns one more instance over that storage. A `Store` that is fenced against another process runs it too.

### Layout on disk

```
<root>/<session>/log.jsonl          source of truth; line N holds seq N
<root>/<session>/blobs/<key>        backend state; retained tool results from phase 3; prompt attachments
```

The box-global `events.jsonl`, `<id>.index.json`, and `<id>.snap` are deleted.

### Event envelope

```json
{"v":1,"seq":42,"t":"2026-10-02T12:00:00Z","k":"turn.ended","d":{}}
```

`Event` is a Go interface with `Kind() string`. One registry maps a kind to its type for decode. An unknown kind fails the load loudly.

### Event kinds

| Kind | Payload |
| --- | --- |
| `session.created` | `parent_id?`, `agent?`, `model`, `settings`, `origin`, `allowed_tools?` |
| `owner.acquired` | `epoch`, `owner` |
| `settings.changed` | `model?`, `effort?`, `service_tier?` |
| `input.admitted` | `input_id`, `delivery` (`queue` or `steer`), `source`, `parts` |
| `input.promoted` | `input_id`, `turn_id` |
| `input.withdrawn` | `input_id` |
| `turn.started` | `turn_id`, `input_ids` |
| `item.completed` | `item_id`, `turn_id`, `message` (user, assistant, tool result) |
| `turn.suspended` | `turn_id`, `cause` |
| `turn.resumed` | `turn_id`, `count` |
| `turn.ended` | `turn_id`, `stop_reason`, `cause?`, `error?`, `error_class?`, `recover_hint?`, `error_detail?` |
| `request.opened` | `request_id`, `item_id`, `kind`, `payload` |
| `request.resolved` | `request_id`, `resolution` (`answered` or `dismissed`), `answer?` |
| `goal.set` | `condition`, `max_turns`, `turns?` |
| `goal.evaluated` | `turn_id`, `verdict`, `guidance?` |
| `goal.changed` | `state`, `reason?`, `retry_at?` |
| `compaction.applied` | `from_seq`, `to_seq`, `summary`, `by_backend`, `usage?` |
| `child.spawned` | `child_id`, `agent?` |
| `child.settled` | `child_id`, `outcome` (`done`, `failed`, `canceled`), `result_ref` |
| `command.recorded` | `input_id`, `line`, `name`, `args?`, `status`, `text?`, `result?`, `result_truncated?` |
| `context.measured` | `tokens`, `window`, `source`, `usage?`, `subscription_usage?`, `cost_usd?` |
| `backend.state` | `backend`, `blob_key` |
| `tool_result.retained` | `handle`, `tool`, `blob_key`, `bytes`, `lines`, `head` |

Ephemeral frames go to subscribers and never to the store: `item.started`, `item.delta`, `status`. Each has `ephemeral: true` and the last durable seq, and an item frame carries its `item_id`, so a client can place it.

### Apply

```go
func (s *State) Apply(r Record) error
```

`Apply` is the only code that changes durable state. Live code appends, then applies the same record. Replay applies the whole log from seq 1. `State.History` holds the summary of the newest compaction and the messages after it, so a reader sees the history from the newest compaction on. `State.ModelHistory` is `State.History` with each pinned segment (see Settle) at its place, and the model reads it. There is no other fold. The summary for `GET /sessions` is `State.Summary()`.

Usage is part of the fold. Each model call that measures anything appends one `context.measured` record with its `usage`, its context reading, and the `subscription_usage` that the provider reported with it. A summary call adds its `usage` to the `compaction.applied` record that it produces. A summary that fails appends a `context.measured` record with its `usage` alone. A goal evaluator call adds none, as in the engine. `State.Usage()` sums both, so a handoff, a crash, and a restart keep it. A record with a `source` sets the context reading, zero included, and a record with no `source` leaves it; a model API backend gives a `source` only to a call with prompt tokens. The newest `subscription_usage` is the one that the view shows. A backend that reports usage once for a turn, as Claude Code does in its `result` frame, records it once, with the subscription snapshot that its `rate_limit_event` frame reported. The actor stamps `captured_at` of a snapshot that has none. The `total_cost_usd` of that `result` frame is `cost_usd` of the record. `State` sums `cost_usd` over the log, and the view shows the sum as `session_cost_usd` of `subscription_usage`, with provider `claude`, no windows, and the time of the view as `captured_at` when no `rate_limit_event` frame came. A session with no `cost_usd` record shows no sum.

### Invariants enforced at append

`eventlog.Check` applies a batch to a copy of the `State` before the actor appends it. A batch that breaks an invariant fails with `ErrIllegal` and is not appended.

- Every tool call item gets exactly one result item, or an open request, before its turn ends. When a turn ends with a tool call still open (completed, failed, or `provider_exhausted`), or a handoff suspends it with a call still open, the runtime gives that call a synthetic result that says the call did not finish, as Codex and Claude Code do. A stopped, goal-cleared, or crashed turn gives its open calls the results that its row names (see "State machines"). The result is an addition: the log never loses or changes a real tool result.
- `seq` is gap-free per session.
- A `turn.ended` follows every `turn.started`, except for a suspended turn.
- Only a suspended turn resumes. A turn that ended never resumes.

### Old-format migration

The runtime reads no old format: not the current journal, index, snapshot, or `events.jsonl`. No session history is lost at the cutover.

A one-time Go migration tool converts each old session journal into `harness.Store` records through `eventlog`. Its inputs are the per-session journal files of the engine on each box disk and in each box archive. It does not read the `box_journal_*` mirror tables of boxes: they hold only the filtered record types that boxes mirrors, and a box clears them at its first batch, so they cannot rebuild a conversation. The tool also copies the retained tool-result files of each session into `Store` blobs, so `read_tool_result` reads a converted handle. The tool runs in the quiesced window of the cutover, before the new harness starts. Each converted session opens with its full conversation, so the agent keeps its context and the console keeps its transcript.

- The tool also converts each archived box in the cutover window. It reads the saved session journals from the `sessions.tar.zst` export in the archive object of the box, and writes the converted event logs back into that archive. A later restore needs no converter. The cutover then loads the converted logs of each archive into `pgstore` through the receiver of `Sync` (`ApplySync`): one conversion path, and no converter in the control plane.
- The tool verifies each session: it replays the new log with `Apply`, and each message of the history matches the old transcript.
- The tool reports each session that fails conversion. It never silently gives that session an empty history.
- `cmd/harness-migrate` runs `migrate.Dir` on a session directory or `migrate.Archive` on an archive. Box disks and archives are the only sources. `pgstore` gets the converted sessions of a live box through `Sync` of the log that the disk conversion wrote, and those of an archived box through the same receiver, so one session has one log. The engine loader reads each journal, so the converted log replays to the transcript that the engine shows.
- Each user message starts a turn, unless a tool call of the running turn has no result yet. The newest compaction summary becomes `compaction.applied`. The messages that a compaction folded are not in the log, as no old reader shows them; the old journal keeps them. The log keeps the model, settings, an active goal (which judges no turn until the next input), queued prompts, slash commands as `command.recorded` after the message that they followed, children with `parent_id`, each child report that reached its parent as `child.settled`, the outcome of the last turn of a child, the Claude Code CLI session as `backend.state`, and each retained tool-result file as a blob with `tool_result.retained`, and the session cost of a Claude Code session as `cost_usd` of a `context.measured` record.
- A child turn that was running at the cutover ends as engine recovery reports it: `done` when its last message is an answer with no tool call, and otherwise `failed` as lost to restart. The parent settles the child when it opens.
- These old records are not converted: `mcp.tools_selected` (the runtime derives MCP selection from the history), the header `workdir` (the runtime has `Options.WorkDir`), `parent_session` fork lineage, `task_depth` (the runtime derives depth from `parent_id`), the turn count of a goal (the engine also counted from 0 after a restart), `claude_code.history_watermark`, `claude_code.question` (a question that waited for an answer ends as a tool call with the result that the engine adds), and the trace-only records `claude_code.compact`, `goal.eval`, `goal.eval_failed`, `goal.stalled`, and `goal.parked`. A journal that names no model gets the `-model` of the tool, as the engine gives it the model of its server.
- An archive keeps each entry and its header unchanged, and each added entry gets the owner of the session directory. The archive object has a `manifest.json` with the size and MD5 of the archive, so the cutover writes the size and MD5 that the tool prints into it.
- The tool keeps each image or file attachment of the history and of the queued prompts: no history is lost, and the engine accepted png, jpeg, gif, webp, and pdf attachments, which the runtime keeps at parity. The log holds an attachment as a blob part (see "Inputs"): the tool stores the bytes with `Store.PutBlob` under `attachment-<sha256>` before the one append, and equal bytes share one key. A blob inside a tool result becomes a text note, because the log's tool result holds text. An engine-context part is request-only and is dropped. A tool call with no result gets the result that the engine adds before each request.
- The tool writes each session in one append, so a session that fails has no log, and a second run skips each session that the store holds.
- The tool is the only code that reads an old format. Phase 6 deletes it after the cutover, so the runtime never carries an old-format reader. Nothing reads an old format again.

## session

### Actor

Each live session is one goroutine. It holds the `Ownership` and the `State`. Commands arrive through a mailbox as functions that run on the actor goroutine. The caller waits for the reply.

| Command | Effect |
| --- | --- |
| `Submit(input)` | Append `input.admitted`; start a turn, queue, or steer |
| `Interrupt(turnID?)` | Cancel the turn; reply when it has stopped |
| `Cancel()` | Append `input.withdrawn` for each queued input, then cancel the turn, in one step; reply when it has stopped |
| `Update(settings)` | Check the model; append `settings.changed`. A running turn takes the new model and settings at its next model call. When the new model has another kind of backend, one that owns its loop or one that does not, the turn fails at its next model call. The error says the model needs another kind of backend; the engine text was a diagnostic that named a file of the engine, so the runtime does not copy it |
| `SetGoal(...)`, `StartGoal(...)`, `AdjustGoal(...)`, `ClearGoal()` | Append goal events |
| `Compact(keep)` | Run a compaction as the run of the actor; `keep` replaces `compaction_keep_turns` |
| `Spawn(child, agent)` | Append `child.spawned`; return the `session.created` of the child. A settled child spawns again before it gets an input from the `task` tool; a child that has not settled appends nothing |
| `Settle(outcome, report)` | Append `child.settled` and admit the report as an input with `source: child`; a settled child changes nothing |
| `Release()` | Suspend the turn with cause `handoff`; stop; release ownership |
| `End()` | Fail with `session_busy` while a turn or a control command runs; else stop a compaction or an evaluation that runs, as `Release` does, stop each live child, then stop and release ownership. A child that this walk stops settles `canceled` with no report, and the `turn.ended` of its turn has cause `ended`, so a parent that opens later drops only those reports. The log stays |
| `Record(command)` | Append `command.recorded`; a repeated input ID returns the newest status |
| `Withdraw(id)` | Append `input.withdrawn` if still queued; otherwise append nothing |
| `Resolve(requestID, resolution)` | Append `request.resolved`; an answer starts a turn with no input, and a dismissal starts none. The receipt `protocol.Resolved` holds the `seq` of `request.resolved` and the `status`: `started` for an answer, `dismissed` for a dismissal |

The actor appends with no other goroutine. It checks the batch with `eventlog.Check`, appends it with `Store.Append`, applies each record, and publishes a new view. A command that needs durability replies after `Apply`. The store write is the only wait on disk in the actor.

The turn runner is one goroutine per turn. It gets a `turn.Request` that the actor builds from the `State`, calls the backend and tools, and sends each item and the end of the turn to the actor as commands. It touches no session field.

Lifecycle:

- `session.Create` and `session.Open` append and replay, but start no goroutine. They record the first turn, the resumed turn, or the next queued input. `Actor.Run` then starts the actor goroutine, the `Sync` sender, and that run. The runtime publishes the session before it calls `Run`, so a tool of the first run, such as `task` or `goal`, finds its own session.
- When the actor stops, for any cause, it cancels its run, refuses every later command with `ErrNotOwned`, and waits until each turn, compaction, and evaluator goroutine has exited. Only then does it wait for `Sync`, release its `Ownership`, and close `Done`. A next owner therefore never runs beside a run of the earlier actor, such as an external harness in its grace after SIGINT.

Reads use `atomic.Pointer[View]`. A `View` is immutable: the `protocol.Session` (status, turn, goal, queue, settings, usage, context gauge, last turn, compaction count, subscription usage, head seq) and whether the actor stopped. The gauge window is the window of the session model, or the window of the newest reading when the model reports none; `OpenView` has no backend and reads the window of the model from `modelmeta`. `Session.View`, `Runtime.List`, and a read of a session that the runtime does not run also fill `Plugins`, the name, state (`not-spawned`, `running`, or `errored`), tools, and hooks of each configured plugin: the plugin host owns that state, and the log does not hold it. The `View` also holds the profile of a child session and its unsettled children. A read of the whole `State`, such as the history for a plugin or the usage of a child, goes to the actor through `Actor.Read`, which runs a function on the actor goroutine and returns only when that function is not running; the runtime replays a log only for a session that it does not run.

### Ownership

```go
package harness

type Owner interface {
	Acquire(ctx context.Context, session string) (Ownership, error)
}

type Ownership interface {
	Epoch() uint64 // recorded in owner.acquired and carried by every SyncBatch
	Lost() <-chan struct{}
	Release()
}
```

`Create` appends `session.created` and `owner.acquired` to an empty log in one append. A conflict there fails with `ErrSessionExists`. An empty `model` is `Config.Model`, or `config.DefaultModel` when that is empty, after one alias lookup, as in `harness serve`. An explicit model gets no alias lookup.

Start-up order of `Open` after `Acquire`:

1. Read `Head`.
2. Append `owner.acquired{epoch, owner}` at that head. On `ErrConflict`, read `Head` again and retry. An old owner's append that lands first is ordered before the fence.
3. Replay through the fence record, then run.

After step 2, every append from a previous owner conflicts. When `Lost` closes, the actor stops without another append; `ErrConflict` from the store has the same effect. The default `Owner` grants every session to the local process, one grant at a time, at epoch 1 or at the `owner_epoch` of the config. Boxes supplies a lease, or writes `owner_epoch` for a box harness.

### State machines

Session status derives from the turn and the open requests:

| Status | Condition |
| --- | --- |
| `idle` | No turn is running |
| `running` | A turn is running |
| `waiting` | A turn ended `awaiting_input`; a request is open |

`retrying` is not a session status. A turn that waits for backoff sends an ephemeral `status` frame with `retrying`, `attempt`, and `next_at`, and the session stays `running`.

The actor has one run at a time. A run is a turn, a compaction, or a goal evaluation, and all three end through one path that answers the waiters of the run and then starts the next work. Status derives from the turn alone, so `idle` also holds while a compaction or an evaluation runs, and `running` always has a `turn_id`. A command acts on each kind of run like this:

| Command | Turn | Compaction | Evaluation |
| --- | --- | --- | --- |
| `Submit` | A `steer` input joins at the next item boundary; any other input waits | Waits | Waits |
| `Interrupt` | Stops the turn | Stops it and appends nothing | Stops nothing |
| `Compact` | `session_busy` | `session_busy` | `session_busy` |
| `ClearGoal` | Stops the turn with `goal_cleared` while the goal is active | The compaction continues | Stops it with `goal_cleared` |
| `Release` | Hands off the turn | Stops it and appends nothing | Stops it and appends nothing |
| `End` | `session_busy` | Stops it and appends nothing | Stops it and appends nothing |
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
| `handoff` | `Session.Release`, `Runtime.Close` | Stop at an item boundary: admit no new tool call, let running tools finish within the budget, append `turn.suspended`. A delegated backend (`OwnsLoop`) cannot stop at an item boundary, so a handoff interrupts it, records every item that it already wrote, gives each open tool call a cut-off result, and appends `turn.suspended`. A suspended turn has no open tool call, so the next owner resumes it automatically. |
| `provider_exhausted` | A usage limit of the provider: a spent quota, credit balance, or spend cap | Append `turn.ended{failed, provider_exhausted}`; keep the partial; queued inputs wait for the next input |
| `crashed` | `Open` finds `turn.started` with no end or suspend (forced stop, OOM, an exceeded handoff budget) | Append `turn.ended{interrupted, crashed}`; keep the partial; each open tool call gets a result saying it was cut off and to check whether it took effect before running it again; an assistant item, `[harness: this turn was interrupted by a process restart and could not complete]`, closes the turn before `turn.ended`, so the next user message does not join it on the wire. The session then starts the next queued input, or waits for input when none is queued. |

`turn.ended` types the cause in `cause`, as Codex `TurnAborted.reason` and the Claude Code `result` subtype do: `stopped`, `ended`, `goal_cleared`, `crashed`, or `provider_exhausted` (`handoff` is the cause of `turn.suspended`). `error` holds only the message of a failure, masked and capped; a turn that ended by its cause alone has no `error`. A `provider_exhausted` turn keeps the provider message in `error`, and the `RecoverHint` of the provider in `recover_hint` when the provider gave one. Any other failed turn types its error in `error_class`: `permanent`, `timed_out`, `rate_limited` (a rate limit that outlasted the retries), the class name of another retryable error (`overloaded`, `server_error`, `stream_truncated`), or `unrecovered`. Every reader branches on `stop_reason` and `cause` and never parses the text of `error`: `Apply`, the `last_turn` of the view (`stop_reason`, `cause`, `error`), the settlement of a child, the status of a task, and the rule that holds queued inputs. The `session.error` plugin event carries `error` as its message. The status and log of a task show the classified reason of the child report (see Settle), which the report of a child gives too.

After any other failed turn, the next queued input runs, as after a completed turn. Only `provider_exhausted` leaves the queue waiting, and `Open` follows the same rule: it starts the next queued input when no turn is open, except after a turn that ended `provider_exhausted`.

No tool call is ever re-run after a stop. This matches Codex, Claude Code, opencode, pi, and fx. The log stays strictly append-only, and a client hides output by cause if it wants to.

Input:

```
admitted ─► promoted   (queue: next turn; steer: next item boundary)
admitted ─► withdrawn
```

A `steer` input that joins a running turn reaches the model as one user message, as the engine writes queued prompts that it injects after a tool call: `OPERATOR MESSAGES (address these, then continue the task):` and a numbered list of the inputs that join together. A child report is not in that list: the model reads it in the `[tasks: …]` segment of the engine (see Children). A child report joins a running turn only on a model API backend, where the segment is in `<harness-engine-context>` tags. A backend that owns the loop takes no child report in the middle of a turn: on Claude Code the report waits in the queue, and the turn that starts next takes every queued report with its first input. When a prompt or another input starts that turn, the segment follows the text of that input, after a blank line, with no tags, as the engine wrote it. When only reports start the turn, the segment follows the trigger sentence of the engine, `A background task you started has finished. See the engine context below for its result, and continue accordingly.`, after a blank line, as the engine wrote it; the trigger sentence does not hold the result, so the result reaches the model once. A prompt that arrives while a turn runs joins the turn on both. A `steer` input with `expected_turn_id` fails with `turn_mismatch` if that turn is not running. A `steer` input on an idle session starts a turn. An input with no `delivery` is `steer`.

Goal:

```
active ─► paused ─► active
   └───► achieved | failed | exhausted
any ─► cleared
```

Goals follow Claude Code `/goal`. There is no deferred goal and no parked goal.

- `SetGoal` on a session with no running turn and no queued input admits the condition as an input with `source: goal`. That input starts a turn through the normal input events.
- `SetGoal` while a turn runs, or with queued input, starts nothing. The next turn that ends is the first one evaluated.
- A new `SetGoal` replaces the goal and resets its turn count. `SetGoal`, `ClearGoal`, each verdict, and each pause or failure withdraw the queued inputs with `source: goal`.
- After each turn that completes or is interrupted, the evaluator runs as the run of the actor, as a compaction does. It reads the condition and the history, and returns `met`, `not_met` with guidance, or `impossible`. Guidance is an input with `source: goal`. `met` yields `achieved`, and `impossible` yields `failed`. The evaluator skips leading markdown marks. A reply with no verdict is `not_met`, and the reply is the guidance.
- The evaluator is `goal_evaluator_model`, resolved through `aliases`. Its request pins effort `off` and a 256-token response cap, as the engine does, and a summary call caps its response at 1024 tokens. `SetGoal` without it is an invalid request. Its prompt copies the engine prompt, with a third form for `impossible`.
- A turn or an evaluation that fails on a retryable error or a usage limit yields `paused` with `retry_at`. The wait starts at 30 s and doubles with each pause before the next verdict, up to 30 min. At `retry_at`, the goal becomes `active` and judges the last turn again after an evaluator error, or admits an input that continues the goal. Any input resumes the goal at once. An error the user must fix yields `failed`.
- `max_turns` bounds goal turns; 0 is unlimited. Reaching it yields `exhausted`. `goal.set` carries `turns`, the count that the goal starts at: 0 for a new goal, and the current count for an adjust.
- With `goal_evaluator_model`, the runtime adds the `goal` tool to each session, as the engine does. `status` reports whether a goal is active or paused and its condition. `set` calls `StartGoal`: `SetGoal` with no turn limit, which fails while a goal is active or paused. `StartGoal` admits the condition as an input with `source: goal` even while a turn runs, and a new goal judges no turn while that input waits. So the turn that calls `set` is not judged; the condition runs as a turn of its own after it, as the engine posts the condition after the turn ends. `adjust` calls `AdjustGoal`: it replaces the condition of the active or paused goal, keeps `max_turns` and the turn count, and does nothing for the same condition. So the model cannot extend its own turn limit. There is no `clear` action; clearing stays with the operator. The tool copies the engine description and error wording. A child session has no `goal` tool: an engine child had the tool, but no goal of a child ever ran. No `goal` tool reaches a turn of a backend that owns its loop, as the tool bridge of the engine left it out.
- `ClearGoal` during a goal turn or its evaluation stops it with cause `goal_cleared` and returns after it ends. When the actor then runs nothing, `ClearGoal` starts the next queued input. An interrupt stops only the turn, and the goal judges the partial turn. An interrupt during an evaluation stops nothing.
- The goal lives in the log. `Open` restores it with its turn count. An `active` goal on an idle session judges the last turn when the goal has not judged it, and a `paused` goal keeps its retry time.
- At the switch, the `goal_met_first_turn`, `goal_not_met_then_met`, `goal_exhausts_max_turns`, and `bifrost_goal_*` rows are the oracle. The deferred and parked rows are deleted.

Request:

```
pending ─► answered | dismissed
```

A backend opens a request with `Sink.Ask` on an open tool call of its turn, with the call ID as the request ID. The turn then ends `awaiting_input`, keeps the call open, and the session reads `waiting`. `Resolve` closes the request. A dismissal closes the call with an error result that says the user dismissed the question. An answer leaves the call open, and the turn that it starts records the result. That turn has no input; a dismissal starts none. A question takes an answer that maps each question to text. An input admitted while a request is open dismisses it first, and so does a queued input that starts after the turn. A settings change to a model of another provider dismisses it too. Claude Code asks with `AskUserQuestion` when `Options.AskUserQuestion` is set and the session is not a child and has no active goal. The Codex CLI approvals come with phase 5.

### Compaction

Compaction runs as the run of the actor, never beside a turn. It copies the engine rules.

- A turn starts at a user message. A compaction folds every turn before the newest `compaction_keep_turns` (default 2). It never folds only the previous summary.
- `to_seq` is the seq before the first kept message. The `input.admitted` record of a kept or queued input can have a lower seq, so a reader that hides `from_seq` through `to_seq` keeps each `input.admitted` record.
- A backend without `OwnsContext` summarizes the folded messages with the session model and the engine compaction prompt. The actor appends `compaction.applied` with `by_backend: false`.
- A backend with `OwnsContext` runs `/compact` as a turn. The backend logs `compaction.applied` with `by_backend: true`.
- `Compact()` returns `protocol.Compacted`: the `from_seq` and `to_seq` of the new `compaction.applied`, `by_backend` for a backend with `OwnsContext`, and `folded`, which is false when the session has too few turns to fold and nothing else is set. It fails with `session_busy` while a turn runs or inputs wait. Its `keep_turns` replaces `compaction_keep_turns` for one call. A `keep_turns` below 1, or any `keep_turns` for a backend with `OwnsContext`, is `invalid_request`.
- Before a queued input starts a turn, the actor compacts first when the newest `context.measured` reading is at or above `compaction_threshold` (default 0.8) of the window of the session model, or of the window of the reading when the model reports none. A setting at or below 0 is the default. A model call with no prompt tokens records no reading.
- A failed summary appends nothing, and the turn starts on the full history. A handoff stops the summary and appends nothing.
- A model call that overflows the context window compacts while its turn runs, for a backend without `OwnsContext`, and the turn calls the model again on the new history. When no turn can fold or the summary fails, the turn fails. With no new input in the turn, a second overflow fails it: the summary already holds every turn but the newest kept turns.
- `Open` starts the next queued input when no turn is open, unless the last turn ended `provider_exhausted`. The next owner thus runs the input that waited for a stopped summary, and compacts first when the reading still passes the threshold.

### Children

A child is a session with `parent_id`. The `task` tool starts it in the background, as the Task tool of Claude Code starts a background subagent. The runtime adds the `task` tool only with a `WorkDir`. The parent holds the child ID, not the child's state. Task notifications become inputs; the separate checkout-and-commit queue is deleted.

- Spawn. The tool reads the agent profile (default `general-purpose`) and checks the tree limits. The parent actor appends `child.spawned`. Then the runtime creates the child in one append: `session.created` with `parent_id`, `agent`, the service tier of the parent, the effort of the call (none when the call names none), the model of the call, else of the profile, else of the parent, and the allowed tools of the parent narrowed by the profile; the task as an input with `source: parent`; and the `turn.started` of that input. The `effort` of the call and the model that the spawn uses, from the call or the profile, are checked before the spawn: an effort that is not `off`, `minimal`, `low`, `medium`, or `high` fails the call, and so does a model that is not `provider/model` (an alias is not looked up) or that no provider of the registry serves; the error of an unserved provider lists the valid aliases and providers. The tool returns the child ID at once. A child that fails to start settles `failed` with no input, and the tool call fails.
- Settle. When a turn of a child ends, the child reports to its parent: `done` for a completed turn, `failed` for a failed or crashed turn, `canceled` for a stopped turn. A completed turn with a queued input does not report; the next turn reports, as the engine runs a queued message before it notifies the parent. The report names the child, its agent, the outcome, and the reason, and holds the last assistant text of the child, as the Task tool of Claude Code returns. The parent appends `child.settled`, with the child turn ID as `result_ref`, and the report as an input with `source: child`, in one append. An idle parent starts a turn with it. A busy parent takes it by its backend, as the engine did. On a model API backend the report is a `steer` input at the next item boundary, as the engine delivered a task notification at its next model call. On Claude Code the engine checked out reports only when a turn started, so a busy parent gets no report in the middle of its turn: the report is a `queue` input, and the next turn that starts reads it, together with every other report that waits, as the engine checked out all pending reports at the start of a turn. A backend that owns the loop (`OwnsLoop`) takes this rule, so the choice never reads a backend name. A prompt that arrives while the parent is busy still joins the turn on both. The model reads the report as the segment of the engine, not under the `OPERATOR MESSAGES` heading. The segment is a message of its own that is pinned: the model reads it at the same place in each later model call, and it is in no history that a reader sees (`View.Messages`, the `log` of `task`, `get_conversation_history`, `client/session.messages`), as the engine pinned it and kept it out of its history. A drain that holds a prompt and a report gives two messages: the `OPERATOR MESSAGES` block, then the segment. A compaction moves each pinned segment that it folds to the end of the history. A compaction that a running turn makes settles the segment there at once, where the turn loop reads it, and the model reads it at that place in each later call of the turn and in each later turn. A compaction that runs outside a turn leaves the segment at the end of the history for the model, and the next turn start settles it there, after the inputs of that turn. A pinned segment after the cut keeps its place. A pin is fixed to the messages around it and replay rebuilds it from the log, so a pin survives a restart. The engine kept each pin as a slot number in memory, so the place of a pin after a compaction and after a restart differs from the engine (see the question on pinned segments under Open questions). The segment holds `[tasks:` and a line `<child> (agent=<agent>) done: <text> (usage: <in> in / <out> out)` for each report that joins together, then `]`. On Claude Code a turn that another input starts reads the segment after the text of that input, after a blank line (`\n\n[tasks: …]`), as the engine appended it; the report that such a turn takes reads there only, so its result reaches the model once. A failed child reads `failed: <reason>`, and a stopped turn reads `failed: canceled`, as the engine did. The reason is the classified text of the engine: a fixed prefix by `error_class`, then the error text of the turn end. `permanent` reads `turn failed with a permanent provider error and cannot succeed on retry: <error>`, `unrecovered` reads `turn failed and did not recover: <error>`, `timed_out` reads `timed out`, and another retryable class reads `provider <class> errors exhausted the retry budget: <error>`. A `provider_exhausted` turn reads `provider capacity exhausted for this account: <error>`, and `rate_limited` reads `provider rate limit outlasted the retry budget for this account: <error>`. A turn with no `error` reads its `cause`, except that a crashed turn reads `lost to restart: turn was in flight when the process last stopped`, as the engine wrote it, and an error with no class reads as it is. The `error` in the reason is the `error_detail` of the turn end, or its `error` when there is no `error_detail`: the engine masked the cause with its secret patterns and cut it at 500 runes with `… [truncated]`, and the runtime does the same. The recover hint is masked the same way and cut at 120 runes. The `status` and `log` actions of the `task` tool show this reason, as the engine showed its classified reason. A child that the cutover converted keeps the cause half of its engine reason and its class, and its `recover_hint`. For these two kinds of wall the line ends, after the usage, with the guidance of the engine: ` — provider exhausted, child preserved: do not spawn a replacement (every session on this provider account hits the same wall); resume this child[ after <recover_hint>] with task send on session_id <child>`. A result of at most 4096 bytes stays whole in the report, and so does a larger result that is 4096 bytes or less once masked: the report holds the masked text. A larger result is masked, and the report holds the first 4096 bytes of the masked text and ` … [full result retained — read the rest with read_tool_result(handle="trh_N")]`, as the engine did: the parent retains the masked result as `tool_result.retained` in the append of `child.settled` (see Tool-result retention). When the parent cannot read the handle back (its backend owns the loop, its allowed tools omit `read_tool_result`, the retained total would pass the budget, or the write of the blob fails), the preview ends `… [truncated; full result unavailable]`. A runtime with no retention cuts the result at 4000 runes with `… [truncated]`. A newline in the text becomes a space. The `usage` is the total of the child. A child settles once, so a later report changes nothing until a `send` of the `task` tool spawns it again. Each turn end of a child reports, and the runtime opens a parent that it does not run, so a later input to a settled child opens its parent again.
- Crash and handoff. Each session follows its own rules. A child that a handoff suspended resumes when it opens. A crashed child turn ends `crashed` and settles `failed`. When a parent opens, it settles each unsettled child that has ended, because a crash can come between the `turn.ended` of the child and the `child.settled` of the parent. A child whose last turn the end of a session stopped (cause `ended`) settles `canceled` with no report input; every other child that has ended settles with its outcome and its report, a `canceled` one included, as the engine did for a turn that a crash left. The silence of a tree interrupt lasts only for its walk, so it does not reach this rule. It settles a child with no log `failed` with no input, and opens each other child, which reports when its turn ends. No tool call runs again.
- Limits. One `internal/tree.Tree` holds `max_task_depth` (default 3) and `max_concurrent_tasks` (default 20) for each root. A negative value fails `Validate`. The runtime reads the root and the depth of a session once, when it loads the session: from its parent when this runtime runs the parent, else from the `session.created` records of the ancestors. The count is the unsettled children in the views of the sessions of the tree that this runtime runs, so a child that a parent opens after a restart counts at once, and so does a settled child that a `send` runs again. No ledger holds the count apart from the logs. A lock for each tree makes the count and the append of `child.spawned` one step. `max_tree_tokens` (default 0, no limit) is the token budget of a tree, as in the engine: a spawn fails once the root and its descendants have used that many tokens, input, output, and cache tokens summed. The sum reads the usage that each log of the tree records, so it holds across a restart. Each spawn reads the log of every session of the tree, through `Actor.Read` for a session that this runtime runs and by replay for any other, so its cost grows with the size of the tree logs; the engine kept a running sum in memory. The budget is opt-in, so this cost is accepted. A spawn past any limit fails the tool call. `HARNESS_MAX_TASK_DEPTH`, `HARNESS_MAX_CONCURRENT_TASKS`, and `HARNESS_MAX_TREE_TOKENS` set the keys through `ApplyEnv`.
- Actions. The `task` tool keeps the engine actions and wording. `action` defaults to `spawn`. `cancel`, `status`, `send`, and `log` take a `session_id` that the caller spawned, directly or through its own children; the tool walks the parents of the target to check it: the view of a session that this runtime runs, else its `session.created` record. `status` reads the log of the target: its status (`running` until its last turn ends, then its outcome), parent, depth, children, agent, final text, failure, and usage. `log` returns the newest `tail` messages (default 20, at most 100) as text entries, each cut at 2000 runes, newest first within 20000 runes. `send` admits the text as a steer input with `source: parent`, so a running child takes it at its next item boundary and an idle child starts a turn. When the parent has settled the child, the parent appends `child.spawned` again first, with the agent of the child, so the child reports the new turn. A parent that this runtime opens for the send settles or opens its unsettled children before the send reads them. Two windows remain: a crash between the `child.spawned` and the input repeats the earlier report, and a child turn that ends just before a send can settle after the send reads the parent, so the turn of that send does not report. `cancel` withdraws the queued inputs of the target and of each of its descendants and stops their turns, as the engine cancel drops the queue of each canceled session, so a queued `send` never runs. The report of the target reaches its parent. The queued `send` note keeps the engine hedge that an interrupt leaves a queued message undelivered.
- Tree interrupt. `interrupt {tree}` stops the turn of the session, then withdraws the queued inputs and stops the turn of each descendant that this runtime runs (`Cancel`). Before it stops a session, the walk silences the children of that session, so a descendant that ends during the walk settles with no report input and no parent inside the tree starts a turn. The walk outlives the request context. A stopped descendant settles `canceled` with no report input. A tree interrupt opens a parent that this runtime does not run and settles the child there with no report input. `DELETE /sessions/{id}` differs: it stops each descendant turn with cause `ended` (see Routes below) and opens no parent; the child settles, with no report input, when the parent opens. At the switch, the `contract_children` rows are the oracle; the cancel-tree row matches the engine for the queued send, which is dropped, and changes by design in one way: a later send is not refused.

### Shutdown

One `sync.WaitGroup` per runtime tracks every goroutine that the runtime starts. `Runtime.Close` releases each session with cause `handoff` and waits for the group. It then closes the model connections, stops the processes, and closes the MCP servers, once. When its ctx ends first, it stops the remaining sessions without an append, which cancels their turns, and still waits for the group before it closes them; their next `Open` finds a crashed turn. A turn ends when its tools and its backend return: a tool gets the canceled context, and an external harness gets SIGINT and is killed after its grace (5 s for Claude Code). This matches the engine `Drain`, which canceled the prompts and waited for them. New work that `Close` would wait for fails with `ErrDraining` after `Close` starts; work that already runs finishes or is canceled.

## HTTP

### Routes

```
POST   /sessions                              create; client id optional
GET    /sessions?after=&limit=                list in creation order
GET    /sessions/{id}                         view
PATCH  /sessions/{id}                         model, effort, service_tier
DELETE /sessions/{id}                         end; 204; the log stays
POST   /sessions/{id}/inputs                  submit
GET    /sessions/{id}/inputs                  queued inputs
DELETE /sessions/{id}/inputs/{input}          withdraw
POST   /sessions/{id}/interrupt               {turn_id?, tree?}
POST   /sessions/{id}/compact                  {keep_turns?}; 200 with {from_seq, to_seq, by_backend, folded}
PUT    /sessions/{id}/goal                    {condition, max_turns}; 200 with the view
DELETE /sessions/{id}/goal                    204
POST   /sessions/{id}/requests/{request}      {answer} | {dismiss}; an answer replies 202 {seq, status}, a dismissal 204
GET    /sessions/{id}/events?after=&limit=    page; SSE with Accept: text/event-stream
GET    /sessions/{id}/messages?before=&limit= page of the conversation, numbered from 1
GET    /models                                models and their capabilities
GET    /commands                              slash commands
GET    /processes · POST /processes/{name}/{action}
GET    /processes/{name}/logs?tail=           last log lines and status
GET    /workspace/changes                     working-tree diff
GET    /health
```

There is no version prefix: harness and its clients change together.

A route that only reads a session, `GET /sessions/{id}`, `GET /sessions/{id}/inputs`, `GET /sessions/{id}/messages`, and the page form of `GET /sessions/{id}/events`, answers from the view of a session that this runtime runs, and replays the log of any other session as `OpenView` does. It never opens a session, so it appends nothing and starts no turn. `Runtime.Open` runs for each route that changes a session and for the SSE stream of its events, which tails them as they happen. `DELETE /sessions/{id}` calls `Runtime.End`, which opens nothing: it stops a session that this runtime runs when no run is on, and stops the turn of each descendant that this runtime runs, as the tree interrupt does, and each of those turns ends with cause `ended`. A session that this runtime does not run stays closed, also when a descendant of it settles, and the child settles when the session opens, with no report input for a turn that ended with cause `ended`. A child that a direct interrupt stops still reports `canceled` to its parent; a tree interrupt keeps its rule of no report input. It answers 204 for any session that has a log, and 404 for an ID with no log. A restart opens no session by itself: the embedder opens each session that has work to resume (`View.Resumable`), `cmd/harness` when it starts and boxes when it wakes the box. A session that has no work stays closed and gets no `owner.acquired`; a log that does not replay is skipped and logged.

`Runtime.Handler` serves every route above. A route that answers with more than one success status has one entry for each status in the route table: `POST /sessions/{id}/inputs` answers 201 for a new input and 200 for a repeat, with the same body, and `POST /sessions/{id}/requests/{request}` answers 202 with `{seq, status}` for an answer and 204 for a dismissal. `interrupt` takes `tree`. `GET /health` answers `protocol.Health`: `status`, `version`, `vcs_revision`, `vcs_time`, `session_sync` (`fsync` or `volume`), `started_at` (the start of the runtime, RFC 3339 UTC), and `capabilities` (`delta_row_identity`, which says that each `item.delta` frame names its item). The handler has no authentication; the embedder wraps it.

Today harness has 36 routes and seven ways to read a session. This has one log and one cursor.

### Inputs

Request: `{id, parts, delivery?, source?, expected_turn_id?}`. The client mints `id`. `delivery` is `steer` when the request has none: a prompt that arrives while a turn runs joins that turn at its next item boundary, as the one queue of the engine delivered it. A `queue` input waits for the next turn. The runtime admits an internal goal input and `/compact` as `queue`.

A part is `{type: "text", text}` or an attachment `{type: "blob", media_type, data}` with its bytes in `data` as base64. The engine accepted png, jpeg, gif, webp, and pdf attachments, and so does the runtime: an attachment is at most 20 MiB, its bytes must be the type that it claims (an image decodes as that image, a PDF begins with `%PDF-`), and a request body is at most 32 MiB (8 MiB for any other route). A part that fails one check is `invalid_request` and leaves no trace. The runtime stores the bytes with `Store.PutBlob` under `attachment-<sha256>` before it appends `input.admitted`, whose blob part holds `media_type`, `blob_key`, and `bytes` and never the bytes. `Sync` carries the blob in the batch of that record, as it carries `backend.state`. A turn reads a blob through `turn.Request.Blob`. `modelapi` sends it as an image or file part of the wire, and `claudecode` sends an `image` or `document` content block on stdin after the text block. A steer input keeps its blob parts. The blob is stored before the input is appended, so an input that then fails (a conflict, a turn mismatch, or no ownership) leaves its blob in the store; a retry with the same bytes reuses it.

| Case | Response |
| --- | --- |
| New id | `201 {input_id, seq}` |
| Same id, same body | `200` with the original receipt |
| Same id, other body | `409 input_conflict` |

A typed slash command answers the same way. Its receipt adds `command`, the newest status of the command, and its `seq` is the first `command.recorded` record. See "Slash commands".

### Events

- One per-session `seq` serves paging and SSE resume.
- Only SSE frames for durable records carry `id: <seq>`. `Last-Event-ID` and `after=` resume exactly, across processes.
- A live frame (`item.started`, `item.delta`, `status`) sets `protocol.Event.Ephemeral` and is never stored. Its `seq` is the last durable seq when it was sent.
- A subscriber reads durable records from the log, so a slow subscriber never misses one. A full subscriber drops ephemeral frames, so the deltas of an item can have holes; its `item.completed` holds the whole item. An error ends a stream with an `error` frame.
- Replication is `Options.Sync`: each `protocol.SyncBatch{epoch, session, from_seq, records, blobs}` is a remote append. The blobs of a batch are those that its records name: `backend.state`, `tool_result.retained`, and the blob parts of `input.admitted`. The receiver rejects an older epoch. A batch with `from_seq` at its head plus one is appended. A batch whose records are all at or below its head is a retry: identical bytes are acknowledged as a duplicate, and different bytes are rejected. Any other `from_seq` is a seq mismatch. Every reply is a `protocol.SyncAck{head}`, including a seq mismatch, so the sender resends from `head+1` out of its own Store; that also heals a receiver that missed records before a crash. `harness.ApplySync` implements these receiver rules over any `Store`.
- The sender reacts to a rejection by its error. `ErrStaleEpoch` (an older epoch), `ErrConflict` (different bytes at a seq), and `ErrSyncRejected` (a request that a resend cannot fix) are final: a resend cannot change them. Each one stops the session with no further append and releases its `Ownership`. `Session.Release` then returns the rejection, and `ErrSessionNotOwned` after a stale epoch. Any other error resends the same batch with backoff (250 ms, doubling to 30 s) until it succeeds or the ownership ends. On start, a box harness replicates every session that its `Store` holds, not only the sessions that it opens, so the logs that the cutover conversion wrote reach the receiver through this path. A receiver diverges when two owners at one epoch write one store, or when a box disk is restored behind the receiver.
- A box harness replicates through the config keys `owner_epoch` and `sync {url, token_file}`, which `boxinit` writes from `BootConfig`. With `sync` set and no `Options.Sync`, `New` builds a sender that posts each `protocol.SyncBatch` as JSON to `sync.url` with `Authorization: Bearer <token>`, and reads the token from `token_file` for each post. Boxes serves `POST /v1/boxes/{id}/sync`; a `200` carries the `protocol.SyncAck{head}`, also on a seq mismatch. The reply follows one contract, and every status that it lists as final stops the session: `200` with `SyncAck{head}`; `409` with `stale_epoch` is `ErrStaleEpoch`; `409` with `sync_conflict` is `ErrConflict`, so the sender stops replicating that session and logs the rejection; `400` with `invalid_request` is `ErrInvalidRequest`; `413` with `too_large` when the body is over 32 MiB; `401` and `403`. The `400`, `413`, `401`, and `403` rejections also match `ErrSyncRejected`. A `5xx` and a transport error resend the batch with backoff. The sender keeps the JSON body of a batch under 32 MiB by cutting it after fewer records than the page of 512, with the blobs of the records that it keeps, so a `413` happens only when one record with its blobs is over the bound, and it is final. `New` refuses `owner_epoch` beside `Options.Owner`, and `sync` beside `Options.Sync`.
- `Runtime.CatchUp` is the start of a box harness: for each session in the `Store` that this runtime does not run, it takes a grant of the `Owner`, sends the log in pages from seq 1 (each acknowledgement moves the next page to the head that the receiver reports), and releases the grant. It appends nothing, so an idle session gets no `owner.acquired`. `Open` of a session that `CatchUp` holds ends the catch-up of that session first, and the opened session replicates itself. `Close` ends `CatchUp` with `ErrDraining`, and the next start catches up again. A stale epoch ends it with `ErrStaleEpoch`. If that `Open` fails, `CatchUp` replicates the session itself. Two calls at once run one after the other. A session that fails for another reason, such as a log that cannot be read, is skipped, and `CatchUp` returns each failure with its session ID after the other sessions finish. The phase 4 switch makes `cmd/harness` call it at start, beside the opens of the sessions that have work to resume.
- The epoch is a number because fencing needs order. An embedder maps its own claim to a monotonic epoch; boxes uses `claim_epoch`, a plain counter that boxes increments when it admits a Spawn, and its string command ID stays the workflow token. Boxes switches its ownership comparison to that counter in the same release.

### Messages

`GET /sessions/{id}/messages?before=&limit=` answers a `protocol.MessagePage` of the conversation that the model reads, oldest first, as the engine answered its message page: `messages`, `first_seq`, `last_seq`, `total`, `has_more`, and `commands`. The seq of a message is its place in the conversation, counted from 1: the summary of the newest compaction is 1 when one exists, and each later message counts on, so a compaction renumbers, and a message ID is the stable key. `before` names a seq: the page holds the messages that precede it, and a missing `before`, or one past the end, names the newest page. A page before `first_seq` is the next older page. `limit` is 100 when absent or 0; a limit above 1000 is `invalid_request`. A `before` or `limit` that is negative, not an integer, empty, or repeated is also `invalid_request`. The IDs derive from the log: `msg_<input_id>` for an input that a turn started with or that a steer promoted, `msg_<item_id>` for an item, `msg_resolved_<request_id>` for the result of a dismissed request, and `cmpsum_<seq>` for the summary. A message holds `id`, `role`, and `parts`; a tool result holds its text in `content`, and a blob part names its media type and size. A `commands` entry is the newest record of a typed slash command, placed after the message that was the newest when its first record was appended. The engine bootstrap form (`stream_from`, `live_from`, `seqs`) has no counterpart: one page route and one event cursor replace it. `View.Messages` reads the same page for a session that this runtime does not run.

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
| `no_base` | 409 |
| `too_many_changes` | 409 |
| `process_not_found` | 404 |
| `payload_too_large` | 413 |
| `draining` | 503 |
| `unauthorized` | 401 |
| `internal` | 500 |

Each code except `internal`, `payload_too_large`, and `unauthorized` is a sentinel error and a `protocol` constant. `cmd/harness serve` writes `unauthorized` in its token check, before the request reaches `server`. The session codes are sentinels in `harness`. `process_not_found` is `process.ErrUnknownProcess`, and the three git codes are sentinels of `internal/workspace`. `server` maps it with `errors.Is`. A body above its limit fails with `payload_too_large`: 32 MiB for `POST /sessions/{id}/inputs`, and 8 MiB for any other route. Any other error is `internal`, and its message is a fixed string. A path or method that no route serves answers 404 or 405 with `invalid_request`.

As the engine did, the actor masks and bounds each error text that it writes to the log (`turn.ended` error, `goal.changed` reason): recognized credential shapes are redacted (best effort: a free-form text can hold a shape that the fixed patterns miss), and a text longer than 256 runes keeps its first 256 runes and ends with `...[truncated]`. A failed `turn.ended` also holds the message as a report to a parent reads it, in `error_detail`, when that differs from `error`: masked by the secret patterns of the engine, and cut at 500 runes with `… [truncated]` (see Settle).

### Slash commands

`Session.Submit` resolves an input with `source: typed` and one text part through the `command` package, with the dispatch rules of the engine server. Any other input is never a command.

- Not a command: the input is admitted as is. `//x` is admitted as the text `/x`.
- An unknown name: the prompt command of that name under `commands_dirs` (default `<WorkDir>/.agents/commands`) is admitted as its expanded text with `source: command`. With no such file, or no `WorkDir`, the line is admitted as text.
- Bad arguments: one `command.recorded` with `failed` and the error of `Resolve`. Nothing runs.
- A frontend command, or a control command with no operation here (`queue-clear`): `unsupported`, "/<name> is not available in this client".
- A control command that is not `available_during_task` while a run is on: `refused`, "/<name> cannot run while a turn is running; send it again after the turn ends".
- Any other control command records `accepted`, runs after `Submit` returns as work that `Runtime.Close` waits for, and records one more status. After `Close` starts, such a command fails with `draining` and records nothing. The statuses: `succeeded` ("/<name> succeeded", with the JSON result up to 16 KiB), `failed` (the error text of a sentinel error, or "/<name> failed: internal error", also for a panic of the operation), `refused` (a `session_busy` error), or `interrupted` (the runtime stopped).
- The operations are the Go API: `abort` is `Interrupt`, `compact` is `Compact`, `goal` is `SetGoal`, `goal-clear` is `ClearGoal`, `model`, `thinking`, and `tier` are `Update`, `status` is `View`, `queue` is the queued input IDs, and `processes` is the process list of `GET /processes`. `/compact [keep_turns]` passes `keep_turns` to `Compact`. Its result is the `protocol.Compacted` of `Compact`. A compaction with no turns to fold fails with "/compact did nothing: the session does not have enough turns yet to fold".
- `<name>` is the name or alias that the user typed. The command never becomes an input, so the model never sees it.
- A repeat of the input ID with the same line returns the newest status; another line, or an input ID of another input, is `input_conflict`.
- `Open` records `interrupted` for each command that an earlier owner accepted and never finished: "harness restarted before /<name> finished; it will not run again". No command runs again.

`GET /commands` returns `Runtime.Commands`: each built-in command and each prompt command, sorted by name, with `serve_support` by name and `discovery_errors` for files with no usable name. A control command names its operation and `available_during_task`; the handler adds the route of the same operation, where one exists. A prompt file that is not valid is listed, unsupported, with its error as the reason. The engine records the label of a prompt command; the runtime does not, and a client shows the expanded text. Switch oracle: `builtin_commands_run_and_record`, with the receipt, the record, and the routes in the new shape.

### Contract source

Go types in `protocol` are the source. `go generate ./protocol` writes `protocol/openapi.json` and `protocol/protocol.ts` with `github.com/invopop/jsonschema` and a route table that `server` exports. CI regenerates and fails on a diff. A test walks the route table against the mux. The hand-written `server/openapi.yaml` is deleted. The generated contract holds what the handler does: each request body is optional, because the handler reads an empty body as the zero request; a type that only a request holds refuses unknown fields, as the handler answers `invalid_request`; and each success status of a route is an entry of the table. The process, logs, and health bodies are `protocol` types (`ProcessInfo`, `ProcessStatus`, `ProcessLogs`, and `Health`).

### Command

`cmd/harness` is the composition root of the runtime. It resolves environment variables and flags, builds one `harness.Runtime`, and holds no runtime behavior.

- `serve` applies the `HARNESS_*` variables with `config.ApplyEnv`, then the flags `-no-instructions`, `-skills-dir`, and `-agent-def-dir`, so a flag wins. It keeps the sessions in a `DiskStore` at `HARNESS_SESSION_DIR`, else `session_dir`, else `~/.harness/sessions`, and passes `Options.ServeURL` and `Options.RunToken` to the plugins. It serves `Runtime.Handler` behind the bearer token of `HARNESS_RUN_TOKEN`; `GET /health` needs no token, a request without the token gets 401 with the error code `unauthorized`, and an empty token is allowed only on a loopback bind or with `-unauthenticated` or `HARNESS_UNAUTHENTICATED`. With `-cors-origin` it adds the CORS headers, and a preflight skips the token. It listens first. Then it calls `CatchUp` and opens each session that has work to resume, and a failure of either is logged and never stops `serve`.
- `serve` wraps its store to log what the engine hooks logged: a store operation that takes over 1 s (`slow store phase`), an operation that still runs after 5 s, repeated every 5 s (`store phase in flight`), the append that creates a session (`session created`, with its time), and the append of `child.spawned` (`task spawned`, with the count of the process). It also logs the config summary at start, and each GC pause of 200 ms or more.
- `run -p <prompt>` or `run -goal <condition>` creates a session, or opens the one that `-r <id>` or `-c` names, on a `DiskStore`, or on a `MemStore` with `-no-save`. `-model` replaces the model of an opened session, `-system` appends a prompt segment, and `-max-tokens` sets `Options.MaxTokens`, the output cap of each model call, as the engine flag did. It submits the prompt as a typed input, so a slash command runs as `Submit` runs it, and prints each text delta of the session and of each task child to stdout, each tool call and each failed tool to stderr, or each event of the session and of each task child as a JSON line with `-json`; each line holds the `session_id` of its session. A task child that the run sends to again prints only the records that the new input adds. It returns when the session has settled. It exits 1 when the turn failed, also under `-goal`, or a typed command did not succeed, and 3 when `-goal` does not reach `achieved` and no turn failed. It prints `session: <id>` on stderr when it saved the session.
- `sessions` lists the stored sessions in creation order with their message counts, or a JSON array with `--json`.
- `plugin probe` calls `Runtime.ProbePlugins` and prints each plugin with its hooks. There is no manifest cache to refresh.
- `serve` deletes `serve-stop.json` in the session directory at start. On `SIGINT` or `SIGTERM` it stops the turns, which hand off, waits for `Sync` to acknowledge every record, and closes the runtime and the HTTP server within 5 s. Then it writes the file as one line, `{"stop":"handoff"|"crashed","sync":"synced"|"unsynced"}`, and exits. `stop` is `handoff` when `Runtime.Close` returned nil and `crashed` when it failed or passed the deadline. `sync` is `synced` when `Close` returned nil, a `sync` is configured, the start caught up every stored session, and `Runtime.SyncStopped` is false, else `unsynced`. A `Close` that finds a session that Sync rejected for good does not fail for it; the stop is still `handoff`, and `sync` is `unsynced`. A missing file, such as after a kill, means `crashed` and `unsynced`; `boxinit` reads the file and posts both facts on the boot-progress route.

## turn and backend

### Backend

```go
package turn

type Backend interface {
	Capabilities(model string) Capabilities // model is a provider/model ref
	Run(ctx context.Context, req Request, out Sink) (Result, error)
}

type Capabilities struct {
	OwnsLoop      bool     // backend runs tools and multi-step turns
	OwnsContext   bool     // backend compacts its own context
	OwnsMCP       bool     // backend connects Config.MCPServers itself on an unrestricted turn
	Steering      bool     // a backend with OwnsLoop takes steer input mid-turn through Sink.Steer
	ContextWindow int      // 0 means the backend reports it
	Tools         []string // built-in tools of a delegated backend
}

type Sink interface {
	Item(m eventlog.Message) error
	Delta(itemID string, d Delta)
	Alive()
	Telemetry(t Telemetry)
	Steer() ([]eventlog.Message, error)
	State(backend string) ([]byte, error)
	SaveState(backend string, blob []byte) error
	Compacted(summary string) error
	Ask(callID, kind string, payload json.RawMessage) error // open a request on an open tool call
	Resolution(id string) (eventlog.RequestResolved, bool)  // the record that closed it
}
```

The turn loop reports through one `Turn`, which the actor binds to the turn. No method takes a turn ID, and a call after the turn is no longer the run of the actor fails with `turn mismatch`:

```go
type Turn interface {
	Sink
	Started() string               // announces an item and returns its ID; the next Item records under it
	Status(f protocol.StatusFrame) // ephemeral frame
	Settings() (string, eventlog.Settings) // the model and settings that the session holds now; "" after the turn stops
	CompactTurn(ctx context.Context) (history []eventlog.Message, ok bool, err error)
	Ended(err error)
}

func Run(ctx, step context.Context, b Backend, req Request, src Source, to Turn, lim Limits)
```

`Delta` ignores the backend `itemID`: one harness item can join several backend items, such as reasoning and text. The first delta of each model call calls `Started`, which gives the item a harness ID; the next `Item` records the message under it. An item that a failed call streamed gets no `Item`, and the next `Started` replaces it.

- A model API backend runs one model call per `Run`. The loop runs the tools, then takes the queued steer inputs through `Sink.Steer`, appends them to the history, and calls the backend again. This item boundary comes after the tool results of each response, on a path that calls the backend again. A response that ends the turn at its `max_tokens` stop, or fails it, takes no steer input: that input waits and runs as the next turn. A queued input joins no running turn. Every model API backend steers, because the loop does it, not the backend.
- A delegated backend (`claudecode`) runs the whole turn and reports items.
- `AllowedTools` holds tool names in one namespace: the embedder tools, the runtime tools (`goal`, `process`, `task`), the plugin tools, the MCP tools, and the built-in tools of the model. A model API backend has the file tools of the `WorkDir` and `read_tool_result` as its built-in tools. A delegated backend has the tools of `Capabilities.Tools`, and any other name fails `Create`. A tool of the runtime or of a plugin with the name of a built-in tool of the backend also fails `Create`. One predicate, `known(model, name)`, decides whether a name is a tool of a session at that model. `New`, the plugin start, `Create`, `Update`, and the tool list of a child profile all use it.
- Retry, the stall watchdog, and compaction read `Capabilities`. No code compares a provider name.
- A backend marks a failed model call with a `turn` sentinel. `ErrRetryable` (a 429, a 5xx, a truncated stream, a response with no output) calls the model again after a wait of 1 s that doubles up to 8 s, with jitter, up to `prompt_retries` times, when the call has recorded no item. A backend with `OwnsLoop` runs its call once, as the engine ran every delegated turn, and its retryable error keeps its class, so a goal still pauses on it. `ErrContextOverflow` compacts (see Compaction). `ErrExhausted` ends the turn with cause `provider_exhausted`. Any other error fails the turn.
- The stall watchdog ends a model call that reports nothing for `stream_idle_timeout_s` (default 300, as Codex). A delta, an item, and `Sink.Alive` each reset it. `modelapi` calls `Alive` for each provider event with no delta, such as a keep-alive or a tool argument that still streams. The stall is retryable. A compaction summary has the same watchdog and no retry. A negative value turns it off. A backend with `OwnsLoop` has no watchdog: its own tools can run silently for a long time.
- `Result.MaxTokens` reports a response that the output cap cut off. The loop runs none of its tool calls and records an error result for each. `modelapi` records a tool call with arguments that are not valid JSON, such as arguments that the cap cut, with no arguments. The next call ends with a continuation message that the log never holds, in `<harness-engine-context>` tags, so the model reads it as engine text. After `max_tokens_continuations` (default 3) continuations, the next cut-off response fails the turn; 0 or less ends the turn at the first one. The count never resets in a turn.
- Private backend state is one `backend.state` event plus a blob, one blob key for each owner. The Claude Code transcript mirror is that blob. The runtime has none of the eight `claudeCode*` fields of the engine or their record kinds.
- Model metadata comes from `modelmeta`. An unknown model fails with `model_unavailable` at create and at a settings change.
- `Telemetry` carries the usage of one model call, its context reading, the subscription snapshot that the provider reported with it, and its cost.

### Model API backend

`internal/backend/modelapi` is the one backend for every model API. It holds one `provider.Provider` client, and only the client differs between wires. `harness` builds the client from the provider entry. One function maps an entry with no type by its key.

| Entry | Client |
| --- | --- |
| `type: openai`, or the `openai` key with no type | `provider/openai` (Responses, the Codex lane included) |
| `type: openai-compat` | `provider/openaicompat` (chat completions, such as Bifrost) |
| The `anthropic` key with no type | `provider/anthropic` (Messages) |
| `type: claude-code-cli` | `internal/backend/claudecode`, a delegated backend |

`Options.ModelTransport` wraps the HTTP client of each entry, and may supply the credentials of each wire. Each request sends a baseline response cap of 8192 tokens. A reasoning request can raise it. `Close` reaches a client that pools connections through an optional `Close` method. Each provider package keeps its own SSE reader and status classifier, and the one retry policy stays in `turn`.

### Third-party harnesses

Delegating a turn to another agent harness is permanent. Claude Code is built; phase 5 adds the Codex CLI. Each one is a `Backend` with `OwnsLoop`, `OwnsContext`, and `Steering` set. They share one adapter contract, so adding a harness adds one package and touches nothing else.

| Concern | Contract | Claude Code | Codex CLI |
| --- | --- | --- | --- |
| Turn | One harness turn is one external turn | `--resume` print run | `turn/start` |
| Items | External items become `item.completed` | stream-json frames | `item/completed` |
| Steer | A `steer` input reaches the running turn | stdin | `turn/steer` with `expectedTurnId` |
| Interrupt | Stops the external turn; reports `interrupted` | SIGINT | `turn/interrupt` |
| Requests | Questions and approvals become `request.opened`; the resolution goes back | `AskUserQuestion` defer | `requestApproval`, `requestUserInput` |
| State | External session id and transcript mirror are one `backend.state` blob | `--session-mirror` | rollout file |
| Tools | Harness tools reach the external harness through a harness-hosted MCP endpoint; `AllowedTools` applies. An unrestricted turn gives `Config.MCPServers` to the external harness beside that endpoint | `--mcp-config` with `--strict-mcp-config` | MCP config |
| Context | The external harness compacts; harness logs `compaction.applied` with `by_backend` | `/compact` | native |
| Telemetry | Usage, cost, and context window arrive through `Sink.Telemetry` | `result` event | `thread/tokenUsage/updated` |

`internal/backend/external` holds what every adapter shares: process supervision, the line-protocol transport, the mirror writer, and the MCP bridge. `internal/backend/claudecode`, and `internal/backend/codexcli` in phase 5, hold only the mapping in the table.

- History bridge. A session may move between a backend that owns its loop and any other backend. A stream-json input cannot seed prior history, so each turn of such a backend has the harness tool `get_conversation_history`, which reads the live history page by page and which no `AllowedTools` list hides. When another provider recorded a message after the newest message that this backend saw, the backend adds one line to its single `--append-system-prompt` value that tells the CLI to call the tool before it answers. A turn belongs to the provider of its model at its start, even when a settings change moves the model in the middle of it. A turn that recorded no item showed its backend nothing. A turn after a turn of the same provider adds none, and nothing is stored.
- Subagent frames. Claude Code runs with `--forward-subagent-text`. The text, the tool calls, and the tool results of a subagent become items, and `Message.ParentCallID` names the tool call that started the subagent, so a client nests them. History hands the model the parts alone.
- Questions. A defer hook parks each `AskUserQuestion` call and the CLI answers its result with `stop_reason: tool_deferred`; the backend opens the request. An answer with no input answers the call over the control channel, with the answers added to the tool input, and the result of the CLI for that call is the result item. Any other resolution denies the call with an interrupt in a run of its own, which records nothing, before the prompt runs. A run that answers a question takes no steer input, as the engine took no injection on it: the CLI would queue the input behind its own continuation and answer it in a second result, so the input waits for the next turn.

### Warm-up

Warm-up is built, as the port of Codex prewarm. `turn` declares an optional `Warmer` interface: `Warm(ctx context.Context, req Request) error`. The session calls it once on create and on wake, fire-and-forget under the session context. Only `internal/backend/modelapi` implements it, through an optional `Warm` method of the client, for the Codex websocket transport. The first turn waits for an in-flight warm-up, as the old engine's first prompt does. No warm-up state lives on the session.

## tool, prompt, and config

### tool

There is no tool registry. The tools of a runtime are a `[]turn.Tool`. The runtime joins them with the tools that can change between model calls into one `turn.Source` for each session.

```go
package turn

type Tool interface { // the same method set as harness.Tool
	Spec() protocol.ToolSpec
	Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error)
}

// Restrict returns the tools that names lists, or every tool when names is nil.
func Restrict(tools []Tool, names []string) []Tool

type Source interface {
	Toolset(ctx context.Context, history []eventlog.Message, allowed []string, model string) Toolset
}

type Toolset struct {
	Tools    []Tool // described to the model
	Deferred []Tool // not described; the model may call them
	Prompt   string // follows the system prompt
	Hooks    Hooks  // run around each tool call; nil: none
}
```

- `New` builds the tool list from `Options.Tools`, the `goal` tool with `goal_evaluator_model`, and, with a `WorkDir`, the `process` and `task` tools. An empty or repeated name fails `New`. The built-in tools of a `WorkDir` belong to each session; see "Built-in tools".
- The runtime builds one `turn.Source` for each session when it loads the session. In order, it gives the runtime tools, with `goal` and `task` bound to the session ID and no `goal` for a child; `goal` only for a backend that does not own the loop; the file tools of the `WorkDir`, for a backend that does not own the loop; `internal/tool/mcpsrc`, except for a backend with `OwnsMCP` on a turn with no `AllowedTools`; and `internal/tool/pluginsrc`. The actor adds a last `Source`: `get_conversation_history` for a backend that owns the loop, which no `AllowedTools` list hides, and `read_tool_result` with the retention hooks for a harness loop. `turn.Sources` joins the `Source`s, and `turn.Run` takes no other tool list. Each `Source` gets the `AllowedTools` of the session for each model call and applies `turn.Restrict` once.
- `turn.Sources` chains the `turn.Hooks` of its `Source`s in order: `Before` runs in order and a deny ends the chain, and `After` runs in order.
- The loop runs the tool calls of a response one at a time, in order. A tool error, or a call to a tool that the model may not call, is an error result that the model sees.
- No tool receives a session. The `goal` and `task` tools hold the session ID that the runtime binds when the session starts.

Agent profiles name a kind of child, in the agent format of Claude Code so one file serves every backend: `name`, `description`, `tools` (comma separated; omitted allows every tool of the parent), `model` (a ref or an alias; omitted or `inherit` keeps the model of the parent), and a prompt body. `color` is read and ignored; any other key skips the file. `internal/prompt.Profiles` reads `*.md` in each of `agent_defs_dirs` (default `<WorkDir>/.agents`; a relative path joins `WorkDir`; a name that two files repeat fails the load) at each spawn, beside the built-in profiles, which a file of the same name replaces. The built-in `general-purpose` allows every tool of the parent. The built-in `explore` and `plan` allow only the read-only file tools, and `plan` asks for an implementation plan. `session_info` joins the read-only set at the switch (see Decided); it is not built yet. Tool names differ by backend, so a spawn keeps only the names of a profile that `known` accepts for the model of the child: a runtime tool, a plugin tool, an MCP tool, or a built-in tool of its model. A profile left with no tool logs a WARN line. A file that is not valid is skipped with a WARN log line. A name that two files repeat, in one directory or across `agent_defs_dirs`, fails the load, and the error names both files, with no `engine:` prefix: the spawn fails its tool call. A session that exists opens as before, with the profile of the first file of that name. A profile applies through the allowed tools of the child (`turn.Restrict`) and its body as the last segment of the system prompt of the child. The runtime reads the profile once, when it starts the child, and the spawn uses that same read for the model and the tools of the child.

### Built-in tools

With a `WorkDir`, each session of the harness loop gets the built-in tools of the engine, with their behavior, limits, and text: `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `ls`, `bash`, `read_tool_result`, and `session_info`. An embedder tool or a plugin tool with one of these names fails, and so does an embedder tool named `model` with a `WorkDir`, whether or not `model_tool` is false. With a WorkDir and `model_tool` not false (default true), each session also has the `model` tool, as the engine had it: `status` reports the model, the aliases, and the providers, `list` reports the providers and the aliases alone, and `set` takes a ref or one alias and calls `Session.Update`, so the model changes at the next model call. The providers are the registry of the engine: the native `anthropic` and `openai`, the default `openrouter`, and each configured entry. A native provider needs no entry, and a `set` to it builds its client as the engine did, with the key of its default environment variable. An unserved provider fails with the valid aliases and providers listed, and a session has no action that clears its model. Each provider has a `billing` of `subscription` for `claude-code` and `codex`, and `api` for any other. A backend that owns its loop gets the `list` action alone, because `set` would move the model of the turn that calls it. `session_info` takes no argument and reports the model, the effort, the service tier, and the usage of its session from the view; the system prompt that the newest turn sent, with the MCP and plugin segments of its newest model call; the tool names of that call; the instructions files and the skills that the session read when it started; and the plugin inventory. It reports what the session loaded and sent, so a file that changes later does not change the report. Without a `WorkDir`, a session has none of them, and no result is retained. Switch oracle: the `file_tools_*` rows; `internal/tool/builtin` runs their calls with the texts of their goldens.

- The runtime gives each session its own file tools in its `Source`, because the `write_file` guard belongs to one session: `write_file` overwrites only a file that the session read or wrote, with no change on disk since. As in the engine, the guard is in memory, so after `Open` the model reads a file again before it overwrites it. The tools cannot be one `Options.Tools` list for the runtime.
- The actor builds `read_tool_result` over itself, so the tool gets its dependencies when it is built. `session.Config.Retain` turns this on, and the runtime sets it with a `WorkDir`.
- A backend that owns the loop, such as Claude Code, gets none of the others and has no retention. It has its own tools.
- One file-size cap of 20 MiB replaces the read budget of the engine. `read_file`, `edit_file`, and the `write_file` guard read at most that many bytes of a file and fail on a larger one. Tools run one at a time, so the cap bounds the memory of the reads.
- `read_file` reads an image as one summary line. `bash` gets no `shell.env` additions, because the runtime does not dispatch that hook.

### Tool-result retention

`internal/toolresult` holds the retention of the engine and `read_tool_result`, with its limits and text.

- A tool result above 16384 bytes has its secrets masked, goes to a blob, and the history holds a header with a `trh_N` handle and the first 16384 bytes. A result that fits after the mask stays inline. `read_tool_result` reads the blob back by line window or literal search, bounded by `max_bytes`. Its own result is never retained.
- The `Source` that the actor adds to an agent turn has a `turn.Hooks` that retains each result after the plugin hooks, so the blob holds the text that the model would see. The turn records the preview, and the next model call of the turn never carries the whole result. The blob is written outside the actor, and the record is appended only while the turn runs.
- A `tool_result.retained` record names the blob, and `Sync` carries the blob with the record, as for `backend.state`. The handle numbers count these records, so a replay and the next owner continue the count. The blob key is the handle and the fence seq, so a fenced owner never overwrites the blob of the next owner.
- A turn whose allowed tools omit `read_tool_result` retains nothing, so a preview never names a tool that the model cannot call.
- A result that would take the retained total of the session above 4 MiB keeps its preview with a notice and no handle, and nothing is written. A failed write keeps the whole result.
- The result of a done child in a report is retained the same way when it is larger than 4096 bytes once masked: the preview is 4096 bytes, the tool name is `task`, and the record joins the append of `child.settled`, so a handle always names a blob that exists. The handle takes the next number at that append, and a result of the session that is retained at the same time takes the next free number. The blob key is `report-<child turn id>-<fence seq>`, because the handle has no number before the append.
- Each compaction summary ends with an index of the newest 32 retained results, so a handle stays reachable after its preview folds.

### MCP tools

`internal/tool/mcpsrc` gives the tools of `Config.MCPServers` to each model call. The `mcp` package does not change. `turn` declares the one seam: a `Source` returns a `Toolset` of described tools, deferred tools, and a prompt segment. The turn reads it before each model call, so a tool that the model loads in a turn is callable on the next request of that turn. A deferred tool runs when the model calls it. A backend that owns the loop gets every tool described. A backend with `OwnsMCP` gets no MCP tool on a turn with no `AllowedTools`: the runtime asks no server, and the backend gives every server to the external harness. With `AllowedTools`, the allowed MCP tools reach it through the harness-hosted MCP endpoint.

- Connection. The first model call of a session that the runtime serves MCP tools for connects every configured server, in parallel, once for each runtime. An unrestricted turn of an `OwnsMCP` backend connects none: the external harness connects them. `connect_timeout_s` (default 15) bounds each attempt. A server that fails stays down, with no background retry, until `mcp(action="connect")` makes one more attempt. A connected server is never dialed again. `Runtime.Close` closes every connection and stops each stdio server.
- Names. Each tool is `mcp__<server>__<tool>`. With `mcp_servers`, an embedder tool named `mcp`, `list_mcp_resources`, `read_mcp_resource`, or `mcp__…` fails `New`. `AllowedTools` restricts these tools and the deferred list, and a backend that owns the loop accepts these names. MCP needs no `WorkDir`: the embedder asks for it in its config.
- Deferral. The mode of a server is its `tool_loading`, else `mcp_tool_loading`, else `eager`. `lazy` defers each tool of the server. `auto` defers when the connected servers have more than `mcp_tool_loading_threshold` tools (default 20). The prompt segment lists each deferred tool with the first line of its description. As with Claude Code's ToolSearch, the model loads a tool with `mcp(action="select")` and finds one with `mcp(action="search")`. When a server can defer, the `mcp` tool has these actions.
- Replay. A tool is loaded when the history holds a `select` that names it or a call of it. The log holds both, so a replay or the next owner loads the same tools with no new event kind. A compaction that folds the call defers the tool again.
- Instructions. The segment starts with `<mcp_instructions>`: the instructions of each server that the first connect reached, with its allowed tool names, and one line about the resource tools. It never changes after the first connect, so a server that connects later never joins it.
- Resources. When a connected server serves resources, `list_mcp_resources` and `read_mcp_resource` are added. A listing stops at 500 resources for each server.
- Results. A result is text. An image, an audio item, or a binary resource becomes one line with its size and media type. An error that is not the server's own RPC error names a reason, never the endpoint URL or a response body.
- The segment follows the process status line. There is no `status` action, no MCP status segment, and no background retry. Switch oracle: the `mcp_*` rows except `mcp_status_*`. By design, an error has no `engine:` prefix, a call to a tool that is not there reads `no such tool available`, binary content becomes text, and `connect` is the only action of an eager runtime.

### plugins

`internal/tool/pluginsrc` connects `Config.Plugins` to each session. The `plugin` package does not change.

- Start. The first `Create` or `Open` of a runtime reads the manifest of each plugin with one probe, bounded at 30 s. A probe that fails, a manifest whose name is not the config name, or a plugin tool with the name of another tool or of a built-in tool of the model backend fails that call. The next call tries again. The probe is work that `Runtime.Close` waits for, and a call after `Close` returns `ErrDraining` before any probe. A plugin process starts on its first hook or tool call and stays warm. `Runtime.Close` stops it. There is no manifest cache. The phase 4 switch can add the cache of `cmd/harness` when a measurement needs it.
- Tools. Each manifest tool is a tool that runs through `tool/execute`. `AllowedTools` restricts them, and a backend that owns the loop accepts their names. A plugin needs no `WorkDir`.
- Hooks. A `turn.Toolset` carries `turn.Hooks`, and `runTool` runs each tool call between `Before` and `After`. This covers every tool: embedder, process, MCP, and plugin tools, and each call that a backend that owns the loop makes through `Request.Call`. A call that an `OwnsMCP` backend makes to a server that it connects itself runs no hook. `tool.execute.before` rewrites the arguments or denies the call. A denied call does not run, and its deny is the error result. `tool.execute.after` rewrites the result text. The log records the call as the model made it.
- Prompt. Each model call runs `system.transform` with the model of that call, and adds its segments after the MCP segment. A backend that owns the loop reads no system prompt of the harness, so its calls run no `system.transform`; its tool calls still run the hooks.
- Events. `session.Config.Appended` gives each appended event, and the state after it, to the runtime, which gives the events to the plugins of the session. `turn.started` and `turn.resumed` send `session.status` busy. `turn.suspended` and `turn.ended` send idle, and a failed turn first sends `session.error`. The tool hooks send `tool.execute.start` and `tool.execute.end` around each call that runs, and `file.edited`, with an absolute path, after a `write_file` or `edit_file` call that succeeds. Delivery is best effort, as in the engine.
- Client API. `client/session.messages` reads the history of the session from its actor when this runtime runs it, and from the store otherwise. The history carries each attachment as a `blob` part with its bytes, read from the store by `blob_key`; a failed read fails the call. `client/mcp.call` and `client/generate` fail.
- The runtime does not dispatch `chat.params`, `chat.message`, or `shell.env`. No contract row pins them, and no boxes plugin uses them.
- Switch oracle: the `plugin_*` rows. `serve_url` and `run_token` are `Options.ServeURL` and `Options.RunToken`, empty when the embedder sets none.

### prompt

`internal/prompt.Build(cfg, workDir)` returns the system prompt of a session as segments. The runtime joins them with a blank line. It reads them once, when the session is created or opened, and sends them as `turn.Request.Instructions` on each model call. The log never holds them, so the next `Open` reads the files again. Codex and Claude Code also read the prompt once at session start.

With `Options.WorkDir` empty, the prompt is `append_system_prompt` alone, and no file is read. An embedder in the boxes control plane never gets the `AGENTS.md` of its process directory.

With a `WorkDir`, the segments are, in order:

1. The base prompt of a coding agent. It ends with the working directory.
2. The `append_system_prompt` entries.
3. The `AGENTS.md` chain. Each directory from the git root down to `WorkDir` gives its `AGENTS.md`, or else its `AGENT.md`. Outside a repository, only `WorkDir` counts. `instructions: false` turns the chain off, and `instructions_path` replaces it with one file. Each file is cut at `instructions_max_bytes` (default 64 KiB), and a marker names the file and the byte counts. A negative value keeps the whole file.
4. The skill list: each valid `SKILL.md` below `skills_dirs` (default `.agents/skills`), sorted by name, with its path.

A file that cannot be read, is empty, or is not UTF-8 is skipped, and so is a skill that is not valid or repeats a name. The session starts without it. Each skip and each cut writes a WARN log line.

A backend that owns the loop ignores `Instructions` and builds its own prompt. The runtime still reads the prompt when such a session starts, and the backend does not use it. Claude Code gets `append_system_prompt` as one `--append-system-prompt` value, and the CLI runs in `WorkDir`.

Each turn sends the prompt with one process status line after it, built when the turn starts. See "processes". Each model call then adds the MCP segment, then the plugin segments. See "MCP tools" and "plugins". There is no outline mode, no chain ceiling, and no other ambient segment than the engine banner. With `Options.Version`, each model call of a harness-loop turn sends `[engine: harness <version> · session_sync=<mode> · engine started <time>]` in `<harness-engine-context>` tags after the history message that was newest when the first request of the session left. Each request is a prefix of the next, also after a compaction in the middle of a turn, and a compaction can only move the place earlier. A backend that owns its loop gets no banner. The base prompt says that the status follows the system prompt. The engine keeps its own sentence, about the newest user message, through `prompt.EngineBase` until phase 6. Tools run one at a time, so the tool-batching segment is gone. At the switch, the `runtime_prompt` contract rows change in three ways: the `instructions_mode` and outline rows go, a bad file degrades instead of failing the turn, and no batching segment follows the base prompt.

### processes

With a `WorkDir`, the runtime builds one `process.Manager` from `Config.Processes` and adds the `process` tool over that manager. Without a `WorkDir`, no process runs and the model sees no `process` tool. With a `WorkDir`, an embedder tool named `process` fails `New`.

`declare` runs any argv that the model names. A `WorkDir` therefore grants command execution equal to `bash`, even when `Options.Tools` has no shell tool. An embedder that wants only AGENTS.md discovery must not set `WorkDir`.

- The tool description names the configured processes only, so it never changes while the runtime runs. `declare` adds a process in memory until the runtime closes.
- A tool error is the error of the manager, with one `process:` prefix.
- A result reports no elapsed time. The status line names instants instead.
- When a turn starts, the session appends one status line to its system prompt: `[processes: dev ready :3000 since <RFC 3339> log=.harness/proc/dev.log]`, one entry for each process that has started. The line is inside `<harness-engine-context>` tags, so the base prompt marks it as trusted. An instant changes only when a process changes state, so the line is stable for the turn and for each later turn with no process change. The log never holds it.
- This deviates from the engine, which puts the status at the end of the newest user message. Each process change (a start, a restart, ready, a stop, or an exit) changes the system prompt of the next turn. That turn misses the prompt cache for the whole history, and on the OpenAI WebSocket path it sends the full input instead of a suffix. A process that exits during a turn shows in the next turn. The cost is one cache miss for each process change, which keeps one prompt for each turn.
- `Runtime.Close` stops every process after the sessions end. When its ctx ends first, it cancels the turns, waits for them to end, and kills the processes before it returns.
- The process tool and the `/processes` routes share the one manager. A `start`, `stop`, or `restart` route is work that `Runtime.Close` waits for, and fails with `draining` (503) after `Close` starts, so no route starts a process that `Close` does not stop. The runtime does not expose the manager. `GET /processes` lists every process with its definition and status, and is `[]` without a `WorkDir`. `start`, `stop`, and `restart` reply with the status. `GET /processes/{name}/logs` replies with `content`, the last `tail` lines (default 50), and `status`. An unknown name, or any name without a `WorkDir`, is `process_not_found`. Any other error of the manager, such as a failed start, is `internal` with the fixed message. Switch oracle: `process_http_lifecycle`, `process_http_unknown_name_is_404`, and `process_tool_from_the_model`. Its two double-prefix rows change by design.

### workspace

`GET /workspace/changes?scope=&dir=` diffs the git work tree with the rules of the engine route `GET /git/changes`. The route exists only with a `WorkDir`.

- `scope` is `branch` (default), against the merge base with the first of `origin/HEAD`, `origin/main`, and `origin/master` that resolves, or `uncommitted`, against `HEAD`. An unborn `HEAD` diffs against the empty tree.
- `dir` defaults to the `WorkDir`. Any other `dir`, relative to the `WorkDir`, and its repository must stay under the `WorkDir` after symlinks.
- Untracked files count as added, through a private copy of the index and a private object directory. The request never writes the real index or object store, never runs a filter driver, hook, external diff, or textconv, and ignores the `GIT_*` variables that select another repository.
- `files` is complete. An untracked file above 2 MiB is `large` with no hunk. `patch` ends at the last whole file within 1 MiB and sets `truncated`. The patch is not HTML-escaped.
- One request has a 28 s budget. A request that passes it, or whose file list passes 32 MiB, fails with `too_many_changes`. Errors: `invalid_request`, `not_a_git_repo`, and `no_base`.
- Switch oracle: the `git_changes_*` rows, with the route renamed and the error body in the new shape.

### config

One `Config` struct. `Defaults` is the one defaults table, and each accessor reads it for an unset key. `Validate` is the one rule set. `LoadProject` runs it on the merged config, and `New` runs it on `Options.Config`. It never changes the config. `ProcessSpec.Validate` is the per-entry rule that `process.Declare` also uses.

`New` also refuses a config that sets a key which the runtime does not read, with `ErrInvalidRequest` that names the key: `instructions_mode`, `event_sink`, `snapshot_every_records`, `tool_result_inline_bytes`, and `tool_result_retained_bytes`. `session_dir` stays for `cmd/harness`, and `session_sync` sets the engine banner. The switch stops reading these keys and changes boxinit in the same release, and phase 6 deletes them; `model_tool` turns the `model` tool off, and `New` reads it.

`owner_epoch` (a number) and `sync {url, token_file}` are the keys of a box harness; `Validate` requires an `http` or `https` URL with no userinfo, and a token file. Only the user file sets them: a project file cannot. A project file is still read as a file, so an invalid `sync` there fails the load, and a valid one is dropped.

`ApplyEnv` sets each top-level string, number, or bool key from `HARNESS_<KEY>`. An empty variable keeps the key. A map, slice, or struct key has no variable. A parse error names the variable and never the value. There are no env-only knobs. `cmd/harness` applies it in `serve` and `run`, and a flag wins over a variable.

The package imports only the standard library. The `config-leaf` depguard rule enforces it.

## Feature disposition

| Feature | Verdict |
| --- | --- |
| Goals | Keep; one state machine. Delete deferred goals and parking |
| Queue, `prompt_async`, `enqueue`, `send` | Merge into inputs |
| Task notifications | Merge into inputs with `source: child` |
| Structured questions (#317) | Generalize to requests |
| Claude Code delegated backend | Keep; first third-party harness |
| Codex CLI | Add through the third-party harness seam |
| Codex lane, websocket transport | Keep inside `provider/openai` |
| Startup prewarm | Move into `modelapi` behind `turn.Warmer` |
| Agent definitions | Keep as agent profiles applied at `Spawn` |
| Git changes | Keep as `GET /workspace/changes` in `internal/workspace` |
| Tool-result retention, `read_tool_result` | Keep; drop the size knobs; port in phase 3 |
| Read budget | Replace with a file-size cap |
| Snapshots, index | Replace with `Apply` over the log |
| Box-global `events.jsonl` | Delete |
| Worktrees, `workdir_isolation`, worktree sweep | Delete |
| Modal guide and `scripts/modal-e2e.py` | Delete |
| `/debug/pprof`, `/debug/goroutines` | Delete |
| `cancel_tree` | Merged into `interrupt {tree}` |
| `HARNESS_SEQUENTIAL_TOOLS`, read-budget and tuning knobs | Delete |
| `/wait`, `/request`, `/session/status`, `/event/tip` | Delete; the new API covers them |

Firm deletions remove about 1.2k–1.5k lines. The persistence and provider duplication removes about 2k–4k more.

## Tests and guardrails

### Contract suite

- `harnesstest` is a public package. It serves the Anthropic Messages API (`New`) or the OpenAI chat-completions API (`NewChat`, as a gateway such as Bifrost) from a script: text, tool call, error status with a message and `Retry-After`, a context-overflow error, `max_tokens`, usage values, and block until released or the client cancels.
- A scenario is a model script and a list of actions. Its golden holds the normalized model requests and the final transcript of each session, not event shapes, so the goldens survive the event-log change in phase 4.
- Scenarios run the real `harness serve` binary over loopback, in its own process group, through a driver interface. Each wait has a bound.
- Every scenario checks the invariants that hold in both formats: unique message ids, one result for each tool call, and contiguous event sequence numbers.
- The suite exists before any internal change. It pins today's behavior, so each later phase is checked against it.
- An action that returns a result records its status and body in the golden under `calls`, keyed `<action>.<alias>`, with `#2`, `#3` on a repeat. Ids in a body become `ses:<alias>`, `msg#N`, and so on. A scenario binds each child with `bindChild` before it lists sessions, so no id gets a run-dependent number.
- `scenario.config` adds top-level keys to the served config, for example `context_window_tokens` or `compaction_keep_turns`.
- `harnesstest.NewOpenAI` serves the ChatGPT Codex Responses wire over SSE and websocket from the same `Step` script. It answers a websocket prewarm without consuming a step, rejects a `previous_response_id` that its connection never completed, and can report `x-codex-*` usage headers or a `codex.rate_limits` frame, drop a response mid-turn, or refuse the websocket upgrade. `WireEvents` lists each dial, prewarm, and request. The Codex scenarios record it under `calls.codex_wire`.
- `scenario.chat` serves the model through `NewChat` and points the config at it as provider `bifrost`.
- The runtime host is the oracle for the new runtime. With `HARNESS_E2E_RUNTIME=1`, each row also runs on `harness.Runtime` in process, through `Runtime.Handler`, on a `DiskStore`, with the config that serve reads. The runtime mints each session ID. A restart closes the runtime and opens a new one on the same store. A kill copies the store, closes the old runtime on its own directory under an ended context, and opens the new runtime on the copy, so nothing that the old runtime does after the kill reaches the next owner. The driver reads the transcript from the session events, as a reader sees it, with no pinned segment, and maps each old route to its new route. It never calls a route that this spec deletes; the call records `deleted_by_design`. A wait for an idle child also waits until its parent records `child.settled` and is idle, because the report of the child can start a turn of the parent.
- Each golden has one disposition in `e2e/runtime_rows_test.go`: the same golden; a runtime golden under `e2e/testdata/runtime`; deleted by a cited line of this spec; or pending on the findings, phases, or lines of this spec that it waits for. A runtime golden cites each line of this spec that decides one of its differences, and each finding that owns another. Every break that no decision covers is a line under Open questions, which a row cites like any other line. `F01` to `F24` are the findings of the re-architecture review, and `TestRuntimeRows` accepts no other finding ID. The same and re-golden rows gate the runtime host. A pending row runs in a child test process as an expected failure: the run fails when a pending row matches its serve golden, so it must become a same row. A pending row with a by-design difference never matches serve, so a fix gives it no signal: its owner moves it to a re-golden row in the PR that lands the fix. `TestRuntimeRows` checks that each golden has one disposition and that each citation holds. A same row compares with its serve golden. CI runs the runtime host in a step that gates.
- `HARNESS_E2E_COVER=1 go test -race ./e2e/ -run TestContract` builds an instrumented binary, runs the contract scenarios, and prints the statement coverage by package from `TestMain`. It appends a Markdown table to `$GITHUB_STEP_SUMMARY` when that variable is set. Test cleanup sends SIGTERM before SIGKILL so a serve process flushes its counters. CI runs the command without gating.

Today ~70% of 127k test lines read unexported state and will not survive the restructure. The target is 40k–50k test lines.

### Unit tests

Unit tests cover pure code only: `Apply`, wire transcoders (`provider/*/` and the `internal/backend` wire mapping files), `config`, `message`. TDD means writing the failing contract row in `e2e/` first, seeing it fail for the named reason, then implementing. A file outside the contract suite and the pure-code packages may add test lines only when `testdata/test-exceptions.txt` lists it with a reason. A bug fix adds a table row, not a file. No test reads unexported state across packages.

### CI gates

| Gate | Threshold |
| --- | --- |
| Whole-line comments per file | Warn above 15%; fail above 25%. The `doc.go` package comment does not count |
| History markers in comments | Fail on issue numbers, dates, "previously", "no longer", "red-verified", "confirmed live", "an earlier version", "before this change", "round N", review or fix rounds, and "copilot" |
| File size | Fail above 800 lines |
| Function size | Fail when the closing brace is more than 80 lines below the opening brace |
| Test lines vs code lines per package | Fail above 1.5 test lines per code line, unless the ratio does not rise over the merge base. A new package has no base, but a moved package compares with the package it came from. A change that removes code and adds no test lines always passes |
| Test lines outside the contract suite | Fail when a changed `_test.go` file adds test lines (code lines inside `Test`, `Benchmark`, `Fuzz`, and `Example` functions and package-level `var` declarations; top-level helpers, fakes, and types do not count) outside `e2e/`, `internal/eventlog`, `provider/*/`, `config`, `message`, `internal/gates`, and the wire mapping test files `internal/backend/modelapi/convert_test.go` and `internal/backend/claudecode/frames_test.go`, unless `testdata/test-exceptions.txt` lists the file with a reason. The count is net per file, so a change that deletes and adds the same number of test lines passes. A change that deletes test lines always passes. A listed file still meets the test:code ratio row |
| `time.Sleep`, `time.After` in tests | Fail in a test file. `internal/testpoll` is not a test file |
| `AGENTS.md` length | Fail above 80 lines at the root and 25 lines in a scoped file |
| Merge-base diff | `TestRepository` checks only files and packages that differ from `git merge-base HEAD origin/main`, or from `$GATES_BASE_REF`. A new file meets each limit above. A changed file may not cross a limit that it met, and may not get worse on a limit that it already broke. A renamed file compares with its old path. A change that only deletes code always passes. An unchanged file is not checked. No baseline file exists |
| Imports | `depguard`: internal packages never import `server` or `cmd` |
| Lint | `govet`, `staticcheck`, `errcheck`, `unused`, `revive` |

Gates compare a branch with its merge base, so old code never blocks a change and new code starts strict. The protocol drift gate regenerates `protocol/` and fails on a diff; it is the Protocol drift step of CI. `AGENTS.md` shrinks to these gates and the four rules.

## Boxes integration

Boxes has its own re-architecture ("Boxes architecture") built on this one. The two share one pattern: a resource is one ordered log with one owner, every change is a command, every seam belongs to its consumer, and the contract is generated from Go types.

```
                 web · boxctl · Slack · MCP
                          │ boxes protocol (generated)
   ┌──────────────────────▼───────────────────────────────────────┐
   │ boxes control plane                                          │
   │  server ── box (Decide/Admit) ── lifecycle ── capacity        │
   │    │ /v1/boxes/{id}/sessions/…  (harness protocol, 1:1)       │
   │  SessionHost ─┬─ box  → reverse proxy to the harness in a box │
   │               └─ home → harness.Runtime in-process (lease)     │
   │  pgstore  =  harness.Store  ◀── home: direct · box: Sync ──┐   │
   └─────────────────────────────────────────────────────────────┼─┘
                          │ BootConfig{epoch, harness/config}    │
               ┌──────────▼──────────────────────────┐           │
               │ box: boxinit → harness serve         │ SyncBatch │
               │      DiskStore · Owner(epoch) ───────┼───────────┘
               └──────────────────────────────────────┘
```

- **Two session hosts.** A box session runs in the box's harness. A home session runs in an embedded `harness.Runtime` inside the control plane, under a lease. Boxes forwards `/v1/boxes/{id}/sessions/…` to one `SessionHost` seam: a reverse proxy to the box, or the local `Runtime.Handler()`, or a proxy to the replica that holds the lease. The console cannot tell them apart.
- **One store for every session.** `pgstore` is one Postgres `harness.Store`, proven by `storetest`. Home sessions write it directly. Box sessions replicate into it through `Sync`. `OpenView` over it serves reads of a box that is not running.
- **One cutover.** Phase 4 and the boxes session cutover are one quiesced cutover: no per-box canary, no `harness_api` column, and no dual stack. A rehearsal on a copy of production data and a tested rollback come first. The `box_journal_*` tables drop one release after the cutover.
- **One pinned commit.** Boxes builds each box image from an exact harness commit that it pins (`ARG HARNESS_REF`), never from harness `main` at its head. Harness reaches boxes only through an explicit bump PR on boxes that moves that pin. The pin PR merges on boxes before the harness phase 4 switch merges, and the switch reaches boxes only through the bump in the cutover release. A harness change that merges to `main` therefore ships to no box until a bump.
- **One epoch.** The box `claim_epoch` is the `Ownership.Epoch`. It is recorded in `owner.acquired`, carried by every `SyncBatch`, and checked by boxes. A stale box learns it is fenced from the rejection.

| Boxes requirement | Harness answer | Phase |
| --- | --- | --- |
| Read-only open | `OpenView` | 2 |
| Public scripted model | `harness/harnesstest` | 1 |
| Pump contract | `Sync` and `protocol.SyncBatch` | 2 |
| `config.Config` without server dependencies | `harness/config` | 3 |
| Durable head per session | `protocol.Session.HeadSeq` from `Session.View()` or `View.Session()` (Apply runs after a durable append) | 2 |
| Open after a forced stop | the `crashed` cause | 2 |
| `Models()` with no sessions | `New` does no I/O | 2 |
| External lease that fences a stale epoch | `Ownership.Epoch`; `ErrStaleEpoch` stops the session and releases its `Ownership` | 2 |

Combined sequence:

| Harness | Boxes |
| --- | --- |
| 1 contract suite, gates, `harnesstest` | 0 defect fixes · 1 protocol, contract suite, gates · 2+3 commands and lifecycle · 4 capacity (none need harness) |
| 2 runtime core | Home chat on `pgstore` and `SessionHost` (meta) |
| 3 leaves, `harness/config` | 5 `BootConfig` embeds `harness/config` |
| 4 HTTP cutover, journal migration | 7 session cutover with the read-only view, same release |
| 5 remaining backends, `codexcli` | — |
| 6 delete `engine`, `server`, the migration tool | — |

Answered boxes requests: `Runtime.Close` and `Session.Release` return only after `Sync` acknowledges every record through the handoff, and `View` reports `SyncedSeq`; `Sync.Deliver` returns `SyncAck{head}` on every reply; the epoch is a monotonic number (`claim_epoch`). The home chat is one per person.

## Migration

Each phase is one or more PRs on `main`. Each ships alone.

| Phase | Work | Consumers |
| --- | --- | --- |
| 1 | Contract suite: scenario scripts and `harnesstest`; CI gates that diff against the merge base; new `AGENTS.md` | Boxes contract suite reuses `harnesstest` |
| 2 | New runtime core beside the old engine, in the order meta needs it: `harness.Store` and `storetest`; `Owner` with `Epoch`; `Runtime`, `Session.Submit`, `Events`, `OpenView`; `Sync` and `SyncBatch`; handoff and crash causes; a native backend with `ModelTransport` (Codex first); `harness.Tool` and `Restrict`; the `external` adapter and `claudecode`. Absorbs the design of PR #359, its conformance suite, and its `fakeclaude` modes. | The meta home chat embeds it on `pgstore`; it is the first consumer |
| 3 | `harness/config` with `Defaults`, `Validate`, and `ApplyEnv`, on the standard library only; one `modelapi` backend for every model API wire; provider error classes, the stall watchdog, and max_tokens continuation in `turn`; goals as one state machine in `session`; the built-in tools, and large-result retention and `read_tool_result` in `internal/toolresult`; children, agent profiles, and the `task` tool | Boxes `BootConfig` |
| 4 | New HTTP and `protocol` generation. Scenario scripts carry over; their assertions move to the new API. One PR switches `cmd/harness`. A one-time tool converts every old session journal, including the journals in archived boxes, to the event log in the quiesced window, before the new harness starts; see "Old-format migration". Merged before the switch: `Runtime.Handler`, the box routes and slash commands, the MCP tools, the plugins, the processes and the `process` tool, the prompt builder, the workspace route, and `harness/migrate` with `cmd/harness-migrate`. Also merged before the switch: requests with `Resolve`, the engine banner, the plugin inventory, the crash marker, the MCP connect reason, the history bridge, the frames of a subagent, the settings change in the middle of a turn, the `model` tool with the `model` and `effort` of `task spawn`, and `Runtime.End` with `DELETE /sessions/{id}`. The rows of "Deliberate parity breaks" that are open wait for a decision. | Boxes console adopts the harness shapes; boxes routes become thin forwarders. Same release. |
| 5 | Remaining backends on capabilities; `codexcli` with its approvals | None |
| 6 | Delete `engine`, `server`, the migration tool, dead features; move leaves to `internal/` | None |

PR #359 closes unmerged; its design is in this doc. The meta home chat has no old data or routes, so it proves the new runtime before boxes switches. Phase 4 is a cutover, not an adapter: no old route or Go API survives it, and the runtime reads no old format. Only the migration tool reads the old journals, and phase 6 deletes it after it has converted live and archived boxes.

## Open questions

- Where does a pinned segment sit after a compaction, and does it survive a restart? The engine kept each pin as a slot number, the smaller of that slot and the length of the history, in memory only. The runtime fixes a pin to the messages around it and rebuilds it from the log. Three cases differ. (1) A pin after the cut of a compaction: the engine puts it in the message of the next input, after the last message of the turn that held the report; the runtime keeps it after the message that it followed. (2) A pin that a compaction folds, with a kept tail of several calls: the engine puts it at its slot inside the tail, between a tool call and its result; the runtime puts it at the end of the history, after the next input. (3) A restart after a report: the engine lost the segment, and the runtime still reads it. The engine result of cases 2 and 3 splits a tool call from its result and loses a report, so the runtime does not copy it. Andy or a parity decision settles each case.

## Closed parity questions

Andy closed these on 2026-10-04 and 2026-10-05: the switch keeps each one, at parity with the engine. The coordinator closed the last eight (the context gauge and cost of a Claude Code turn, a failed Claude Code turn, the `[continuation: …]` tags, the creation order of `GET /sessions`, the receipt of the answer route, a repeated agent definition name, a settings change to another kind of backend in the middle of a turn, and the log parts of a child report to a busy parent) overnight on 2026-10-05 by the parity rule; Andy may veto them.

- Does a prompt that arrives while a turn runs join that turn? Yes, built: the default `delivery` is `steer`, so the prompt joins the turn at its next item boundary, as the engine delivered a queued prompt. A child report joins a busy parent the same way on a model API backend; on Claude Code it waits for the next turn (see Decided). The goal input and `/compact` stay `queue`.
- Does a child report to a busy parent keep the task notification of the engine? Yes, built for the line format, a done child with its text, and a canceled child: the model reads the report in the `[tasks: …]` segment of the engine, and not under the `OPERATOR MESSAGES` steer heading. The failed child, the exhausted child, the crashed child, the bound of the reason, and the long result have the text of the engine (see Settle). An idle parent on a model API backend starts a turn with the report as its text, as it did; on Claude Code the turn reads the trigger sentence of the engine and the `[tasks: …]` segment (see Decided).
- Does the log keep a `task_report` part and an `engine_context` part for a report that joins a busy parent? No, built: the segment is pinned for the model calls and kept out of history, as the engine pinned it (see Settle). The input holds the text and the `task_report` line, and the history has no message for the segment, so the `log` of `task`, `get_conversation_history`, and `client/session.messages` of a plugin show no segment.
- Does the switch port `session_info` and `model`? Yes, built: see "Built-in tools". The contract goldens list both in the tool list of each request.
- Does the switch keep the engine banner, `[engine: harness <version> · session_sync=… · engine started …]` in `<harness-engine-context>` tags? Yes, built: see "Prompt".
- Does the switch keep the plugin inventory? Yes, built: `protocol.Session.Plugins` lists each plugin with its hooks, tools, and state.
- Does the switch keep the crash marker? Yes, built: a crashed turn ends with the assistant message `[harness: this turn was interrupted by a process restart and could not complete]`.
- Does the switch keep the classified reason of a failed MCP connect? Yes, built: `connect` names `initialize timed out`, `initialize cancelled`, `connection refused`, `connection failed`, or `initialize failed`.
- Does the switch keep the cross-lane history bridge on a model switch and the frames of a subagent? Yes, built: see "Third-party harnesses".
- Does the switch port questions (`AskUserQuestion` and `Resolve`) and Codex prewarm? Yes, both before the phase 4 switch: see "Third-party harnesses" and "Warm-up".
- Does the first turn of a session wait for an in-flight Codex warm-up? Yes, as the first prompt of the engine does (see "Warm-up"). The wait handle is coordination, not session state.
- What does `DELETE /sessions/{id}` do? It matches the engine: `409 session_busy` while a turn or a control command runs, `404 session_not_found` for an unknown session, else it ends the session (a compaction or an evaluation that runs stops, as under `Release`) and answers `204`. It stops each live child, as `interrupt` with `tree` does, and unloads the session, as `Release` does. Only a child that this walk stopped loses its report: the walk marks the stop durably, as cause `ended` on the `turn.ended` of the child, so a parent that opens later drops only those reports. Every other child whose turn ended stopped, such as one that `interrupt` stopped before the `DELETE`, settles `canceled` and reports as "Children" says, as the engine did; the `DELETE` changes none of this. The log stays in the store, so a later `DELETE` of a stored session that is not loaded also answers `204`, and the session can open again. It is `Runtime.End` and an actor command.
- What page `limit` does `GET /sessions/{id}/messages` take? The engine page route limit: default 100 when `limit` is absent or 0, at most 1000. A larger, negative, non-integer, empty, or repeated `limit` or `before` fails with `invalid_request` (400), as the engine page route does; it does not clamp.
- When does the runtime contract step of CI gate? At the phase 4 switch, as "Contract suite" says.
- Does the switch wrap the messages that the engine writes for the model in `<harness-engine-context>` tags? Yes, built: the `[continuation: …]` message of a max_tokens turn is in the tags, as the engine sent it, and the log never holds it.
- Does `GET /sessions` keep creation order? Yes, built: it lists by the time of the first record of each session, and equal times order by ID. `after` is the ID of the last session of the page before it; an `after` that names no session fails with `invalid_request`.
- Does the answer route keep the receipt `202 {seq, status}`? Yes, built: an answer replies `202` with the `seq` of `request.resolved` and the `status` `started`. A dismissal has no engine route and replies `204`.
- Does a repeated agent definition name fail the load? Yes, built: see "tool, prompt, and config", the paragraph on agent profiles. The load fails and names both files, as the engine did.
- Does a settings change to a model of another kind of backend fail the running turn? Yes, built, as the engine failed it: the turn fails at its next model call, and the next turn uses the new model.
- Does the switch keep the context gauge and the session cost of a Claude Code turn? Yes, built: both come from the `result` frame, as in the engine; see Decided.
- Does a failed Claude Code turn run again? No, built: the CLI runs once, as in the engine, and the turn fails with the text of the `result` frame; see Decided.

## Deliberate parity breaks

Each row is a difference between the runtime and the engine that remains after the parity work. A decision of Andy decides a row, or a line under Open questions waits for one, and each line under Open questions has a row. A pending row waits for its line, so it names no re-golden. The contract rows name each row of `e2e/runtime_rows_test.go` that the difference re-goldens. The cross-lane history bridge, the frames of a subagent, and a settings change in the middle of a turn are kept (see Decided), so they are not rows.

| Difference | Engine | Runtime | Decision | Contract rows |
| --- | --- | --- | --- | --- |
| Claude Code `/compact` | A `/compact` message and `compaction.claude_code` with the tokens before and after | `compaction.applied` with `by_backend`, and no `/compact` message | Decided by the Compaction rules above | `claudecode_compact_delegated` |
| Cause of a child that `DELETE /sessions/{id}` stops | No `cause` field on a turn end | Cause `ended` on the `turn.ended` of the child | Decided by the cause list of State machines | `end_idle_parent_cancels_running_child` and `end_then_send_runs_no_report_of_the_stopped_child` |
| Place of a pinned segment after the cut of a compaction | The pin sits in the message of the next input | The pin keeps its place after the message that it followed | Open: see Open questions | `child_report_to_a_busy_parent_after_the_cut_of_a_compaction` |
| Place of a pinned segment that a compaction folds into a kept tail | The pin sits at its slot inside the tail, between a tool call and its result | The pin sits at the end of the history, after the next input | Open: see Open questions | `child_report_to_a_busy_parent_folded_with_a_long_kept_tail` |
| Pinned segment after a restart | The segment is gone | The segment is rebuilt from the log | Open: see Open questions | `child_report_to_a_busy_parent_survives_a_restart` |
| `event_sink` | The engine posts each journal record to the URL | `New` refuses the key; a box harness replicates through the config key `sync` (see Events) | Closed: boxes moves to `Sync` at the cutover (see Decided) | None: no row sets `event_sink` |

## Decided

- A Claude Code turn reads its context gauge and its cost from the `result` frame, as the engine did (2026-10-05). The gauge reading is the prompt size of the newest assistant frame of the main thread, or the usage of the `result` frame when no assistant frame carried usage, with the window of `modelUsage`. The `result` frame of each turn adds its `total_cost_usd` to `session_cost_usd` of `subscription_usage`, also when it fails the turn. A `result` frame sets the reading also when it carries no prompt tokens, so a compaction that the CLI ran reads 0. The window of a reading stays when a later reading of the same source reports none, as the engine kept the window that the CLI reported; a change of model clears it, and `context` in the session view shows an explicit reading of 0 tokens with its window. A failed Claude Code turn runs the CLI once and fails with the text of the `result` frame, or with the exit of the CLI when it wrote none; `prompt_retries` does not apply to it, and the error keeps its retryable class for a goal (2026-10-05).
- Mid-turn delivery is at parity with the single queue of the engine (2026-10-04). An input with no `delivery` is `steer`, and a prompt that arrives while a turn runs joins it at the next item boundary, on a model API backend and on Claude Code. A child report is a `steer` input on a model API backend and joins at the next item boundary. On Claude Code the engine checked out reports only when a turn started, so a child report to a busy parent is a `queue` input: it is not injected in the middle of the turn, and the next turn that starts reads it with every other queued report. When another input starts that turn, the reports are appended to the text of that input as `\n\n[tasks: …]`; when only reports start it, the model reads the trigger sentence of the engine and then `\n\n[tasks: …]` (2026-10-05). An internal goal input and `/compact` stay `queue`.
- The migration tool converts archived boxes in the cutover window, with live boxes. It reads `sessions.tar.zst` from each archive object and writes the converted event logs back into it. A restore after the cutover needs no converter, and phase 6 deletes the tool.
- Claude Code delegation is permanent, and the Codex CLI follows it through the same seam (see Third-party harnesses).
- `interrupt` stops the running turn only. The next queued input then starts, and an active goal keeps running, as in Claude Code. `interrupt` replies after the turn has stopped.
- Workspace inspection stays in harness as `GET /workspace/changes`, in an isolated `internal/workspace` package.
- Worktrees are deleted.
- Agent definitions stay as agent profiles in Claude Code's format.
- Startup prewarm moves into `internal/backend/modelapi` behind `turn.Warmer`.
- The Modal guide and script are deleted.
- PR #359 is folded into phase 2; the meta home chat is the first consumer of the new runtime.
- The boxes console adopts the harness API shapes; boxes routes forward.
- Embedders add in-process tools through `harness.Tool`; `call.ID` is stable across resume.
- Embedders inject model credentials through `Options.ModelTransport`.
- Every early stop keeps the partial; clearing a goal is a stop with cause `goal_cleared`.
- One `pgstore` backs home sessions and the box mirror; `OpenView` reads both.
- Handoff stops at an item boundary and resumes; a crash ends the turn, marks open tool calls cut off, and starts the next queued input or waits for input. No tool call is ever re-run.
- The scripted model is public as `harness/harnesstest`.
- No session history is lost at the cutover. A one-time tool converts every old journal to the event log and verifies each session; phase 6 deletes it.
- The rebuild keeps parity with the old engine and adds no new functionality.
- The switch keeps `session_info` and `model`, the engine banner, the plugin inventory, the crash marker, and the classified reason of a failed MCP connect (see Closed parity questions). It also keeps the cross-lane history bridge on a model switch, the frames of a subagent, and a model change that takes effect in the middle of a turn.
- The runtime ports questions (`AskUserQuestion` and `Resolve`) and Codex prewarm before the phase 4 switch. The first turn of a session waits for an in-flight warm-up, as the first prompt of the engine does.
- `turn.ended` has a typed `cause` (2026-10-04), as Codex `TurnAborted.reason` and the Claude Code `result` subtype type it. `error` holds only the masked, capped message, and a reader uses `cause` and never parses the text of `error`.
- A tool call that is still open when a turn completes, fails, or ends `provider_exhausted`, or when a handoff suspends the turn, gets a synthetic result that says the call did not finish (2026-10-04), as Codex and Claude Code do. This replaces the earlier rule that the runtime has no repair site for an orphan tool call.
- The parity questions under Closed parity questions are closed (2026-10-04, and 2026-10-05 for the child report by backend and for `DELETE /sessions/{id}`): mid-turn delivery, `session_info` and `model`, the banner, the plugin inventory, the crash marker, the MCP connect reason, the child report in the `[tasks: …]` segment of the engine, read by backend as the mid-turn delivery bullet says, the history bridge and the frames of a subagent, questions and Codex prewarm, the warm-up wait, `DELETE /sessions/{id}`, the messages page limit, the CI step of the contract suite, the `[continuation: …]` tags, the creation order of `GET /sessions`, the receipt of the answer route, a repeated agent definition name, a settings change to another kind of backend in the middle of a turn, and the log parts of a child report to a busy parent. The coordinator closed the last five of these, and the log parts of a child report, by the parity rule on 2026-10-05; Andy may veto them. The questions under Open questions stay open.
- Boxes pins an exact harness commit for its box images (2026-10-04), and harness reaches boxes only through a bump PR (see Boxes integration).
- The cutover is one quiesced cutover, as this spec says: no per-box canary, no `harness_api` column, and no dual stack. A rehearsal on a copy of production data and a tested rollback come first, so the `box_journal_*` tables of boxes drop one release after the cutover.
- What posts `SyncBatch` in a box (2026-10-05, by the parity and spec rules): the config keys `owner_epoch` and `sync {url, token_file}` build the `Owner` epoch and an HTTP sender in `New`, and `Runtime.CatchUp` replicates the stored sessions at start (see Events).
- The wire contract of `POST /v1/boxes/{id}/sync` (2026-10-05): `409 sync_conflict` is `ErrConflict`, and `400 invalid_request`, `413 too_large`, `401`, and `403` are final; `5xx` and transport errors resend. A batch stays under 32 MiB (see Events).
- On start, a box harness replicates every stored session through `Sync`, not only the open ones. At the cutover, the converted archive logs load into `pgstore` through the same receiver, so there is one conversion path and no converter in the control plane.
- `claim_epoch` is a plain counter that boxes increments when it admits a Spawn, and the ownership comparison uses it in the same release. Boxes writes one `BootConfig` file, validates it once, and sends post-boot values such as `DATABASE_URL` as an update to that same file, with no serve-env channel; `boxinit` supervises `harness serve` in phase 5.
- There is no comment purge. A history comment leaves when its code is rewritten or deleted; the gates stop new ones.
