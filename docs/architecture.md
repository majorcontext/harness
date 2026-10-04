# Harness architecture

A proposed re-architecture of harness: a session is an append-only event log, one goroutine owns each session, and every seam is a small interface owned by its consumer.

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
- A public Go API small enough to read in one sitting: `harness`, `harness/config`, `harness/protocol`, `harness/storetest`, `harness/harnesstest`.
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
  │ turn      agent loop → Backend (capabilities) · tool.Registry         │
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
| `harness` | `Runtime`, `Options`, `Session`, `OpenView`, `Store`, `Owner`, `Ownership`, `Tool`, `DiskStore`, `MemStore`, sentinel errors | meta home chat, boxes control plane, CLI |
| `harness/config` | `Config`, `Defaults`, `Validate`, `ApplyEnv`; imports only the standard library | boxinit `BootConfig` |
| `harness/protocol` | Data types shared by the Go API and HTTP, including `SyncBatch`; source of the generated OpenAPI and TS | boxes server, web, boxctl |
| `harness/storetest` | Conformance suite for a `Store` | boxes `pgstore` |
| `harness/harnesstest` | Scripted model server for contract suites | harness and boxes contract suites |

The Go API:

```go
package harness

func New(opts Options) (*Runtime, error) // no I/O; sessions load on Create, Open, or List

type Options struct {
	Config config.Config // New checks it with Validate
	Store  Store         // required
	Owner  Owner  // nil: the local process owns every session
	Tools  []Tool // embedder tools, beside built-in, MCP, and plugin tools
	// ModelTransport returns the HTTP transport for a model provider.
	// nil, or a nil result: the default transport.
	ModelTransport func(provider string) http.RoundTripper
	Sync           Sync // nil: no replication
	Logger         *slog.Logger
}

// A Runtime hosts many sessions. Each runs only while its Ownership holds.
func (r *Runtime) Handler() http.Handler
func (r *Runtime) Create(ctx context.Context, req protocol.CreateSession) (*Session, error)
func (r *Runtime) Open(ctx context.Context, id string) (*Session, error) // acquire, fence, replay, resume
func (r *Runtime) List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error)
func (r *Runtime) Models() []protocol.Model
// Close hands off every session, then returns once Sync has acknowledged
// every record through each handoff, or ctx ends.
func (r *Runtime) Close(ctx context.Context) error

func (s *Session) View() protocol.Session // includes HeadSeq and SyncedSeq
func (s *Session) Submit(ctx context.Context, in protocol.Input) (protocol.Admitted, error)
func (s *Session) Admit(ctx context.Context, in protocol.Input) (protocol.Admitted, bool, error) // Submit, and whether the input repeats
func (s *Session) Interrupt(ctx context.Context, req protocol.Interrupt) error
func (s *Session) Resolve(ctx context.Context, requestID string, res protocol.Resolution) error
func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)
func (s *Session) SetGoal(ctx context.Context, g protocol.Goal) error
func (s *Session) ClearGoal(ctx context.Context) error
func (s *Session) Compact(ctx context.Context) error
func (s *Session) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
func (s *Session) Release(ctx context.Context) error // hand off, flush Sync, release ownership

// OpenView reads a session from any Store without owning it: no Acquire, no appends.
func OpenView(ctx context.Context, st Store, id string) (*View, error)
func (v *View) Session() protocol.Session
func (v *View) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
func (v *View) Messages(ctx context.Context, before uint64, limit int) ([]protocol.Message, error)

// Sync replicates each session's records elsewhere, in seq order.
type Sync interface {
	// Deliver returns the receiver's head on success and on a seq mismatch;
	// the sender resends from Head+1. ErrStaleEpoch fires Ownership.Lost.
	Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error)
}

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
| `internal/eventlog` | Event schema, record codec, `Apply`, checkpoints |
| `internal/session` | Session actor, mailbox, state machines, child tree, views |
| `internal/turn` | One agent loop; declares `Backend` and `Tools` |
| `internal/backend/modelapi` | The one model API backend, for every provider wire |
| `internal/backend/external`, `claudecode`, `codexcli` | Third-party harness backends |
| `internal/message` | Conversation types used inside the runtime |
| `internal/modelmeta` | Context-window table from models.dev; exposed only through `Runtime.Models` and `GET /models` |
| `internal/tool` with `builtin`, `mcpsrc`, `pluginsrc` | Tool interface and sources |
| `internal/toolresult` | Large-result retention and `read_tool_result` |
| `internal/prompt` | System-prompt segments and agent profiles |
| `internal/mcp`, `plugin`, `skill`, `command`, `process` | Moved as is in phase 6 |

`internal/workspace` serves `GET /workspace/changes`. It shells out to git and cannot reach the runtime or any session. Harness is the only HTTP server in a box, so box-level reads live here, isolated.

Deleted: `engine`, `provider/claudecode`, `mcpserver` (merged into `internal/mcp`), and `imageclamp` and `typeid` (merged into their one consumer).

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
- A stale writer gets `ErrConflict` and stops.
- After an ambiguous error (a timeout, a lost connection), the writer reads `Head`. Equal to `expectedSeq`: the write did not land; retry. Equal to `expectedSeq` plus the number of records, and the records read back match: it landed. Anything else: stop, as if fenced.
- Compare-and-append alone does not fence a lease handoff: an old owner's in-flight append can still hold the current `expectedSeq`. The new owner therefore appends `owner.acquired` before it replays or runs (see Ownership). Any later append from the old owner conflicts. A shared store may also check its lease in the same transaction as the append.
- `Append` is durable on return. `DiskStore` group-commits one `fsync` per batch. `MemStore` is for tests.
- Checkpoints are blobs. List summaries come from the checkpoint.
- `storetest.Run(t, newStore)` is the conformance suite. Every `Store` runs it.

### Layout on disk

```
<root>/<session>/log.jsonl          source of truth
<root>/<session>/checkpoint.json    State at seq N, with a prefix hash; newest only
<root>/<session>/blobs/<key>        tool results, backend transcripts
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
| `session.created` | `parent_id?`, `model`, `settings`, `origin` |
| `owner.acquired` | `epoch`, `owner` |
| `settings.changed` | `model?`, `effort?`, `service_tier?` |
| `input.admitted` | `input_id`, `delivery` (`queue` or `steer`), `source`, `parts` |
| `input.promoted` | `input_id`, `turn_id` |
| `input.withdrawn` | `input_id` |
| `turn.started` | `turn_id`, `input_ids` |
| `item.completed` | `item_id`, `turn_id`, `message` (user, assistant, tool result) |
| `turn.suspended` | `turn_id`, `cause` |
| `turn.resumed` | `turn_id`, `count` |
| `turn.ended` | `turn_id`, `stop_reason`, `error?`, `usage` |
| `request.opened` | `request_id`, `item_id`, `kind`, `payload` |
| `request.resolved` | `request_id`, `resolution` (`answered` or `dismissed`), `answer?` |
| `goal.set` | `condition`, `max_turns` |
| `goal.evaluated` | `turn_id`, `verdict`, `guidance?` |
| `goal.changed` | `state`, `reason?` |
| `compaction.applied` | `from_seq`, `to_seq`, `summary`, `by_backend` |
| `child.spawned` | `child_id`, `agent?` |
| `child.settled` | `child_id`, `outcome` (`done`, `failed`, `canceled`), `result_ref` |
| `context.measured` | `tokens`, `window`, `source` |
| `backend.state` | `backend`, `blob_key` |

Ephemeral frames go to subscribers and never to the store: `item.started`, `item.delta`, `status`. Each has `ephemeral: true` and the last durable seq, and an item frame carries its `item_id`, so a client can place it.

### Apply

```go
func (s *State) Apply(r Record) error
```

`Apply` is the only code that changes durable state. Live code appends, then applies the same record. Replay applies the log from the checkpoint. There is no other fold. The summary for `GET /sessions` is `State.Summary()`.

### Invariants enforced at append

- Every tool call item gets exactly one result item, or an open request. `message.ResolveOrphanToolCalls` and its four sibling repair sites are deleted.
- `seq` is gap-free per session.
- A `turn.ended` follows every `turn.started`, except for a suspended turn.
- A turn stopped with `ErrTurnStopped` never resumes.

### No old formats

Nothing reads the current journal, index, snapshot, or `events.jsonl`. At cutover, existing sessions start with empty history, and archived boxes are not converted.

## session

### Actor

Each live session is one goroutine. It holds the `Ownership` and the `State`. Commands arrive through a mailbox; each has an optional reply channel.

| Command | Effect |
| --- | --- |
| `Admit(input)` | Append `input.admitted`; promote or queue |
| `Withdraw(id)` | Append `input.withdrawn` if still queued |
| `Interrupt(turnID?)` | Cancel the turn; reply when it has stopped |
| `Resolve(requestID, resolution)` | Append `request.resolved`; resume the turn |
| `SetSettings(...)` | Validate the model; append `settings.changed` |
| `SetGoal(...)`, `ClearGoal()` | Append goal events |
| `Compact()` | Run compaction through the turn runner |
| `Spawn(child)` | Create a child session; append `child.spawned` |
| `Suspend(cause)` | Stop the turn with a handoff cause; release ownership |

The actor sends appends to one writer goroutine per session, which keeps `expectedSeq` order and group-commits. `Apply` runs after the append returns. A command that needs durability replies after `Apply`. The mailbox never waits on disk.

The turn runner is one goroutine per turn. It reads a `State` view, calls the backend and tools, and sends `item.completed` and `turn.ended` to the actor. It touches no session field.

Reads use `atomic.Pointer[View]`. A `View` is immutable: status, turn, goal, queue, pending requests, settings, usage, context, head seq.

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

Start-up order after `Acquire`:

1. Read `Head`.
2. Append `owner.acquired{epoch, owner}` at that head. On `ErrConflict`, read `Head` again and retry. An old owner's append that lands first is ordered before the fence.
3. Replay through the fence record, then run.

After step 2, every append from a previous owner conflicts. When `Lost` closes, the actor stops without another append; `ErrConflict` from the store has the same effect. The default `Owner` grants every session to the local process. Boxes supplies a lease.

### State machines

Session status derives from the turn and the queue:

| Status | Condition |
| --- | --- |
| `idle` | No turn is running |
| `running` | A turn is running |
| `waiting` | A turn ended `awaiting_input`; a request is open |
| `retrying` | A turn waits for backoff; carries `attempt` and `next_at` |

Turn:

```
running ─► completed | interrupted | failed | awaiting_input
running ─► suspended ─► running   (next owner resumes from the last completed item)
```

A turn ends early for one of four causes. A live owner carries the first three with `context.WithCancelCause`; `Open` detects `crashed` in the log:

| Cause | Trigger | Effect |
| --- | --- | --- |
| `stopped` | User interrupt | Keep the partial; unfinished tool calls get `interrupted` results; the next queued input runs |
| `goal_cleared` | `ClearGoal` during a goal turn | Same as `stopped` |
| `handoff` | `Session.Release`, `Runtime.Close` | Stop at an item boundary: admit no new tool call, let running tools finish within the budget, append `turn.suspended`. A delegated backend (`OwnsLoop`) cannot stop at an item boundary, so a handoff interrupts it, records every item that it already wrote, gives each open tool call a cut-off result, and appends `turn.suspended`. A suspended turn has no open tool call, so the next owner resumes it automatically. |
| `crashed` | `Open` finds `turn.started` with no end or suspend (forced stop, OOM, an exceeded handoff budget) | Append `turn.ended{interrupted, crashed}`; keep the partial; each open tool call gets a result saying it was cut off and to check whether it took effect before running it again. The session then starts the next queued input, or waits for input when none is queued. |

No tool call is ever re-run after a stop. This matches Codex, Claude Code, opencode, pi, and fx. The log stays strictly append-only, and a client hides output by cause if it wants to.

Input:

```
admitted ─► promoted   (queue: next turn; steer: next item boundary)
admitted ─► withdrawn
```

A `steer` input with `expected_turn_id` fails with `turn_mismatch` if that turn is not running. A `steer` input on an idle session starts a turn.

Goal:

```
active ─► paused ─► active
   └───► achieved | failed | exhausted
any ─► cleared
```

Goals follow Claude Code `/goal`. There is no deferred goal.

- `SetGoal` on an idle session with no queued input admits the condition as an input with `source: goal`. That input starts a turn through the normal input events.
- `SetGoal` on a busy session, or with queued input, starts nothing. The next turn that ends is the first one evaluated.
- A new `SetGoal` replaces the goal and resets its turn count.
- After each turn, the evaluator returns `met`, `not_met` with guidance, or `impossible`. Guidance is an input with `source: goal`. `impossible` yields `failed`.
- A turn that fails on a retryable error or a usage limit yields `paused`. Harness retries with backoff, and any input resumes the goal. An error the user must fix yields `failed`.
- `max_turns` bounds goal turns; 0 is unlimited. Reaching it yields `exhausted`.
- The goal lives in the log. `Open` restores it with its turn count. An `active` goal on an idle session continues, and a `paused` goal keeps its retry time.

Request:

```
pending ─► answered | dismissed
```

Any backend can open a request. An input admitted while a request is open dismisses it first.

### Compaction

Compaction runs as the run of the actor, never beside a turn. It copies the engine rules.

- A turn starts at a user message. A compaction folds every turn before the newest `compaction_keep_turns` (default 2). It never folds only the previous summary.
- `to_seq` is the seq before the first kept message. The `input.admitted` record of a kept or queued input can have a lower seq, so a reader that hides `from_seq` through `to_seq` keeps each `input.admitted` record.
- A backend without `OwnsContext` summarizes the folded messages with the session model and the engine compaction prompt. The actor appends `compaction.applied` with `by_backend: false`.
- A backend with `OwnsContext` runs `/compact` as a turn. The backend logs `compaction.applied` with `by_backend: true`.
- `Compact()` fails with `session_busy` while a turn runs or inputs wait.
- Before a queued input starts a turn, the actor compacts first when the newest `context.measured` reading is at or above `compaction_threshold` (default 0.8) of its window. A setting at or below 0 is the default. A model call with no prompt tokens records no reading.
- A failed summary appends nothing, and the turn starts on the full history. A handoff stops the summary and appends nothing.
- `Open` starts the next queued input when no turn is open. The next owner thus runs the input that waited for a stopped summary, and compacts first when the reading still passes the threshold.

### Children

A child is a session with `parent_id`. The parent holds a handle, not the child's state. When a child turn ends, the child sends `child.settled` to the parent, and the parent admits the outcome as an input with `source: child`. Task notifications become inputs; the separate checkout-and-commit queue is deleted.

Tree limits (depth, concurrency, token budget) live in a per-root supervisor with its own state.

### Shutdown

One `errgroup` per runtime tracks every actor, turn runner, and writer. `Runtime.Close` suspends each session with a handoff cause and waits.

## HTTP

### Routes

```
POST   /sessions                              create; client id optional
GET    /sessions?after=&limit=                list
GET    /sessions/{id}                         view
PATCH  /sessions/{id}                         model, effort, service_tier
DELETE /sessions/{id}
POST   /sessions/{id}/inputs                  submit
GET    /sessions/{id}/inputs                  queued inputs
DELETE /sessions/{id}/inputs/{input}          withdraw
POST   /sessions/{id}/interrupt               {turn_id?, tree?}
POST   /sessions/{id}/compact
PUT    /sessions/{id}/goal                    {condition, max_turns}
DELETE /sessions/{id}/goal
POST   /sessions/{id}/requests/{request}      {answer} | {dismiss}
GET    /sessions/{id}/events?after=&limit=    page; SSE with Accept: text/event-stream
GET    /sessions/{id}/messages?before=&limit= projection, same seq
GET    /models                                models and their capabilities
GET    /commands                              slash commands
GET    /processes · POST /processes/{name}/{action}
GET    /workspace/changes                     working-tree diff
GET    /health
```

There is no version prefix: harness and its clients change together.

Today harness has 36 routes and seven ways to read a session. This has one log and one cursor.

### Inputs

Request: `{id, parts, delivery, source?, expected_turn_id?}`. The client mints `id`.

| Case | Response |
| --- | --- |
| New id | `201 {input_id, seq, state}` |
| Same id, same body | `200` with the original receipt |
| Same id, other body | `409 input_conflict` |

### Events

- One per-session `seq` serves paging and SSE resume.
- Only SSE frames for durable records carry `id: <seq>`. `Last-Event-ID` and `after=` resume exactly, across processes.
- A live frame (`item.started`, `item.delta`, `status`) sets `protocol.Event.Ephemeral` and is never stored. Its `seq` is the last durable seq when it was sent.
- A slow subscriber gets a `gap` frame and a close. It never loses a record silently.
- Replication is `Options.Sync`: each `protocol.SyncBatch{epoch, session, from_seq, records, blobs}` is a remote append. The receiver rejects an older epoch. A batch with `from_seq` at its head plus one is appended. A batch whose records are all at or below its head is a retry: identical bytes are acknowledged as a duplicate, and different bytes are rejected. Any other `from_seq` is a seq mismatch. Every reply is a `protocol.SyncAck{head}`, including a seq mismatch, so the sender resends from `head+1` out of its own Store; that also heals a receiver that missed records before a crash. A stale-epoch rejection fires `Ownership.Lost`.
- The epoch is a number because fencing needs order. An embedder maps its own claim to a monotonic epoch; boxes uses `claim_epoch`, and its string command ID stays the workflow token.

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
| `payload_too_large` | 413 |
| `draining` | 503 |
| `internal` | 500 |

Each code except `internal` is a sentinel error in `harness` and a `protocol` constant. `server` maps it with `errors.Is`. Any other error is `internal`, and its message is a fixed string. A path or method that no route serves answers 404 or 405 with `invalid_request`.

### Contract source

Go types in `protocol` are the source. `go generate ./protocol` writes `protocol/openapi.json` and `protocol/protocol.ts` with `github.com/invopop/jsonschema` and a route table that `server` exports. CI regenerates and fails on a diff. A `server` test walks the route table against the mux. The hand-written `server/openapi.yaml` is deleted.

## turn and backend

### Backend

```go
package turn

type Backend interface {
	Capabilities(model message.ModelRef) Capabilities
	Run(ctx context.Context, req Request, out Sink) (Result, error)
}

type Capabilities struct {
	OwnsLoop      bool     // backend runs tools and multi-step turns
	OwnsContext   bool     // backend compacts its own context
	Steering      bool     // accepts input mid-turn
	ContextWindow int      // 0 means the backend reports it
	Tools         []string // built-in tools of a delegated backend
}

type Sink interface {
	Item(message.Message) error
	Delta(itemID string, d Delta)
	Open(Request) error
	Telemetry(Telemetry)
}
```

- A model API backend runs one model call per `Run`. The loop runs the tools.
- A delegated backend (`claudecode`) runs the whole turn and reports items.
- `AllowedTools` holds tool names in one namespace. For a model API backend, they are the embedder tools. For a delegated backend, they are its built-in tools from `Capabilities.Tools` and the embedder tools, and any other name fails `Create`. An embedder tool with the name of a built-in tool also fails `Create`.
- Retry, the stall watchdog, and compaction read `Capabilities`. No code compares a provider name.
- Private backend state is one `backend.state` event plus a blob. The Claude Code transcript mirror is that blob. The eight `claudeCode*` fields and their record kinds are deleted.
- Model metadata comes from `modelmeta`. An unknown model fails with `model_unavailable` at create and at a settings change.
- `Telemetry` carries usage, cost, the context reading, and subscription quota.

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

Delegating a turn to another agent harness is permanent. Claude Code is the first; the Codex CLI is next. Each one is a `Backend` with `OwnsLoop`, `OwnsContext`, and `Steering` set. They share one adapter contract, so adding a harness adds one package and touches nothing else.

| Concern | Contract | Claude Code | Codex CLI |
| --- | --- | --- | --- |
| Turn | One harness turn is one external turn | `--resume` print run | `turn/start` |
| Items | External items become `item.completed` | stream-json frames | `item/completed` |
| Steer | A `steer` input reaches the running turn | stdin | `turn/steer` with `expectedTurnId` |
| Interrupt | Stops the external turn; reports `interrupted` | SIGINT | `turn/interrupt` |
| Requests | Questions and approvals become `request.opened`; the resolution goes back | `AskUserQuestion` defer | `requestApproval`, `requestUserInput` |
| State | External session id and transcript mirror are one `backend.state` blob | `--session-mirror` | rollout file |
| Tools | Harness tools reach the external harness through a harness-hosted MCP endpoint; `Restrict` applies | `--mcp-config` | MCP config |
| Context | The external harness compacts; harness logs `compaction.applied` with `by_backend` | `/compact` | native |
| Telemetry | Usage, cost, and context window arrive through `Sink.Telemetry` | `result` event | `thread/tokenUsage/updated` |

`internal/backend/external` holds what every adapter shares: process supervision, the line-protocol transport, the mirror writer, and the MCP bridge. `internal/backend/claudecode` and `internal/backend/codexcli` hold only the mapping in the table.

### Warm-up

`turn` declares an optional `Warmer` interface: `Warm(ctx context.Context, req Request) error`. The session calls it once on create and on wake, fire-and-forget under the session context. Only `internal/backend/modelapi` implements it, through an optional `Warm` method of the client, for the Codex websocket transport. The first turn uses the warm connection if it is ready. No warm-up state lives on the session.

## tool, prompt, and config

### tool

```go
package tool

type Tool interface {
	Spec() Spec
	Run(ctx context.Context, call Call) (Result, error)
}

func (r *Registry) Add(src Source) error            // ErrDuplicate on a name clash
func (r *Registry) Restrict(names []string) *Registry
func (r *Registry) Lookup(name string) (Tool, bool)
```

- `Spec.Traits` carries `Serial`, `Key`, `MaxInline`, and `Deferrable` for every source: built-in, MCP, and plugin.
- `Call.Env` is a narrow interface: work dir, processes, children, goal. No tool receives a session.
- `Restrict` implements `AllowedTools` and `DisableBuiltinTools`.

Agent profiles name a kind of child: `name`, `description`, `tools`, `model`, and a prompt body, in Claude Code's agent frontmatter so one file serves every backend. `internal/prompt` loads them from `<workdir>/.agents`. `Spawn(child{agent})` applies a profile through `Registry.Restrict` and a prompt segment.

### prompt

```go
package prompt

type Provider interface {
	Segment(ctx context.Context) (Segment, error)
}

type Memo[T any] struct{ /* once, value, err */ }
```

Each segment declares whether a load error fails the turn or degrades with a notice. A bad `SKILL.md` degrades. Ambient status stays a pinned message, through one `Ambient` provider.

### config

One `Config` struct. `Defaults` is the one defaults table, and each accessor reads it for an unset key. `Validate` is the one rule set. `LoadProject` runs it on the merged config, and `New` runs it on `Options.Config`. It never changes the config. `ProcessSpec.Validate` is the per-entry rule that `process.Declare` also uses.

`ApplyEnv` sets each top-level string, number, or bool key from `HARNESS_<KEY>`. An empty variable keeps the key. A map, slice, or struct key has no variable. A parse error names the variable and never the value. There are no env-only knobs. The phase 4 switch wires `ApplyEnv` into `cmd/harness`.

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
| Tool-result retention | Keep; drop the size knobs |
| Read budget | Replace with a file-size cap |
| Snapshots, index | Replace with checkpoint and `Apply` |
| Box-global `events.jsonl` | Delete |
| Worktrees, `workdir_isolation`, worktree sweep | Delete |
| Modal guide and `scripts/modal-e2e.py` | Delete |
| `/debug/pprof`, `/debug/goroutines` | Delete |
| `cancel_tree` | Merge into `interrupt {tree}` |
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
- `HARNESS_E2E_COVER=1 go test -race ./e2e/ -run TestContract` builds an instrumented binary, runs the contract scenarios, and prints the statement coverage by package from `TestMain`. It appends a Markdown table to `$GITHUB_STEP_SUMMARY` when that variable is set. Test cleanup sends SIGTERM before SIGKILL so a serve process flushes its counters. CI runs the command without gating.

Today ~70% of 127k test lines read unexported state and will not survive the restructure. The target is 40k–50k test lines.

### Unit tests

Unit tests cover pure code only: `Apply`, wire transcoders, `config`, `message`. A bug fix adds a table row, not a file. No test reads unexported state across packages.

### CI gates

| Gate | Threshold |
| --- | --- |
| Whole-line comments per file | Warn above 15%; fail above 25%. The `doc.go` package comment does not count |
| History markers in comments | Fail on issue numbers, dates, "previously", "no longer", "red-verified", "confirmed live", "an earlier version", "before this change", "round N", review or fix rounds, and "copilot" |
| File size | Fail above 800 lines |
| Function size | Fail when the closing brace is more than 80 lines below the opening brace |
| Test lines vs code lines per package | Fail above 1.5 test lines per code line, unless the ratio does not rise over the merge base. A new package has no base, but a moved package compares with the package it came from. A change that removes code and adds no test lines always passes |
| `time.Sleep`, `time.After` in tests | Fail in a test file. `internal/testpoll` is not a test file |
| `AGENTS.md` length | Fail above 80 lines at the root and 25 lines in a scoped file |
| Merge-base diff | `TestRepository` checks only files and packages that differ from `git merge-base HEAD origin/main`, or from `$GATES_BASE_REF`. A new file meets each limit above. A changed file may not cross a limit that it met, and may not get worse on a limit that it already broke. A renamed file compares with its old path. A change that only deletes code always passes. An unchanged file is not checked. No baseline file exists |
| Imports | `depguard`: internal packages never import `server` or `cmd` |
| Lint | `govet`, `staticcheck`, `errcheck`, `unused`, `revive` |

Gates compare a branch with its merge base, so old code never blocks a change and new code starts strict. A protocol drift gate (regenerate; fail on a diff) is planned for phase 4, when `protocol` generation exists. `AGENTS.md` shrinks to these gates and the four rules.

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
- **One epoch.** The box claim token is the `Ownership.Epoch`. It is recorded in `owner.acquired`, carried by every `SyncBatch`, and checked by boxes. A stale box learns it is fenced from the rejection.

| Boxes requirement | Harness answer | Phase |
| --- | --- | --- |
| Read-only open | `OpenView` | 2 |
| Public scripted model | `harness/harnesstest` | 1 |
| Pump contract | `Sync` and `protocol.SyncBatch` | 2 |
| `config.Config` without server dependencies | `harness/config` | 3 |
| Durable head per session | `protocol.Session.HeadSeq` from `Session.View()` or `View.Session()` (Apply runs after a durable append) | 2 |
| Open after a forced stop | the `crashed` cause | 2 |
| `Models()` with no sessions | `New` does no I/O | 2 |
| External lease that fences a stale epoch | `Ownership.Epoch`; `ErrStaleEpoch` fires `Lost` | 2 |

Combined sequence:

| Harness | Boxes |
| --- | --- |
| 1 contract suite, gates, `harnesstest` | 0 defect fixes · 1 protocol, contract suite, gates · 2+3 commands and lifecycle · 4 capacity (none need harness) |
| 2 runtime core | Home chat on `pgstore` and `SessionHost` (meta) |
| 3 leaves, `harness/config` | 5 `BootConfig` embeds `harness/config` |
| 4 HTTP cutover | 7 session cutover with the read-only view, same release |
| 5 backends, `codexcli`, requests | — |
| 6 delete `engine`, `server` | — |

Answered boxes requests: `Runtime.Close` and `Session.Release` return only after `Sync` acknowledges every record through the handoff, and `View` reports `SyncedSeq`; `Sync.Deliver` returns `SyncAck{head}` on every reply; the epoch is a monotonic number (`claim_epoch`). The home chat is one per person.

## Migration

Each phase is one or more PRs on `main`. Each ships alone.

| Phase | Work | Consumers |
| --- | --- | --- |
| 1 | Contract suite: scenario scripts and `harnesstest`; CI gates that diff against the merge base; new `AGENTS.md` | Boxes contract suite reuses `harnesstest` |
| 2 | New runtime core beside the old engine, in the order meta needs it: `harness.Store` and `storetest`; `Owner` with `Epoch`; `Runtime`, `Session.Submit`, `Events`, `OpenView`; `Sync` and `SyncBatch`; handoff and crash causes; a native backend with `ModelTransport` (Codex first); `harness.Tool` and `Restrict`; the `external` adapter and `claudecode`. Absorbs the design of PR #359, its conformance suite, and its `fakeclaude` modes. | The meta home chat embeds it on `pgstore`; it is the first consumer |
| 3 | `harness/config` with `Defaults`, `Validate`, and `ApplyEnv`, on the standard library only; one `modelapi` backend for every model API wire | Boxes `BootConfig` |
| 4 | New HTTP and `protocol` generation. Scenario scripts carry over; their assertions move to the new API. One PR switches `cmd/harness`. | Boxes console adopts the harness shapes; boxes routes become thin forwarders. Same release. |
| 5 | Remaining backends on capabilities; `codexcli`; requests; `Warmer` | None |
| 6 | Delete `engine`, `server`, old formats, dead features; move leaves to `internal/` | None |

PR #359 closes unmerged; its design is in this doc. The meta home chat has no old data or routes, so it proves the new runtime before boxes switches. Phase 4 is a cutover, not an adapter: no old route, format, or Go API survives it.

## Open questions

None remain.

Decided:

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
- There is no comment purge. A history comment leaves when its code is rewritten or deleted; the gates stop new ones.
