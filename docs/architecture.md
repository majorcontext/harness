# Harness architecture

The re-architecture of harness, as built and as planned: a session is an append-only event log, one goroutine owns each session, and every seam is a small interface owned by its consumer.

Phases 1 to 3 are built. The new runtime runs beside `engine` and `server` until the phase 4 switch. A statement that names a later phase describes planned work.

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

The Go API:

```go
package harness

func New(opts Options) (*Runtime, error) // no I/O; sessions load on Create, Open, or List

type Options struct {
	Store  Store         // required
	Owner  Owner         // nil: the local process owns every session
	Sync   Sync          // nil: no replication
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
func (s *Session) Resolve(ctx context.Context, requestID string, res protocol.Resolution) error // phase 5
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
func (v *View) Messages(ctx context.Context, before uint64, limit int) ([]protocol.Message, error) // phase 4

// Sync replicates each session's records elsewhere, in seq order.
type Sync interface {
	// Deliver returns the receiver's head on success and on a seq mismatch;
	// the sender resends from Head+1. ErrStaleEpoch fires Ownership.Lost.
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
| `internal/session` | Session actor, mailbox, state machines, views, replication; the child tree is planned |
| `internal/turn` | One agent loop; declares `Backend`, `Tool`, and `Source`; `Restrict` |
| `internal/backend/modelapi` | The one model API backend, for every provider wire |
| `internal/backend/external`, `claudecode` | Third-party harness backends; phase 5 adds `codexcli` |
| `internal/tool/mcpsrc` | A `turn.Source` that gives MCP tools to each model call |
| `internal/tool/pluginsrc` | A `turn.Source` and `turn.Hooks` that give the plugin tools, hooks, and events to each session |
| `internal/tool/builtin` | The file, search, and shell tools of a coding agent |
| `internal/toolresult` | Large-result retention and `read_tool_result`, at parity with the engine |
| `internal/prompt` | System-prompt segments; agent profiles are planned |

Phase 6 moves the leaf packages to `internal/`: `message` (conversation types), `modelmeta` (context-window table from models.dev; exposed only through `Runtime.Models` and `GET /models`), and `mcp`, `plugin`, `skill`, `command`, and `process`, as is.

`internal/workspace` is planned for phase 4. It serves `GET /workspace/changes`. It shells out to git and cannot reach the runtime or any session. Harness is the only HTTP server in a box, so box-level reads live here, isolated.

Phase 6 deletes `engine`, `server`, `provider/claudecode`, `mcpserver` (merged into `internal/mcp`), and `imageclamp` and `typeid` (merged into their one consumer).

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
- Any other append error also stops the session actor, with no retry. The next `Open` fences and replays from the store, so an append that landed is in the replay, and one that did not land is not.
- Compare-and-append alone does not fence a lease handoff: an old owner's in-flight append can still hold the current `expectedSeq`. The new owner therefore appends `owner.acquired` before it replays or runs (see Ownership). Any later append from the old owner conflicts. A shared store may also check its lease in the same transaction as the append.
- `Append` is durable on return. `DiskStore` writes the records of one `Append` with one `fsync`. `MemStore` is for tests.
- There are no checkpoints. `Open` and `OpenView` replay the whole log. `List` reads the view of a session that this runtime runs, and replays the log of any other session. Add a checkpoint only when a measurement shows that replay costs too much.
- `storetest.Run(t, newStore)` is the conformance suite. Every `Store` runs it.

### Layout on disk

```
<root>/<session>/log.jsonl          source of truth; line N holds seq N
<root>/<session>/blobs/<key>        backend state; retained tool results from phase 3
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
| `turn.ended` | `turn_id`, `stop_reason`, `error?`, `usage` |
| `request.opened` | `request_id`, `item_id`, `kind`, `payload` |
| `request.resolved` | `request_id`, `resolution` (`answered` or `dismissed`), `answer?` |
| `goal.set` | `condition`, `max_turns` |
| `goal.evaluated` | `turn_id`, `verdict`, `guidance?` |
| `goal.changed` | `state`, `reason?`, `retry_at?` |
| `compaction.applied` | `from_seq`, `to_seq`, `summary`, `by_backend` |
| `child.spawned` | `child_id`, `agent?` |
| `child.settled` | `child_id`, `outcome` (`done`, `failed`, `canceled`), `result_ref` |
| `context.measured` | `tokens`, `window`, `source` |
| `backend.state` | `backend`, `blob_key` |
| `tool_result.retained` | `handle`, `tool`, `blob_key`, `bytes`, `lines`, `head` |

Ephemeral frames go to subscribers and never to the store: `item.started`, `item.delta`, `status`. Each has `ephemeral: true` and the last durable seq, and an item frame carries its `item_id`, so a client can place it.

### Apply

```go
func (s *State) Apply(r Record) error
```

`Apply` is the only code that changes durable state. Live code appends, then applies the same record. Replay applies the whole log from seq 1. `State.History` holds the summary of the newest compaction and the messages after it, so the model sees the history from the newest compaction on. There is no other fold. The summary for `GET /sessions` is `State.Summary()`.

### Invariants enforced at append

`eventlog.Check` applies a batch to a copy of the `State` before the actor appends it. A batch that breaks an invariant fails with `ErrIllegal` and is not appended.

- Every tool call item gets exactly one result item, or an open request, before its turn ends. The runtime has no repair site for an orphan tool call.
- `seq` is gap-free per session.
- A `turn.ended` follows every `turn.started`, except for a suspended turn.
- Only a suspended turn resumes. A turn that ended never resumes.

### No old formats

Nothing reads the current journal, index, snapshot, or `events.jsonl`. At cutover, existing sessions start with empty history, and archived boxes are not converted.

## session

### Actor

Each live session is one goroutine. It holds the `Ownership` and the `State`. Commands arrive through a mailbox as functions that run on the actor goroutine. The caller waits for the reply.

| Command | Effect |
| --- | --- |
| `Submit(input)` | Append `input.admitted`; start a turn, queue, or steer |
| `Interrupt(turnID?)` | Cancel the turn; reply when it has stopped |
| `Update(settings)` | Check the model; append `settings.changed` |
| `SetGoal(...)`, `ClearGoal()` | Append goal events |
| `Compact()` | Run a compaction as the run of the actor |
| `Spawn(child, agent)` | Append `child.spawned`; return the `session.created` of the child |
| `Settle(outcome, report)` | Append `child.settled` and admit the report as an input with `source: child`; a settled child changes nothing |
| `Release()` | Suspend the turn with cause `handoff`; stop; release ownership |
| `Withdraw(id)` | Phase 4: append `input.withdrawn` if still queued |
| `Resolve(requestID, resolution)` | Phase 5: append `request.resolved`; resume the turn |

The actor appends with no other goroutine. It checks the batch with `eventlog.Check`, appends it with `Store.Append`, applies each record, and publishes a new view. A command that needs durability replies after `Apply`. The store write is the only wait on disk in the actor.

The turn runner is one goroutine per turn. It gets a `turn.Request` that the actor builds from the `State`, calls the backend and tools, and sends each item and the end of the turn to the actor as commands. It touches no session field.

Reads use `atomic.Pointer[View]`. A `View` is immutable: the `protocol.Session` (status, turn, goal, queue, settings, usage, head seq) and whether the actor stopped.

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

`Create` appends `session.created` and `owner.acquired` to an empty log in one append. A conflict there fails with `ErrSessionExists`.

Start-up order of `Open` after `Acquire`:

1. Read `Head`.
2. Append `owner.acquired{epoch, owner}` at that head. On `ErrConflict`, read `Head` again and retry. An old owner's append that lands first is ordered before the fence.
3. Replay through the fence record, then run.

After step 2, every append from a previous owner conflicts. When `Lost` closes, the actor stops without another append; `ErrConflict` from the store has the same effect. The default `Owner` grants every session to the local process at epoch 1, one grant at a time. Boxes supplies a lease.

### State machines

Session status derives from the turn and the open requests:

| Status | Condition |
| --- | --- |
| `idle` | No turn is running |
| `running` | A turn is running |
| `waiting` | A turn ended `awaiting_input`; a request is open |

`retrying` is not a session status. A turn that waits for backoff sends an ephemeral `status` frame with `retrying`, `attempt`, and `next_at`, and the session stays `running`.

Turn:

```
running ─► completed | interrupted | failed | awaiting_input
running ─► suspended ─► running   (next owner resumes from the last completed item)
```

A turn ends early for one of five causes. A live owner carries the first three with `context.WithCancelCause`; `Open` detects `crashed` in the log; the turn loop reports `provider_exhausted`:

| Cause | Trigger | Effect |
| --- | --- | --- |
| `stopped` | User interrupt | Keep the partial; unfinished tool calls get `interrupted` results; the next queued input runs |
| `goal_cleared` | `ClearGoal` during a goal turn | Same as `stopped` |
| `handoff` | `Session.Release`, `Runtime.Close` | Stop at an item boundary: admit no new tool call, let running tools finish within the budget, append `turn.suspended`. A delegated backend (`OwnsLoop`) cannot stop at an item boundary, so a handoff interrupts it, records every item that it already wrote, gives each open tool call a cut-off result, and appends `turn.suspended`. A suspended turn has no open tool call, so the next owner resumes it automatically. |
| `provider_exhausted` | A usage limit of the provider: a spent quota, credit balance, or spend cap | Append `turn.ended{failed, provider_exhausted}`; keep the partial; queued inputs wait for the next input |
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

Goals follow Claude Code `/goal`. There is no deferred goal and no parked goal.

- `SetGoal` on a session with no running turn and no queued input admits the condition as an input with `source: goal`. That input starts a turn through the normal input events.
- `SetGoal` while a turn runs, or with queued input, starts nothing. The next turn that ends is the first one evaluated.
- A new `SetGoal` replaces the goal and resets its turn count. `SetGoal`, `ClearGoal`, each verdict, and each pause or failure withdraw the queued inputs with `source: goal`.
- After each turn that completes or is interrupted, the evaluator runs as the run of the actor, as a compaction does. It reads the condition and the history, and returns `met`, `not_met` with guidance, or `impossible`. Guidance is an input with `source: goal`. `met` yields `achieved`, and `impossible` yields `failed`. The evaluator skips leading markdown marks. A reply with no verdict is `not_met`, and the reply is the guidance.
- The evaluator is `goal_evaluator_model`, resolved through `aliases`. `SetGoal` without it is an invalid request. Its prompt copies the engine prompt, with a third form for `impossible`.
- A turn or an evaluation that fails on a retryable error or a usage limit yields `paused` with `retry_at`. The wait starts at 30 s and doubles with each pause before the next verdict, up to 30 min. At `retry_at`, the goal becomes `active` and judges the last turn again after an evaluator error, or admits an input that continues the goal. Any input resumes the goal at once. An error the user must fix yields `failed`.
- `max_turns` bounds goal turns; 0 is unlimited. Reaching it yields `exhausted`.
- `ClearGoal` during a goal turn or its evaluation stops it with cause `goal_cleared` and returns after it ends. When the actor then runs nothing, `ClearGoal` starts the next queued input. An interrupt stops only the turn, and the goal judges the partial turn. An interrupt during an evaluation stops nothing.
- The goal lives in the log. `Open` restores it with its turn count. An `active` goal on an idle session judges the last turn when the goal has not judged it, and a `paused` goal keeps its retry time.
- At the switch, the `goal_met_first_turn`, `goal_not_met_then_met`, `goal_exhausts_max_turns`, and `bifrost_goal_*` rows are the oracle. The deferred and parked rows are deleted.

Request:

```
pending ─► answered | dismissed
```

The schema and `Apply` hold requests today. Phase 5 lets any backend open a request and adds `Resolve`. An input admitted while a request is open dismisses it first.

### Compaction

Compaction runs as the run of the actor, never beside a turn. It copies the engine rules.

- A turn starts at a user message. A compaction folds every turn before the newest `compaction_keep_turns` (default 2). It never folds only the previous summary.
- `to_seq` is the seq before the first kept message. The `input.admitted` record of a kept or queued input can have a lower seq, so a reader that hides `from_seq` through `to_seq` keeps each `input.admitted` record.
- A backend without `OwnsContext` summarizes the folded messages with the session model and the engine compaction prompt. The actor appends `compaction.applied` with `by_backend: false`.
- A backend with `OwnsContext` runs `/compact` as a turn. The backend logs `compaction.applied` with `by_backend: true`.
- `Compact()` fails with `session_busy` while a turn runs or inputs wait.
- Before a queued input starts a turn, the actor compacts first when the newest `context.measured` reading is at or above `compaction_threshold` (default 0.8) of its window. A setting at or below 0 is the default. A model call with no prompt tokens records no reading.
- A failed summary appends nothing, and the turn starts on the full history. A handoff stops the summary and appends nothing.
- A model call that overflows the context window compacts while its turn runs, for a backend without `OwnsContext`, and the turn calls the model again on the new history. When no turn can fold or the summary fails, the turn fails. With no new input in the turn, a second overflow fails it: the summary already holds every turn but the newest kept turns.
- `Open` starts the next queued input when no turn is open. The next owner thus runs the input that waited for a stopped summary, and compacts first when the reading still passes the threshold.

### Children

A child is a session with `parent_id`. The `task` tool starts it in the background, as the Task tool of Claude Code starts a background subagent. The runtime adds the `task` tool only with a `WorkDir`. The parent holds the child ID, not the child's state. Task notifications become inputs; the separate checkout-and-commit queue is deleted.

- Spawn. The tool reads the agent profile (default `general-purpose`) and checks the tree limits. The parent actor appends `child.spawned`. Then the runtime creates the child in one append: `session.created` with `parent_id`, `agent`, the model and settings of the parent, and the allowed tools of the parent narrowed by the profile; the task as an input with `source: parent`; and the `turn.started` of that input. The tool returns the child ID at once. A child that fails to start settles `failed` with no input, and the tool call fails.
- Settle. When a turn of a child ends, the child reports to its parent: `done` for a completed turn, `failed` for a failed or crashed turn, `canceled` for a stopped turn. The report names the child, its agent, the outcome, and the error, and holds the last assistant text of the child, as the Task tool of Claude Code returns. The parent appends `child.settled`, with the child turn ID as `result_ref`, and the report as an input with `source: child`, in one append. An idle parent starts a turn with it; a busy parent queues it. A child settles once, so a later report changes nothing. Each turn end of a child reports, and the runtime opens a parent that it does not run, so a later input to a settled child opens its parent again.
- Crash and handoff. Each session follows its own rules. A child that a handoff suspended resumes when it opens. A crashed child turn ends `crashed` and settles `failed`. When a parent opens, it settles each unsettled child that has ended, because a crash can come between the `turn.ended` of the child and the `child.settled` of the parent. It settles a child with no log `failed` with no input, and opens each other child, which reports when its turn ends. No tool call runs again.
- Limits. One supervisor in the runtime holds `max_task_depth` (default 3) and `max_concurrent_tasks` (default 20) for each root. A negative value fails `Validate`. The depth comes from the `session.created` records of the ancestors. The count is the unsettled children of the tree that this runtime spawned. A child that a parent opens after a restart does not count. A spawn past either limit fails the tool call. `HARNESS_MAX_TASK_DEPTH` and `HARNESS_MAX_CONCURRENT_TASKS` set the keys through `ApplyEnv`.
- A client sends an input to a child, or interrupts it, as it does any session. There is no tree interrupt, no token budget, and no `send`, `status`, `cancel`, or `log` action. At the switch, the `contract_children` rows are the oracle; the cancel-tree row waits for `interrupt {tree}`.

### Shutdown

One `sync.WaitGroup` per runtime tracks every actor, turn runner, compaction, and `Sync` sender. `Runtime.Close` releases each session with cause `handoff` and waits for the group. It then closes the model connections, stops the processes, and closes the MCP servers. When its ctx ends first, it stops the remaining sessions without an append and returns; their next `Open` finds a crashed turn.

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
PUT    /sessions/{id}/goal                    {condition, max_turns}; 200 with the view
DELETE /sessions/{id}/goal                    204
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

`Runtime.Handler` serves these routes today: `POST` and `GET /sessions`, `GET` and `PATCH /sessions/{id}`, `POST /sessions/{id}/inputs`, `POST /sessions/{id}/interrupt`, `POST /sessions/{id}/compact`, `PUT` and `DELETE /sessions/{id}/goal`, `GET /sessions/{id}/events`, `GET /models`, and `GET /health`. Phase 4 adds the other routes; `requests` comes with phase 5 and `tree` with children. The handler has no authentication; the embedder wraps it.

Today harness has 36 routes and seven ways to read a session. This has one log and one cursor.

### Inputs

Request: `{id, parts, delivery, source?, expected_turn_id?}`. The client mints `id`.

| Case | Response |
| --- | --- |
| New id | `201 {input_id, seq}` |
| Same id, same body | `200` with the original receipt |
| Same id, other body | `409 input_conflict` |

### Events

- One per-session `seq` serves paging and SSE resume.
- Only SSE frames for durable records carry `id: <seq>`. `Last-Event-ID` and `after=` resume exactly, across processes.
- A live frame (`item.started`, `item.delta`, `status`) sets `protocol.Event.Ephemeral` and is never stored. Its `seq` is the last durable seq when it was sent.
- A subscriber reads durable records from the log, so a slow subscriber never misses one. A full subscriber drops ephemeral frames, so the deltas of an item can have holes; its `item.completed` holds the whole item. An error ends a stream with an `error` frame.
- Replication is `Options.Sync`: each `protocol.SyncBatch{epoch, session, from_seq, records, blobs}` is a remote append. The receiver rejects an older epoch. A batch with `from_seq` at its head plus one is appended. A batch whose records are all at or below its head is a retry: identical bytes are acknowledged as a duplicate, and different bytes are rejected. Any other `from_seq` is a seq mismatch. Every reply is a `protocol.SyncAck{head}`, including a seq mismatch, so the sender resends from `head+1` out of its own Store; that also heals a receiver that missed records before a crash. A stale-epoch rejection fires `Ownership.Lost`. `harness.ApplySync` implements these receiver rules over any `Store`.
- The epoch is a number because fencing needs order. An embedder maps its own claim to a monotonic epoch; boxes uses `claim_epoch`, and its string command ID stays the workflow token.

### Errors

Body: `{"error":{"code":"...","message":"...","details":{}}}`.

| Code | Status |
| --- | --- |
| `invalid_request` | 400 |
| `session_not_found` | 404 |
| `session_exists` | 409 |
| `request_not_pending` (phase 5) | 409 |
| `session_not_owned` | 409 |
| `input_conflict` | 409 |
| `turn_mismatch` | 409 |
| `session_busy` | 409 |
| `model_unavailable` | 409 |
| `payload_too_large` | 413 |
| `draining` | 503 |
| `internal` | 500 |

Each code except `internal` and `payload_too_large` is a sentinel error in `harness` and a `protocol` constant. `server` maps it with `errors.Is`. A body above 8 MiB fails with `payload_too_large`. Any other error is `internal`, and its message is a fixed string. A path or method that no route serves answers 404 or 405 with `invalid_request`.

### Contract source

Go types in `protocol` are the source. Generation is planned for phase 4. `go generate ./protocol` writes `protocol/openapi.json` and `protocol/protocol.ts` with `github.com/invopop/jsonschema` and a route table that `server` exports. CI regenerates and fails on a diff. A `server` test walks the route table against the mux. The hand-written `server/openapi.yaml` is deleted.

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
	Steering      bool     // accepts input mid-turn
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
}
```

Phase 5 adds the `Sink` method that opens a request.

- A model API backend runs one model call per `Run`. The loop runs the tools.
- A delegated backend (`claudecode`) runs the whole turn and reports items.
- `AllowedTools` holds tool names in one namespace. For a model API backend, they are the embedder tools. For a delegated backend, they are its built-in tools from `Capabilities.Tools` and the embedder tools, and any other name fails `Create`. An embedder tool with the name of a built-in tool also fails `Create`.
- Retry, the stall watchdog, and compaction read `Capabilities`. No code compares a provider name.
- A backend marks a failed model call with a `turn` sentinel. `ErrRetryable` (a 429, a 5xx, a truncated stream, a response with no output) calls the model again with backoff, up to `prompt_retries` times, when the call has recorded no item. `ErrContextOverflow` compacts (see Compaction). `ErrExhausted` ends the turn with cause `provider_exhausted`. Any other error fails the turn.
- The stall watchdog ends a model call that reports nothing for `stream_idle_timeout_s` (default 300, as Codex). A delta, an item, and `Sink.Alive` each reset it. `modelapi` calls `Alive` for each provider event with no delta, such as a keep-alive or a tool argument that still streams. The stall is retryable. A compaction summary has the same watchdog and no retry. A negative value turns it off. A backend with `OwnsLoop` has no watchdog: its own tools can run silently for a long time.
- `Result.MaxTokens` reports a response that the output cap cut off. The loop runs none of its tool calls and records an error result for each. `modelapi` records a tool call with arguments that are not valid JSON, such as arguments that the cap cut, with no arguments. The next call ends with a continuation message that the log never holds. After `max_tokens_continuations` (default 3) continuations, the next cut-off response fails the turn; 0 or less ends the turn at the first one. The count never resets in a turn.
- Private backend state is one `backend.state` event plus a blob, one blob key for each owner. The Claude Code transcript mirror is that blob. The runtime has none of the eight `claudeCode*` fields of the engine or their record kinds.
- Model metadata comes from `modelmeta`. An unknown model fails with `model_unavailable` at create and at a settings change.
- `Telemetry` carries usage and the context reading. Cost and subscription quota are not built yet.

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

Delegating a turn to another agent harness is permanent. Claude Code is built; phase 5 adds the Codex CLI and the Requests row. Each one is a `Backend` with `OwnsLoop`, `OwnsContext`, and `Steering` set. They share one adapter contract, so adding a harness adds one package and touches nothing else.

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

### Warm-up

Warm-up is planned for phase 5. `turn` declares an optional `Warmer` interface: `Warm(ctx context.Context, req Request) error`. The session calls it once on create and on wake, fire-and-forget under the session context. Only `internal/backend/modelapi` implements it, through an optional `Warm` method of the client, for the Codex websocket transport. The first turn uses the warm connection if it is ready. No warm-up state lives on the session.

## tool, prompt, and config

### tool

There is no tool registry. The tools of a runtime are a `[]turn.Tool`, and a `turn.Source` adds the tools that can change between model calls.

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

- `New` builds the tool list from `Options.Tools` and, with a `WorkDir`, the `process` tool. An empty or repeated name fails `New`. The built-in tools of a `WorkDir` belong to each session; see "Built-in tools".
- When a turn starts, `turn.Restrict` applies the session's `AllowedTools`. The `Source` gets the same list for each model call.
- `internal/tool/mcpsrc` and `internal/tool/pluginsrc` are the `Source`s, joined by `turn.Sources`.
- The loop runs the tool calls of a response one at a time, in order. A tool error, or a call to a tool that the model may not call, is an error result that the model sees.
- No tool receives a session.

Agent profiles name a kind of child, in the agent format of Claude Code so one file serves every backend: `name`, `description`, `tools` (comma separated; omitted allows every tool of the parent), `model` (a ref or an alias; omitted or `inherit` keeps the model of the parent), and a prompt body. `color` is read and ignored; any other key skips the file. `internal/prompt.Profiles` reads `<workdir>/.agents/*.md` at each spawn, beside the built-in profiles, which a file of the same name replaces. The built-in `general-purpose` allows every tool of the parent. The built-in `explore` and `plan` allow only the read-only file tools, and `plan` asks for an implementation plan. Tool names differ by backend, so a spawn keeps only the names of a profile that the child has: a runtime tool or a built-in tool of its model. A profile left with no tool logs a WARN line. A file that is not valid, or that repeats a name, is skipped with a WARN log line. A profile applies through the allowed tools of the child (`turn.Restrict`) and its body as the last segment of the system prompt of the child, which the child reads at its first turn after it opens.

### Built-in tools

With a `WorkDir`, each session of the harness loop gets the built-in tools of the engine, with their behavior, limits, and text: `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `ls`, `bash`, and `read_tool_result`. An embedder tool or a plugin tool with one of these names fails. `session_info` and `model` are not in this set; see Open questions. Without a `WorkDir`, a session has none of them, and no result is retained. Switch oracle: the `file_tools_*` rows; `internal/tool/builtin` runs their calls with the texts of their goldens.

- `session.Config.Agent` gives each session its own file tools, because the `write_file` guard belongs to one session: `write_file` overwrites only a file that the session read or wrote, with no change on disk since. As in the engine, the guard is in memory, so after `Open` the model reads a file again before it overwrites it. The tools cannot be one `Options.Tools` list for the runtime.
- The actor builds `read_tool_result` over itself, so the tool gets its dependencies when it is built.
- A backend that owns the loop, such as Claude Code, gets none of them and has no retention. It has its own tools.
- One file-size cap of 20 MiB replaces the read budget of the engine. `read_file`, `edit_file`, and the `write_file` guard read at most that many bytes of a file and fail on a larger one. Tools run one at a time, so the cap bounds the memory of the reads.
- `read_file` reads an image as one summary line, because the log has no image part. `bash` gets no `shell.env` additions, because the runtime does not dispatch that hook.

### Tool-result retention

`internal/toolresult` holds the retention of the engine and `read_tool_result`, with its limits and text.

- A tool result above 16384 bytes has its secrets masked, goes to a blob, and the history holds a header with a `trh_N` handle and the first 16384 bytes. A result that fits after the mask stays inline. `read_tool_result` reads the blob back by line window or literal search, bounded by `max_bytes`. Its own result is never retained.
- The `turn.Source` of an agent turn adds a `turn.Hooks` that retains each result after the plugin hooks, so the blob holds the text that the model would see. The turn records the preview, and the next model call of the turn never carries the whole result. The blob is written outside the actor, and the record is appended only while the turn runs.
- A `tool_result.retained` record names the blob, and `Sync` carries the blob with the record, as for `backend.state`. The handle numbers count these records, so a replay and the next owner continue the count. The blob key is the handle and the fence seq, so a fenced owner never overwrites the blob of the next owner.
- A result that would take the retained total of the session above 4 MiB keeps its preview with a notice and no handle, and nothing is written. A failed write keeps the whole result.
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
- Prompt. Each model call runs `system.transform` with the model of that call, and adds its segments after the MCP segment.
- Events. `session.Config.Appended` gives each appended event to the plugins of the session. `turn.started` and `turn.resumed` send `session.status` busy. `turn.suspended` and `turn.ended` send idle, and a failed turn first sends `session.error`. The tool hooks send `tool.execute.start` and `tool.execute.end` around each call that runs, and `file.edited`, with an absolute path, after a `write_file` or `edit_file` call that succeeds. Delivery is best effort, as in the engine.
- Client API. `client/session.messages` reads the history of the session from the store. `client/mcp.call` and `client/generate` fail.
- The runtime does not dispatch `chat.params`, `chat.message`, or `shell.env`. No contract row pins them, and no boxes plugin uses them.
- Switch oracle: the `plugin_*` rows. `serve_url` and `run_token` stay empty until the phase 4 switch.

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

Each turn sends the prompt with one process status line after it, built when the turn starts. See "processes". Each model call then adds the MCP segment, then the plugin segments. See "MCP tools" and "plugins". There is no outline mode, no chain ceiling, and no other ambient segment. The base prompt says that the status follows the system prompt. The engine keeps its own sentence, about the newest user message, through `prompt.EngineBase` until phase 6. Tools run one at a time, so the tool-batching segment is gone. At the switch, the `runtime_prompt` contract rows change in three ways: the `instructions_mode` and outline rows go, a bad file degrades instead of failing the turn, and no batching segment follows the base prompt.

### processes

With a `WorkDir`, the runtime builds one `process.Manager` from `Config.Processes` and adds the `process` tool over that manager. Without a `WorkDir`, no process runs and the model sees no `process` tool. With a `WorkDir`, an embedder tool named `process` fails `New`.

`declare` runs any argv that the model names. A `WorkDir` therefore grants command execution equal to `bash`, even when `Options.Tools` has no shell tool. An embedder that wants only AGENTS.md discovery must not set `WorkDir`.

- The tool description names the configured processes only, so it never changes while the runtime runs. `declare` adds a process in memory until the runtime closes.
- A tool error is the error of the manager, with one `process:` prefix.
- A result reports no elapsed time. The status line names instants instead.
- When a turn starts, the session appends one status line to its system prompt: `[processes: dev ready :3000 since <RFC 3339> log=.harness/proc/dev.log]`, one entry for each process that has started. The line is inside `<harness-engine-context>` tags, so the base prompt marks it as trusted. An instant changes only when a process changes state, so the line is stable for the turn and for each later turn with no process change. The log never holds it.
- This deviates from the engine, which puts the status at the end of the newest user message. Each process change (a start, a restart, ready, a stop, or an exit) changes the system prompt of the next turn. That turn misses the prompt cache for the whole history, and on the OpenAI WebSocket path it sends the full input instead of a suffix. A process that exits during a turn shows in the next turn. The cost is one cache miss for each process change, which keeps one prompt for each turn.
- `Runtime.Close` stops every process after the sessions end. When its ctx ends first, it cancels the turns and kills the processes before it returns.
- The `/processes` routes come with the phase 4 switch. Switch oracle: `process_tool_from_the_model`. Its two double-prefix rows change by design.

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
| Tool-result retention, `read_tool_result` | Keep; drop the size knobs; port in phase 3 |
| Read budget | Replace with a file-size cap |
| Snapshots, index | Replace with `Apply` over the log |
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
| 3 | `harness/config` with `Defaults`, `Validate`, and `ApplyEnv`, on the standard library only; one `modelapi` backend for every model API wire; provider error classes, the stall watchdog, and max_tokens continuation in `turn`; goals as one state machine in `session`; the built-in tools, and large-result retention and `read_tool_result` in `internal/toolresult`; children, agent profiles, and the `task` tool | Boxes `BootConfig` |
| 4 | New HTTP and `protocol` generation. Scenario scripts carry over; their assertions move to the new API. One PR switches `cmd/harness`. | Boxes console adopts the harness shapes; boxes routes become thin forwarders. Same release. |
| 5 | Remaining backends on capabilities; `codexcli`; requests; `Warmer` | None |
| 6 | Delete `engine`, `server`, old formats, dead features; move leaves to `internal/` | None |

PR #359 closes unmerged; its design is in this doc. The meta home chat has no old data or routes, so it proves the new runtime before boxes switches. Phase 4 is a cutover, not an adapter: no old route, format, or Go API survives it.

## Open questions

- Does the switch port `session_info` and `model`? No contract row calls them, and Claude Code and Codex have neither. The contract goldens list both in the tool list of each request, so leaving them out changes those goldens at the switch.

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
