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
- A public Go API small enough to read in one sitting: `harness`, `harness/protocol`, `harness/storetest`, `harness/harnesstest`.
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

| Package | Owns |
| --- | --- |
| `harness` | `Runtime`, `Options`, `Session`, `Store`, `Owner`, `Ownership`, `DiskStore`, `MemStore`, sentinel errors |
| `harness/protocol` | Data types shared by the Go API and HTTP; source of the generated OpenAPI and TS |
| `harness/storetest` | Conformance suite for a `Store` implementation |
| `harness/harnesstest` | Scripted model server, so other modules test against the same wire fixtures |

The Go API:

```go
package harness

func New(opts Options) (*Runtime, error)

type Options struct {
	Config Config
	Store  Store // nil: DiskStore under Config.Dir
	Owner  Owner // nil: the local process owns every session
	Tools  []Tool // embedder tools, beside built-in, MCP, and plugin tools
	// ModelTransport returns the HTTP transport for a model provider.
	// nil, or a nil result: the default transport.
	ModelTransport func(provider string) http.RoundTripper
	Logger         *slog.Logger
}

// A Runtime hosts many sessions. Each runs only while its Ownership holds.
func (r *Runtime) Handler() http.Handler
func (r *Runtime) Create(ctx context.Context, req protocol.CreateSession) (*Session, error)
func (r *Runtime) Open(ctx context.Context, id string) (*Session, error) // acquire, fence, replay, resume
func (r *Runtime) List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error)
func (r *Runtime) Models() []protocol.Model
func (r *Runtime) Close(ctx context.Context) error // suspends every session with a handoff cause

func (s *Session) View() protocol.Session
func (s *Session) Submit(ctx context.Context, in protocol.Input) (protocol.Admitted, error)
func (s *Session) Interrupt(ctx context.Context, req protocol.Interrupt) error
func (s *Session) Resolve(ctx context.Context, requestID string, res protocol.Resolution) error
func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)
func (s *Session) SetGoal(ctx context.Context, g protocol.Goal) error
func (s *Session) ClearGoal(ctx context.Context) error
func (s *Session) Compact(ctx context.Context) error
func (s *Session) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
func (s *Session) Release(ctx context.Context) error // suspend and release ownership

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
| `internal/backend/anthropic`, `openai`, `openaicompat`, `claudecode` | Backend implementations |
| `internal/backend/httpx` | SSE reader, error classifier, retry policy, usage normalizer |
| `internal/backend/wirenorm` | Wire repair |
| `internal/message` | Conversation types used inside the runtime |
| `internal/modelmeta` | Context-window table from models.dev; exposed only through `Runtime.Models` and `GET /models` |
| `internal/tool` with `builtin`, `mcpsrc`, `pluginsrc` | Tool interface, registry, sources |
| `internal/toolresult` | Large-result retention and `read_tool_result` |
| `internal/prompt` | System-prompt segments and the `Memo` loader |
| `internal/mcp`, `plugin`, `skill`, `command`, `process` | Moved as is |

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
| `goal.set` | `condition`, `max_turns`, `deferred` |
| `goal.evaluated` | `turn_id`, `verdict`, `guidance?` |
| `goal.changed` | `state`, `reason?` |
| `compaction.applied` | `from_seq`, `to_seq`, `summary`, `by_backend` |
| `child.spawned` | `child_id`, `agent?` |
| `child.settled` | `child_id`, `outcome` (`done`, `failed`, `canceled`), `result_ref` |
| `context.measured` | `tokens`, `window`, `source` |
| `backend.state` | `backend`, `blob_key` |

Ephemeral frames go to subscribers and never to the store: `item.started`, `item.delta`, `status`. Each carries its `item_id` and the last durable seq, so a client can place it.

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

A turn ends early for one of three causes, carried by `context.WithCancelCause`:

| Cause | Trigger | Effect |
| --- | --- | --- |
| `stopped` | User interrupt | Keep the partial; unfinished tool calls get `interrupted` results; never resumes |
| `goal_cleared` | `ClearGoal` during a goal turn | Same as `stopped` |
| `handoff` | `Session.Release`, `Runtime.Close` | Tool calls stay open; the next owner resumes from the last completed item with the same call ids, up to `MaxTurnResumes` |

The log stays strictly append-only. A client hides output by cause if it wants to.

Input:

```
admitted ─► promoted   (queue: next turn; steer: next item boundary)
admitted ─► withdrawn
```

A `steer` input with `expected_turn_id` fails with `turn_mismatch` if that turn is not running. A `steer` input on an idle session starts a turn.

Goal:

```
armed ─► active ─► backoff ─► active
            └───► parked  ─► active
            └───► achieved | exhausted
any ─► cleared
```

- `armed` is a deferred goal. Its first evaluation judges the finished turn and consumes no turn.
- A goal is a completion condition. It never becomes a second instruction. `NOT MET` guidance is an input with `source: goal`.
- `max_turns` bounds `active`. Reaching it yields `exhausted`.

Request:

```
pending ─► answered | dismissed
```

Any backend can open a request. An input admitted while a request is open dismisses it first.

### Children

A child is a session with `parent_id`. The parent holds a handle, not the child's state. When a child turn ends, the child sends `child.settled` to the parent, and the parent admits the outcome as an input with `source: child`. Task notifications become inputs; the separate checkout-and-commit queue is deleted.

Tree limits (depth, concurrency, token budget) live in a per-root supervisor with its own state.

### Shutdown

One `errgroup` per runtime tracks every actor, turn runner, and writer. `Runtime.Close` suspends each session with a handoff cause and waits.

## HTTP

### Routes

```
POST   /sessions                              create; client id optional
GET    /sessions?parent=&cursor=              list
GET    /sessions/{id}                         view
PATCH  /sessions/{id}                         model, effort, service_tier
DELETE /sessions/{id}
POST   /sessions/{id}/inputs                  submit
GET    /sessions/{id}/inputs                  queued inputs
DELETE /sessions/{id}/inputs/{input}          withdraw
POST   /sessions/{id}/interrupt               {turn_id?, tree?}
POST   /sessions/{id}/compact
PUT    /sessions/{id}/goal                    {condition, max_turns, defer}
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
- SSE frames for durable records carry `id: <seq>`. `Last-Event-ID` and `after=` resume exactly, across processes.
- A slow subscriber gets a `gap` frame and a close. It never loses a record silently.
- The outbound event sink becomes a per-session pump. Each delivery carries `{session, from_seq, records}`. The receiver keeps one cursor per session.

### Errors

Body: `{"error":{"code":"...","message":"...","details":{}}}`.

| Code | Status |
| --- | --- |
| `invalid_request` | 400 |
| `session_not_found` | 404 |
| `request_not_pending` | 409 |
| `session_not_owned` | 409 |
| `input_conflict` | 409 |
| `turn_mismatch` | 409 |
| `model_unavailable` | 409 |
| `payload_too_large` | 413 |
| `draining` | 503 |

Each code is a sentinel error in `harness`. `server` maps it with `errors.Is`.

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
	OwnsLoop      bool // backend runs tools and multi-step turns
	OwnsContext   bool // backend compacts its own context
	Steering      bool // accepts input mid-turn
	ContextWindow int  // 0 means the backend reports it
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
- Retry, the stall watchdog, and compaction read `Capabilities`. No code compares a provider name.
- Private backend state is one `backend.state` event plus a blob. The Claude Code transcript mirror is that blob. The eight `claudeCode*` fields and their record kinds are deleted.
- Model metadata comes from `modelmeta`. An unknown model fails with `model_unavailable` at create and at a settings change.
- `Telemetry` carries usage, cost, the context reading, and subscription quota.

### Shared provider plumbing

`internal/backend/httpx` holds the SSE reader, `ClassifyResponse`, the retry `Policy`, and usage normalization. The three copies in the provider packages are deleted. The Codex websocket transport moves into `openai` as a `Transport`.

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

`turn` declares an optional `Warmer` interface: `Warm(ctx context.Context, req Request) error`. The session calls it once on create and on wake, fire-and-forget under the session context. Only `internal/backend/openai` implements it, for the Codex websocket transport. The first turn uses the warm connection if it is ready. No warm-up state lives on the session.

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

One `Config` struct. One defaults table. One `Validate`. Environment variables override fields by name; there are no env-only knobs.

## Feature disposition

| Feature | Verdict |
| --- | --- |
| Goals, deferred goals, parking | Keep; one state machine |
| Queue, `prompt_async`, `enqueue`, `send` | Merge into inputs |
| Task notifications | Merge into inputs with `source: child` |
| Structured questions (#317) | Generalize to requests |
| Claude Code delegated backend | Keep; first third-party harness |
| Codex CLI | Add through the third-party harness seam |
| Codex lane, websocket transport | Keep inside `openai` |
| Startup prewarm | Move into `openai` behind `turn.Warmer` |
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

- `harnesstest` is a public package. It serves the Anthropic Messages API from a script: text, tool call, error status, block until released.
- A scenario is a model script and a list of actions. Its golden holds the normalized model requests and the final transcript of each session, not event shapes, so the goldens survive the event-log change in phase 4.
- Scenarios run the real `harness serve` binary over loopback, in its own process group, through a driver interface. Each wait has a bound.
- Every scenario checks the invariants that hold in both formats: unique message ids, one result for each tool call, and contiguous event sequence numbers.
- The suite exists before any internal change. It pins today's behavior, so each later phase is checked against it.

Today ~70% of 127k test lines read unexported state and will not survive the restructure. The target is 40k–50k test lines.

### Unit tests

Unit tests cover pure code only: `Apply`, wire transcoders, `config`, `message`. A bug fix adds a table row, not a file. No test reads unexported state across packages.

### CI gates

| Gate | Threshold |
| --- | --- |
| Whole-line comments per file | Warn above 15%; fail above 25%. The `doc.go` package comment does not count |
| History markers in comments | Fail on issue numbers, dates, "previously", "no longer", "red-verified", "confirmed live", "an earlier version", "before this change", "round N", review or fix rounds, and "copilot" |
| File size | Fail above 800 lines |
| Function size | Fail above 80 lines |
| Test lines vs code lines per package | Fail above 1.5 test lines per code line, unless the ratio does not rise over the merge base. A new package has no base |
| `time.Sleep`, `time.After` in tests | Fail in a test file. `internal/testpoll` is not a test file |
| `AGENTS.md` length | Fail above 80 lines at the root and 25 lines in a scoped file |
| Merge-base diff | `TestRepository` checks only files and packages that differ from `git merge-base HEAD origin/main`, or from `$GATES_BASE_REF`. A new file meets each limit above. A changed file may not cross a limit that it met, and may not get worse on a limit that it already broke. An unchanged file is not checked. No baseline file exists |
| Protocol drift | Regenerate; fail on a diff |
| Imports | `depguard`: internal packages never import `server` or `cmd` |
| Lint | `govet`, `staticcheck`, `errcheck`, `unused`, `revive` |

Gates compare a branch with its merge base, so old code never blocks a change and new code starts strict. `AGENTS.md` shrinks to these gates and the four rules.

## Migration

Each phase is one or more PRs on `main`. Each ships alone.

| Phase | Work | Consumers |
| --- | --- | --- |
| 1 | Contract suite: scenario scripts and `harnesstest`; CI gates that diff against the merge base; new `AGENTS.md`; delete history comments | None |
| 2 | New runtime core beside the old engine, in the order meta needs it: `harness.Store` and `storetest`; `Owner`; `Runtime`, `Session.Submit`, `Events`; a native backend with `ModelTransport` (Codex first); `harness.Tool` and `Restrict`; turn resume and stop causes; the `external` adapter and `claudecode`. Absorbs the design of PR #359, its conformance suite, and its `fakeclaude` modes. | The meta home chat embeds it; it is the first consumer |
| 3 | Leaf cleanups: `httpx`, `wirenorm`, `tool` registry, `prompt.Memo` and agent profiles; move leaves to `internal/` | None |
| 4 | New HTTP and `protocol` generation. Scenario scripts carry over; their assertions move to the new API. One PR switches `cmd/harness`. | Boxes console adopts the harness shapes; boxes routes become thin forwarders. Same release. |
| 5 | Remaining backends on capabilities; `codexcli`; requests; `Warmer` | None |
| 6 | Delete `engine`, `server`, old formats, dead features | None |

PR #359 closes unmerged; its design is in this doc. The meta home chat has no old data or routes, so it proves the new runtime before boxes switches. Phase 4 is a cutover, not an adapter: no old route, format, or Go API survives it.

## Open questions

None remain.

Decided:

- Claude Code delegation is permanent, and the Codex CLI follows it through the same seam (see Third-party harnesses).
- `interrupt` stops the running turn only. The next queued input then starts, and an active goal keeps running, as in Claude Code. `interrupt` replies after the turn has stopped.
- Workspace inspection stays in harness as `GET /workspace/changes`, in an isolated `internal/workspace` package.
- Worktrees are deleted.
- Agent definitions stay as agent profiles in Claude Code's format.
- Startup prewarm moves into `internal/backend/openai` behind `turn.Warmer`.
- The Modal guide and script are deleted.
- PR #359 is folded into phase 2; the meta home chat is the first consumer of the new runtime.
- The boxes console adopts the harness API shapes; boxes routes forward.
- Embedders add in-process tools through `harness.Tool`; `call.ID` is stable across resume.
- Embedders inject model credentials through `Options.ModelTransport`.
- Every early stop keeps the partial; clearing a goal is a stop with cause `goal_cleared`.
