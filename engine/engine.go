// Package engine is the headless core: the session loop that streams model
// turns, executes tool calls, and appends everything to the session's
// message history. Every frontend (CLI, TUI, server) is a client of this
// package; none of them are imported by it.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// Hooks is the slice of the plugin host the engine uses. *plugin.Host
// satisfies it; tests use fakes. A nil Hooks disables all hook dispatch.
//
// Every method MUST be safe for concurrent use, and CROSS-CALL ORDER IS
// NOT GUARANTEED. One assistant message's tool calls run as a concurrent
// batch (toolexec.go), so ToolExecuteBefore/ToolExecuteAfter/ExecuteTool
// for two different calls can be in flight at the same moment, and
// before(B) can precede before(A) for calls the model listed as A then B.
//
// What IS guaranteed is PER-CALL order: for any one call id, before runs,
// then the tool, then after. After-hooks across sibling calls are
// completion-ordered.
//
// This is a change from the pre-parallel engine, where call A's whole
// before/tool/after sequence finished before call B started. A hook
// implementation that keeps cross-call state — a running quota, an audit
// chain, a policy that reads the previous call — must key that state by
// call id or serialize itself. The same contract binds an out-of-process
// plugin: its hook dispatches ride the connection plugin/PROTOCOL.md
// specifies, which is id-multiplexed and already carries several requests
// in flight at once, so a plugin must never assume its hooks are called
// one at a time. A deployment that cannot adapt sets
// HARNESS_SEQUENTIAL_TOOLS=1, which restores one-at-a-time execution and
// with it the old hook order.
type Hooks interface {
	ChatParams(ctx context.Context, req *plugin.ChatParamsRequest) plugin.ChatParams
	SystemTransform(ctx context.Context, req *plugin.SystemTransformRequest) []string
	ShellEnv(ctx context.Context, req *plugin.ShellEnvRequest) map[string]string
	ToolExecuteBefore(ctx context.Context, req *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string)
	ToolExecuteAfter(ctx context.Context, req *plugin.ToolExecuteAfterRequest) message.Parts
	ExecuteTool(ctx context.Context, req *plugin.ToolExecuteRequest) (*plugin.ToolExecuteResponse, error)
	Emit(events []plugin.Event)
	Tools() []plugin.ToolDef
	// Plugins reports the configured plugins — name, spawn state, registered
	// tools, and subscribed hooks — for the session_info tool and GET
	// /session. It lists CONFIGURED plugins, not only spawned ones.
	Plugins() []plugin.Info
}

// Tool is a built-in (in-process) tool.
//
// Serial and Key both default to their Go zero value (false, nil), which is
// exactly today's behavior for every existing built-in and for every
// plugin/MCP tool (neither ever sets them) — see toolexec.go for the batch
// executor that reads them.
type Tool struct {
	Def provider.ToolDef
	Run func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error)

	// Serial marks this tool as a BARRIER inside one assistant message's
	// batch of tool calls (see toolexec.go's splitBatch). A batch splits
	// into runs around a Serial call: every call before it finishes before
	// it starts, and it finishes before any call after it starts. Set true
	// only for a tool that mutates session-wide state in a way a plain
	// s.mu-guarded write cannot make safe to interleave with a sibling call
	// — e.g. a model or goal swap mid-batch. See mcpTool, modelTool,
	// goalTool for the three built-ins that set it, each with a one-line
	// comment naming the state it mutates.
	Serial bool

	// Key, when non-nil, computes a resource key from the running session
	// and one call's raw arguments. Two calls in the same batch that
	// produce the same non-empty key run mutually exclusive, in CALL
	// order — never concurrently with each other, though still
	// concurrently with calls that key differently or key empty. An empty
	// string means "no key": the call runs alongside everything else in
	// its segment, exactly as today.
	//
	// Key takes the Session (not just args) because a path-based key must
	// resolve a relative path against the session's own working directory
	// (s.resolvePath) to match an absolute path naming the same file — see
	// editFileTool/writeFileTool/readFileTool. Key must never panic; a
	// tool whose args fail to parse must return a key it has chosen
	// deliberately, never a panic. The two built-in shapes differ on
	// purpose: filePathKey returns a FIXED fallback key, so every
	// unparseable file call serializes against every other one, while
	// processToolKey returns "" (no key), because an unparseable process
	// call cannot collide with a real process name and runProcessTool
	// rejects it before touching any process anyway.
	Key func(s *Session, args json.RawMessage) string
}

// Event is one entry in the session's event stream. Event types follow ACP
// naming where a choice is arbitrary (see AGENTS.md).
type Event struct {
	Type       string              `json:"type"`
	SessionID  string              `json:"session_id"`
	Text       string              `json:"text,omitempty"`
	Message    *message.Message    `json:"message,omitempty"`
	ToolCall   *message.ToolCall   `json:"tool_call,omitempty"`
	Output     message.Parts       `json:"output,omitempty"`
	IsError    bool                `json:"is_error,omitempty"`
	Usage      *provider.Usage     `json:"usage,omitempty"`
	StopReason provider.StopReason `json:"stop_reason,omitempty"`

	// Model is carried by EventModelChanged only: the session's new model
	// after a SetModel call that actually changed it (see SetModel). It is
	// the ONE engine event a model swap emits, whatever the swap route —
	// the `model` tool, a per-request prompt override, or POST
	// /session/{id}/model — so the server journals each swap through a single
	// path (see server/journal.go's EventModelChanged case). It is distinct
	// from the durable recModel resume record persistModel writes (store.go),
	// which restores the model on LoadSession.
	Model message.ModelRef `json:"model,omitzero"`

	// Effort is carried by EventEffortChanged only: the session's new
	// reasoning-effort level after a SetEffort call that actually changed it
	// (see SetEffort). It is the single event an effort swap emits, whatever
	// the route, so the server journals each swap through one path (see
	// server/journal.go's EventEffortChanged case). It is distinct from the
	// durable recEffort resume record persistEffort writes (store.go), which
	// restores the effort on LoadSession.
	Effort message.Effort `json:"effort,omitempty"`

	// Goal-loop fields (set on goal.* events; see goal.go and the state
	// machine documented atop goal.go). GoalCondition is carried by
	// goal.set and goal.updated (the new condition); GoalReason/GoalMet/GoalTurn by goal.eval; GoalReason/GoalTurn
	// by goal.stalled (GoalAttempt is the 1-based retry attempt);
	// GoalReason/GoalTurns by goal.achieved; goal.cleared carries GoalReason
	// when it was triggered by a permanently-failing worker turn, empty for
	// an ordinary caller-initiated clear.
	//
	// GoalRetryable/GoalRetryableClass/GoalWaiting are also carried by
	// goal.stalled (see GitHub issue #61 and promptTurnWithRetry):
	// GoalRetryable is true when the failure was classified provider-
	// retryable weather (provider.RetryableError) rather than a
	// deterministic failure; GoalRetryableClass names the classification
	// (provider.RetryableClass — overloaded/rate_limited/server_error);
	// GoalWaiting is true while still within the retryable budget (still
	// "waiting out provider weather") and false on the final stalled record
	// that reports the budget exhausted (the turn is about to park, not
	// die — see PursueGoal's doc comment). All three are zero-valued on a
	// deterministic-path stall, unchanged from before they existed.
	//
	// GoalEvalFailures is carried by goal.eval_failed only (see goal.go's
	// "Round 6" doc section and evaluateGoal/recordGoalEvalFailed): the
	// number of CONSECUTIVE failed evaluator boundaries as of this one,
	// inclusive — reset to zero the moment a later boundary parses a
	// verdict (MET or NOT MET) or the generation changes (an UpdateGoal),
	// so it measures a streak against one condition, never a cumulative
	// total. goal.cleared itself never carries a count — even the terminal
	// clear that fires once this reaches goalEvalFailureLimit — its
	// dedicated GoalReason text names the limit instead (see
	// server/journal.go's GoalEvalFailures doc comment for the mirrored
	// server-side fold).
	GoalCondition      string `json:"goal_condition,omitempty"`
	GoalReason         string `json:"goal_reason,omitempty"`
	GoalMet            bool   `json:"goal_met,omitempty"`
	GoalTurn           int    `json:"goal_turn,omitempty"`
	GoalTurns          int    `json:"goal_turns,omitempty"`
	GoalAttempt        int    `json:"goal_attempt,omitempty"`
	GoalRetryable      bool   `json:"goal_retryable,omitempty"`
	GoalRetryableClass string `json:"goal_retryable_class,omitempty"`
	GoalWaiting        bool   `json:"goal_waiting,omitempty"`
	GoalEvalFailures   int    `json:"goal_eval_failures,omitempty"`
	// GoalAttempts is carried by goal.parked only (see goal.go's
	// recordGoalParked and "Round 7" doc section): the TOTAL attempt count
	// for the exhausted turn, distinct from GoalAttempt (singular), which
	// is goal.stalled's 1-based per-attempt counter. GoalReason on a
	// goal.parked event is classified, never raw provider error text (see
	// classifyGoalWorkerError) — GoalRetryable/GoalRetryableClass above are
	// reused unchanged from goal.stalled's convention.
	GoalAttempts int `json:"goal_attempts,omitempty"`

	// Compaction fields (see compact.go and docs/design/context-
	// compaction.md §4 "Live event surface"). Carried on
	// EventHistoryCompacted: CompactFirstID/CompactLastID name the folded
	// range, CompactTurnsFolded is the fold count, and CompactSummaryID
	// names the summary message (already delivered via a preceding
	// EventMessage — see Session.Compact). EventCompactionFailed carries
	// only Text (the error detail).
	CompactFirstID     string `json:"compact_first_id,omitempty"`
	CompactLastID      string `json:"compact_last_id,omitempty"`
	CompactTurnsFolded int    `json:"compact_turns_folded,omitempty"`
	CompactSummaryID   string `json:"compact_summary_id,omitempty"`

	// Prompt-queue fields (set on EventPromptQueued/EventPromptDequeued; see
	// queue.go). QueueID is the queue-assigned, session-monotonic prompt ID.
	// QueueText is the queued prompt text, carried on BOTH events (not just
	// queued) so a dequeued event is self-describing without cross-
	// referencing the matching queued one. QueueReason is empty on
	// EventPromptQueued and one of "delivered" (idle drain, Task 3),
	// "injected" (goal-turn-boundary interjection, Task 2), or "cleared"
	// (DELETE /session/{id}/queue, Task 3) on EventPromptDequeued. QueueLen
	// is the queue's length immediately AFTER this event.
	QueueID     int64  `json:"queue_id,omitempty"`
	QueueText   string `json:"queue_text,omitempty"`
	QueueReason string `json:"queue_reason,omitempty"`
	QueueLen    int    `json:"queue_len,omitempty"`
	// QueueSeq is the caller-issued idempotency sequence on an
	// EventPromptQueued emitted by EnqueuePromptDurable (see queue.go);
	// 0/omitted on plain enqueues and on every EventPromptDequeued.
	QueueSeq int64 `json:"queue_seq,omitempty"`
}

// Event types.
const (
	EventTextDelta      = "text.delta"
	EventReasoningDelta = "reasoning.delta"
	EventMessage        = "message"
	EventToolStart      = "tool.start"
	EventToolEnd        = "tool.end"

	// EventTurnRestart fires when streamTurnWithRetry (prompt_retry.go) is
	// about to retry a failed model call whose stream may have already
	// emitted partial EventTextDelta/EventReasoningDelta content (a mid-text
	// stream_truncated or server_error). The next attempt re-streams that
	// content from scratch, so this marker tells a subscriber that renders
	// deltas incrementally (a TUI or SSE client) to DROP the partial content
	// it accumulated since the last EventMessage. Without it, the client
	// renders the failed attempt's partial text immediately followed by the
	// retry's full text — e.g. "Hello wor" then "Hello world" shown as
	// "Hello worHello world". It carries no payload beyond SessionID; the
	// authoritative message still arrives on the turn's final EventMessage.
	EventTurnRestart = "turn.restart"

	// EventModelChanged fires once per SetModel call that actually changes
	// the session's model (never on a no-op set to the current model). It
	// carries the new model in Event.Model and is the single observability
	// event every model-swap route funnels through (see SetModel).
	EventModelChanged = "model.changed"

	// EventEffortChanged fires once per SetEffort call that actually changes
	// the session's reasoning-effort level (never on a no-op set to the
	// current level). It carries the new level in Event.Effort and is the
	// single observability event every effort-swap route funnels through
	// (see SetEffort).
	EventEffortChanged = "effort.changed"

	// Goal-loop events (see goal.go).
	EventGoalSet      = "goal.set"
	EventGoalUpdated  = "goal.updated"
	EventGoalEval     = "goal.eval"
	EventGoalStalled  = "goal.stalled"
	EventGoalAchieved = "goal.achieved"
	EventGoalCleared  = "goal.cleared"
	// EventGoalEvalFailed fires once per failed evaluator boundary — a
	// provider error the retryable-class in-boundary retry couldn't ride out,
	// or two consecutive unparseable replies — see goal.go's "Round 6" doc
	// section. Below goalEvalFailureLimit consecutive failures this is
	// advisory only: the goal stays active and the loop keeps working; at
	// the limit a goal.cleared with a dedicated reason follows instead.
	EventGoalEvalFailed = "goal.eval_failed"
	// EventGoalParked fires once per exit-parked worker turn — either
	// exhaustion tier (deterministic or retryable-class, see goal.go's
	// "Round 7" doc section) — WITHOUT a following goal.cleared: the goal
	// stays active. A server (Task 2) maps this onto a distinct paused
	// presentation and resumes the loop on the next ordinary activity,
	// exactly like it already does for the boot-only restart pause.
	EventGoalParked = "goal.parked"

	// Prompt-queue events (see queue.go and docs/plans/2026-07-19-prompt-
	// queue.md). EventPromptQueued fires on every EnqueuePrompt call;
	// EventPromptDequeued fires on every DequeuePrompt/dequeueAllLocked pop,
	// whatever the reason (delivered/injected/cleared).
	EventPromptQueued   = "prompt.queued"
	EventPromptDequeued = "prompt.dequeued"
)

// SessionSyncFsync and SessionSyncVolume are the two accepted values of
// Config.SessionSync (see its doc comment) — SessionSyncFsync also names
// the zero value's effective behavior.
const (
	SessionSyncFsync  = "fsync"
	SessionSyncVolume = "volume"
)

// Config configures a Session.
type Config struct {
	Providers provider.Registry
	Model     message.ModelRef // initial model; swap any time with SetModel
	Effort    message.Effort   // initial reasoning-effort level; swap with SetEffort (zero = provider default)
	System    []string         // base system prompt segments
	MaxTokens int              // per-response cap; defaults to 8192
	WorkDir   string           // working directory for built-in tools

	// SessionDir is where session logs are persisted, one JSONL file per
	// session. Empty disables persistence entirely.
	SessionDir string

	// SessionSync selects the durability mechanism ensureLog and
	// EnqueuePromptDurable use for attested session-store writes (see
	// store.go/queue.go). SessionSyncFsync ("fsync", also the zero value's
	// effective behavior) fsyncs the log file and, on first creation, its
	// directory — correct for local POSIX filesystems. SessionSyncVolume
	// ("volume") skips both fsync round-trips entirely — no syscall, no
	// phase event — for stores on continuously-synced network volumes whose
	// own commit layer is the documented durability boundary: fsync adds no
	// durability there, and some FUSE/9p transports deadlock permanently on
	// it (fsync(dirfd) especially — see docs/deploy-modal.md). The write(2)
	// calls and all torn-write healing/replay logic (store.go's tail repair,
	// queue.go's last-writer-wins fold) are identical in both modes; only
	// the two fsync round-trips are gated. config.Config.SessionSync is the
	// single validation point for this string (see its doc comment) — an
	// unrecognized value here is a caller bug, not a runtime-checked
	// condition, so it is treated as SessionSyncFsync rather than rejected.
	SessionSync string

	// EngineVersion is the build version string of the serving process
	// (cmd/harness's main.version, ldflags -X'd at release build time,
	// "0.1.0-dev" in an unflagged dev build). Rendered into the ambient
	// engine-identity block (see identityStatusSegment) alongside
	// SessionSync and StartedAt, giving a session first-class, zero-tool-call
	// answer to "what engine am I running under" — the honesty gap that
	// makes `harness version` in bash or grepping the serve log's boot line
	// unreliable is that the binary on disk and the process that logged at
	// boot can both diverge from the currently-serving process. Empty (the
	// zero value — e.g. an embedder that builds Config directly, bypassing
	// cmd/harness) omits the version clause from the block rather than
	// rendering a placeholder; it never suppresses the whole block, since
	// SessionSync's effective mode and StartedAt (when set) are still worth
	// reporting on their own.
	EngineVersion string

	// StartedAt is the serving process's start time, threaded through by
	// cmd/harness (captured once, at the top of serveCmd/runCmd, before any
	// session is created or loaded) so every session under that process
	// reports the SAME instant regardless of when the session itself was
	// created or resumed. Rendered into the ambient engine-identity block
	// (see EngineVersion's doc comment) as a UTC RFC3339 timestamp. Zero
	// (the default) omits the started clause from the block.
	StartedAt time.Time

	// ParentSession is an opaque provenance pointer to the session this one
	// continues from — a re-dispatch after a failed goal, a follow-up fix
	// picked up on a fresh box. It is set once at creation (like WorkDir),
	// persisted on the session header record, and restored by LoadSession
	// (see store.go); it is NOT required to name a session that exists on
	// this server or in this process at all — lineage may cross boxes, so
	// the engine never validates or dereferences it. Empty means no
	// lineage. See Session.ParentSession.
	ParentSession string

	// TaskParentID is a COMPLETELY DIFFERENT concept from ParentSession
	// above, despite both being "a parent pointer": this one is
	// SessionManager's OWN tree-lineage record, set ONLY by Spawn
	// (session_manager.go) to the spawning session's id, and consulted
	// ONLY to reconstruct a child's true depth after SessionManager's
	// in-memory tree has forgotten it (Reap, or a process restart) —
	// see SessionManager.adoptReloadedLocked. ParentSession is an
	// opaque, unvalidated, cross-box-safe provenance pointer a caller
	// may set to ANY prior session id for ANY reason (see its own doc
	// comment) — reusing it for tree reconstruction would be both
	// semantically wrong and unsafe: a session created with
	// ParentSession pointing at some OTHER, unrelated but currently
	// SessionManager-tracked session would be silently misattached
	// under it. TaskParentID has no such ambiguity: it is written by
	// exactly one code path, read by exactly one code path, and never
	// surfaced on the wire (session.info's lineage.parent_id comes from
	// SessionManager's live tree, not this field). Persisted on the
	// session header record like ParentSession, restored by
	// LoadSession. Empty means "not a task-tool-spawned child, or
	// predates this field."
	TaskParentID string

	// TaskAgentType and TaskToolNames are set ONLY by Spawn, alongside
	// TaskParentID above, and exist for the SAME reason: SessionManager's
	// in-memory tree (and the in-memory *Session.tools map restrictTools
	// narrowed at spawn time) can both be forgotten — Reap, or a process
	// restart — while the underlying session itself stays perfectly
	// resumable via a legitimate follow-up (session.send permits
	// messaging a done/failed child). Without a durable record of what
	// this child's tool set was ACTUALLY restricted to, a reload
	// (adoptReloadedLocked) had nothing to restore it from and silently
	// handed back the full, unrestricted default registry — an explore
	// or plan child regaining bash/write_file after a reap or restart. A
	// live review caught this exact escalation.
	//
	// TaskAgentType is opts.AgentType verbatim (e.g. "explore") — kept
	// primarily for lineage.agent_type display surviving a reload, and
	// as a best-effort re-resolution key if TaskToolNames is ever
	// missing on an otherwise-named record (a legacy log predating this
	// field, or one written between the two fields' rollout — see
	// adoptReloadedLocked). TaskToolNames is the actual RESOLVED name
	// list restrictTools was called with at spawn time — nil means
	// "spawned with no restriction beyond whatever it inherited" (a
	// general-purpose-shaped child), a real durable value, not "missing
	// data" (a genuinely unrestricted child and a legacy record with no
	// data at all are, unavoidably, indistinguishable by this field
	// alone — TaskAgentType is the fallback signal for that case).
	// adoptReloadedLocked is the ONLY reader; this is never surfaced on
	// the wire.
	TaskAgentType string
	TaskToolNames []string

	// TaskDepth is the child's tree depth: a root's own children are depth
	// 1, their children depth 2, and so on. Spawn sets it, alongside
	// TaskParentID/TaskAgentType/TaskToolNames above, and persists it the
	// same durable way. See adoptReloadedLocked's own doc comment for why
	// this exists. SessionManager derives a LIVE node's depth as
	// parent.depth+1 at adopt time. But a reload can find this child's OWN
	// parent NOT currently tracked — Reap, or a process restart that
	// hasn't touched the parent again yet. There was previously no durable
	// fallback for that case at all. adoptReloadedLocked substituted
	// m.maxDepth instead: a deliberate "refuse further spawning" REFUSAL
	// SENTINEL, indistinguishable on the wire from a session genuinely AT
	// that depth. A live audit caught this exact collision: a direct
	// child, true depth 1, reported lineage.depth 3 (DefaultMaxTaskDepth).
	//
	// 0 means "not recorded". Every real child's depth is >= 1; only the
	// durably-blank case and a genuine root are ever 0 (a root never reads
	// this field at all). This is the same "legacy header, restores to the
	// Go zero value" rule TaskAgentType's own doc comment already
	// establishes. An already-recorded session predating this field
	// degrades to the OLD sentinel-fallback behavior instead of reporting
	// a false 0.
	//
	// GET /session/{id}.lineage.depth reports SessionManager's own
	// enforcement-effective depth verbatim, not this field re-derived a
	// second time on the wire — see server.lineageJSONFor's own doc
	// comment ("Depth:" paragraph) for why the two must never be allowed
	// to disagree.
	TaskDepth int

	Hooks Hooks // optional plugin host
	// OnEvent is optional; called synchronously, keep it fast. The goal.*
	// events (see goal.go) are emitted while Session.mu is held so the event
	// stream can never invert relative to the journaled log order under a
	// concurrent ClearGoal/evaluation race — so OnEvent must NEVER call back
	// into the Session that raised the event (Prompt, ClearGoal, ActiveGoal,
	// etc.), which would deadlock on that same mutex.
	//
	// The engine MAY call OnEvent from SEVERAL goroutines AT ONCE: one
	// assistant message's batch of tool calls now runs concurrently (see
	// toolexec.go), and EventToolStart/EventToolEnd fire from whichever
	// goroutine is running that call, so two calls in one batch can each be
	// inside OnEvent at the same moment. This was already true before
	// toolexec.go existed, for a different reason — a `task` child's own
	// background turn (SessionManager.Spawn) can call the SAME OnEvent
	// concurrently with its parent's turn, since configSnapshot copies the
	// func value by value into every child's Config (see
	// TestRunOnEventHandlerSerializesConcurrentCallers, cmd/harness). A
	// caller whose OnEvent is not naturally reentrant (writes a shared
	// buffer, encodes to one io.Writer) MUST serialize its own body — see
	// server.Publish/publishLive (routes every event through a session-
	// keyed s.mu) and cmd/harness's newRunOnEventHandler (wraps its body in
	// a mutex) for the two in-repo patterns.
	OnEvent func(Event)

	// OnStorePhase, when non-nil, receives one call per ENDED phase of the
	// durable store paths (op "ensure_log": phases "mkdir", "open", "stat",
	// "tail_repair" (only when repair ran), "header_write" (only on
	// fresh-file), "sync_dir" (only on fresh-file); op "enqueue_durable":
	// phases "write_record", "fsync") — "ended", not "completed
	// successfully": it fires when the phase's operation RETURNS, whether
	// that return is success or an error (elapsed is the real duration
	// either way; see timedStorePhase in store.go, the single call shape
	// every phase site uses to guarantee this). This is what makes
	// OnStorePhaseStart/OnStorePhase a reliable start/end pair for an
	// in-flight watchdog (see its doc comment below): an I/O error (EIO,
	// ENOSPC) still reports its end promptly, so the watchdog's table entry
	// is always cleared, never left stuck warning about a phase that in
	// fact already failed and returned. Called synchronously while the
	// session mutex is held — the callback must be fast and must never call
	// back into the Session (same rule as OnEvent). Purely observational:
	// timing hooks for diagnosing slow storage (e.g. a saturated network
	// volume), never control flow.
	OnStorePhase func(op, phase string, elapsed time.Duration)

	// OnStorePhaseStart, when non-nil, is invoked immediately before each
	// OnStorePhase-instrumented operation begins (same op/phase names — see
	// OnStorePhase's doc comment above), which — see that comment — is
	// GUARANTEED to fire exactly once for every Start call, on success or
	// error alike. It is the counterpart that makes an in-flight watchdog
	// possible: OnStorePhase alone only reports a phase once it ENDS, so a
	// phase that never ends at all — e.g. a wedged network volume hanging a
	// file operation indefinitely, neither succeeding nor erroring — produces
	// no log line at all. That gap is exactly what a production canary hit:
	// a create hung permanently mid-ensureLog with zero phase timing lines,
	// because completion-only logging is blind to a phase that never
	// completes. A caller pairs each Start call with the matching
	// OnStorePhase end in a small table keyed by op/phase (see
	// cmd/harness/main.go's watchdog) so it can warn, repeatedly, while a
	// phase is still stuck — and reliably stop warning the moment it ends,
	// by any outcome. Called synchronously while the session mutex is held
	// — same rules as OnStorePhase/OnEvent: must be fast, must never call
	// back into the Session.
	OnStorePhaseStart func(op, phase string)

	// OnRequest, when non-nil, is invoked synchronously in streamTurn with the
	// exact final request about to be sent to the provider — after params,
	// system-segment, and hook assembly, immediately before prov.Stream. turn
	// counts from 1 for the first model call of the session and increments once
	// per model call, so a tool loop advances it. The *provider.Request is
	// SHARED with the provider call: callbacks MUST NOT mutate it or anything it
	// references (System, Messages, Tools). A nil OnRequest costs nothing.
	//
	// sessionID is the firing session's own s.ID. The call site
	// (streamTurn) supplies it; the callback never derives it. This
	// mirrors emit()'s ev.SessionID = s.ID for OnEvent, for the same
	// reason. configSnapshot (session_manager.go) copies a Config by
	// value into every Spawn'd child, func value included. A callback
	// closed over its original session's id would therefore misattribute
	// every descendant's request.meta records to that one session. A live
	// audit caught exactly this: cmd/harness closed OnRequest over a
	// local `sess` variable, and a spawned child's requests all reported
	// the parent's id. The explicit sessionID keeps the callback
	// session-agnostic, so any number of Spawn generations can inherit it
	// safely (see newSessionFn/loadSessionFn).
	OnRequest func(sessionID string, turn int, req *provider.Request)

	// Instructions controls project-instruction (AGENTS.md) injection into
	// the system prompt. A nil value is the default: auto-discover AGENTS.md
	// by walking up from WorkDir. See InstructionsConfig.
	Instructions *InstructionsConfig

	// SkillsDirs are the directories scanned for Agent Skills
	// (agentskills.io), each holding skill subdirectories with a SKILL.md.
	// A nil value is the default: use <WorkDir>/.agents/skills when that
	// directory exists. An explicit empty (non-nil) slice disables skill
	// discovery entirely. Duplicate skill names across dirs are an error.
	// See skills.go.
	SkillsDirs []string

	// AgentDefsDirs are the directories scanned for custom `task`-tool
	// agent definitions (*.md files — see agentdef.go's LoadAgentDefs),
	// mirroring SkillsDirs' own nil/empty/multi-dir contract exactly: a
	// nil value is the default (use <WorkDir>/.agents — LoadAgentDefs's
	// existing single-location behavior, unchanged for every caller that
	// never sets this field), an explicit empty (non-nil) slice disables
	// custom agent definitions entirely, and a multi-entry slice merges
	// every directory's definitions, with a duplicate NAME across two
	// directories treated as a load error exactly like a duplicate name
	// within one directory already is (see ResolveAgentDefs) — there is
	// no "which one wins" answer for two genuinely different definitions
	// claiming the same name, project-local or otherwise.
	AgentDefsDirs []string

	// MCP is the MCP client integration this session's tools draw from: its
	// Tools() are merged into the request's tool list (namespaced
	// mcp__<server>__<tool>) and a call to one of them routes through
	// CallTool. *MCPManager (see mcp.go) is the production implementation,
	// built once per process and shared across every session — nil (the
	// default) registers no MCP tools at all. See MCPRegistry's doc
	// comment for why this is injected rather than built from raw server
	// specs here. It also gates the built-in `mcp` session tool (status/
	// connect — see mcp_tool.go): registered whenever MCP reports at least
	// one configured server (via the narrow mcpConfigReader interface),
	// with no separate config flag, unlike GoalTool below.
	MCP MCPRegistry

	// MCPToolLoading selects when this session defers MCP tool SCHEMAS
	// instead of registering every one of them on every request (see
	// mcp_lazy.go and docs/design/mcp-lazy-tools.md). The zero value is
	// eager: every tool of every connected server registers with its full
	// schema, exactly as it did before deferral existed.
	MCPToolLoading MCPToolLoading
	// MCPToolLoadingThreshold is the tool COUNT MCPToolLoadingAuto compares
	// the live catalog against. Any non-positive value (including the zero
	// value) resolves to defaultMCPDeferThreshold -- never to a floor of 1,
	// which would defer every catalog (see mcpDeferThreshold).
	MCPToolLoadingThreshold int
	// MCPToolLoadingByServer overrides MCPToolLoading for one named server:
	// MCPToolLoadingEager pins its tools loaded, MCPToolLoadingLazy always
	// defers them. An absent entry inherits MCPToolLoading.
	//
	// The policy rides Config, not MCPServerConfig, because *MCPManager is
	// a box-scoped singleton shared by every session (see mcp.go's package
	// doc) while a SELECTION is per-session state: two sessions on one box
	// select different tools from the same catalog. Connection settings
	// stay with the manager; presentation policy sits with the session that
	// applies it. cmd/harness still lets the config author write the
	// override next to the server it names (mcp_servers.<name>.tool_loading).
	MCPToolLoadingByServer map[string]MCPToolLoading

	// Processes is the managed-process integration the `process` session
	// tool and the ambient status injection (see streamTurn) draw from.
	// *process.Manager (see engine/process.go and package process) is the
	// production implementation, built once per harness process and
	// shared across every session — nil (the default) installs no
	// `process` tool at all and injects no ambient status. Unlike MCP,
	// cmd/harness's serve wiring builds a non-nil Manager unconditionally
	// (even with zero declared processes): see docs/design/
	// managed-processes.md for why the process tool is exposed
	// unconditionally in serve mode.
	Processes ProcessRegistry

	// GoalTool enables the built-in `goal` session tool (status/set/adjust —
	// see goal_tool.go), which lets the model itself inspect, arm, or adjust
	// this session's completion goal in-process, no HTTP round-trip. False
	// (the default) installs no `goal` tool at all, exactly like a nil
	// Config.Processes installs no `process` tool. The server/CLI wiring
	// that sets this true when a goal evaluator is configured is a later
	// task (see docs/design/2026-07-19-goal-self-adjust.md) — this field
	// only gates registration.
	GoalTool bool

	// ModelTool enables the built-in `model` session tool (status/set — see
	// model_tool.go), which lets the model itself inspect the current model,
	// the configured aliases, and the configured provider names, and swap the
	// MAIN session model in-process via Session.SetModel. There is no clear
	// action. False (the default zero value) installs no `model` tool; the
	// server/CLI wiring sets it true by default (config key `model_tool`,
	// default true) so an operator opts OUT, unlike GoalTool which opts in
	// only when an evaluator is configured.
	ModelTool bool

	// SessionManager, when non-nil, enables the built-in `task` tool
	// (task_tool.go): it is the manager this session was created or
	// adopted through (SessionManager.NewRoot/AdoptRoot/Spawn all set it),
	// and Run reads it back off Session.cfg to actually spawn a child —
	// the same "read the dependency off cfg at Run time" shape MCP,
	// Processes, and GoalTool already use, not a closure captured at tool-
	// construction time. A session built directly via NewSession/
	// LoadSession, bypassing SessionManager entirely, leaves this nil and
	// gets no `task` tool at all — today's existing single-session flow is
	// completely unaffected. Whether `task` actually ends up registered
	// (and, for a spawned child, whether it survives an agent definition's
	// tools: restriction) is decided by SessionManager itself, not by this
	// field alone — see SessionManager.installTaskToolLocked.
	SessionManager *SessionManager

	// ModelAliases maps short alias names ("fast", "smart") to model refs,
	// mirroring config.Config.Aliases (the engine never imports config). The
	// `model` tool resolves a one-level alias against this map before parsing
	// (see runModelTool), so a host that populates it lets the model swap by
	// alias, not only by full "provider/model" ref. Nil means no aliases.
	ModelAliases map[string]string

	// Tools are additional built-in tools. The bash tool is always
	// installed.
	Tools       []Tool
	BashTimeout time.Duration // defaults to 2m

	// BashOutputCap bounds the bytes of combined stdout+stderr the bash tool
	// keeps from one command, truncating (head + tail, marker in between)
	// before the output ever reaches the message log. Zero/negative means
	// the default (see defaultBashOutputCap in bash.go, 96KB) — a runaway
	// command (an apt-get/npm install storm is the real-world trigger) can
	// otherwise dump megabytes into a single message and poison the session.
	BashOutputCap int

	// StreamIdleTimeout bounds the gap between consecutive provider
	// stream events within one model call — an idle-stream watchdog, NOT
	// a total-turn deadline (a legitimate long turn that keeps streaming
	// is never cut). When a stream goes silent for this long, the request
	// is cancelled and the failure surfaces classified
	// provider.RetryableStreamTruncated, riding the same retry tier a
	// dropped stream does. Zero defaults to 5 minutes — the same knob and
	// default as Codex's stream_idle_timeout_ms (300_000), and far above
	// any healthy stream's inter-event gap (adapters see provider
	// keep-alive pings as events). Negative disables the watchdog.
	StreamIdleTimeout time.Duration

	// PromptRetries is how many ADDITIONAL attempts the base interactive
	// Prompt loop (runAgenticLoop, see streamTurnWithRetry) makes when a
	// model call fails with a transient, retryable provider error — an HTTP
	// 5xx/429/529 or a truncated stream, classified through
	// provider.AsRetryable, never by matching error text. Zero (the zero
	// value) DISABLES the retry: the first failure surfaces exactly as it did
	// before this field existed, which keeps a bare embedder-built
	// engine.Config unchanged. The config/CLI wiring sets the product default
	// of 2 (config key `prompt_retries`, config.Config.PromptRetriesValue).
	//
	// The budget is deliberately small and short (basePromptRetryDelay: 1s,
	// then 2s) because an interactive user waits on the turn — it smooths a
	// one-off blip, NOT the goal loop's ~30min weather tiers
	// (promptTurnWithRetry, goal.go). A deterministic failure, a
	// provider.AsPermanent malformed-request shape, or an interruptedTurnError
	// is never retried regardless of this value — see streamTurnWithRetry.
	PromptRetries int
	// ContextWindowTokens is the model's context window size, in tokens, as
	// an EXPLICIT operator override. A caller passing a positive value here
	// pins the window for the session's lifetime, immune to a later model
	// switch (see SetModel). Left zero, newSession derives it instead from
	// the session's model via modelmeta.ContextWindow (package modelmeta,
	// context_window.go's resolveContextWindow) — so by the time Prompt
	// first runs, this field already holds the EFFECTIVE window, not
	// necessarily what the caller passed in; a caller inspecting it right
	// after NewSession/LoadSession returns sees the resolved value.
	// Automatic compaction stays disabled only when no model metadata was
	// found either (an unrecognized ref, or one below the sanity floor —
	// see minAutoContextWindowTokens). When positive, Prompt checks
	// LastUsage against ContextWindowTokens * CompactionThreshold at the top
	// of every call and compacts first if over. See docs/design/
	// context-compaction.md and, for the derivation itself,
	// context_window.go's package doc comment.
	ContextWindowTokens int
	// CompactionThreshold is the fraction of ContextWindowTokens at which
	// automatic compaction triggers. Zero defaults to 0.8, mirroring
	// newSession's existing zero-fills-a-default pattern for BashTimeout.
	CompactionThreshold float64
	// CompactionKeepTurns is how many of the most recent turns automatic
	// (and, unless overridden per call, explicit) compaction always keeps
	// verbatim. Zero defaults to 2. The effective value can never compact
	// below 1 (see Session.Compact).
	CompactionKeepTurns int
	// CompactionModel overrides the model used for the compaction summary
	// call. Zero (the default) uses the session's own current model at the
	// time Compact runs — unlike GoalOptions.Evaluator, summarization needs
	// competence, not independence.
	CompactionModel message.ModelRef

	// ToolResultInlineBytes is the size above which a TEXT tool result is
	// retained into this session's sidecar store and replaced in history by
	// a short preview carrying a trh_N handle (see toolresult.go and
	// docs/plans/2026-08-19-tool-result-handles.md). Zero or negative — the
	// zero value — DISABLES retention entirely: an embedder building a bare
	// engine.Config gets exactly the pre-retention behavior, with no sidecar
	// directory ever created. The config/CLI layer supplies the product
	// default of 16384 (config key `tool_result_inline_bytes`), the same
	// split PromptRetries uses.
	//
	// Retention additionally requires SessionDir: without one there is
	// nowhere to durably put the bytes, and a preview naming an unreadable
	// handle is worse than no preview (see Session.toolResultInlineLimit).
	ToolResultInlineBytes int
	// ToolResultRetainedBytes is the per-session ceiling on TOTAL retained
	// bytes. Once reached, a further oversized result is still previewed but
	// its remainder is discarded rather than written, and the preview says
	// so with no handle (see toolResultCapHeader). Zero or negative disables
	// the ceiling. Config key `tool_result_retained_bytes`, product default
	// 4194304.
	ToolResultRetainedBytes int

	// ToolConcurrency bounds how many of one assistant message's tool calls
	// runToolCalls (toolexec.go) runs at once. Resolved ONCE, in newSession,
	// into Session's own toolConcurrency field — see resolveToolConcurrency
	// (toolexec.go), which follows resolveContextWindow's exact
	// precedence shape (context_window.go).
	//
	// Precedence: 0 (the zero value, "unset") resolves to the package
	// default, defaultToolConcurrency (8) — a batch runs in parallel, capped
	// at 8 calls in flight. 1 resolves to strictly SEQUENTIAL execution:
	// one call at a time, in call order, including for a serial tool (a
	// barrier is a no-op when nothing ever runs beside it). A value above
	// 1 is the cap verbatim. A negative value is clamped to 1 (sequential)
	// — never treated as "unlimited".
	//
	// 1 restores the pre-parallel ORDER, not the pre-parallel behavior in
	// every respect, and an operator reaching for it as an exact revert
	// should know the two deliberate differences: runOneGuarded turns a
	// panicking tool into one error result instead of letting it unwind
	// through Prompt, and admitAndRun refuses to START a call once the
	// turn is canceled, where the old loop ran every remaining call.
	// Neither depends on the mode, by design — the one-result-per-call
	// guarantee must hold identically however a batch executes. See the
	// sequential branch in toolexec.go, which states the same thing.
	//
	// The engine itself never reads an environment variable (see
	// session_manager.go's own "the engine itself never reads environment
	// variables" rule) — this field is the ONLY seam. cmd/harness's
	// toolConcurrency() turns the HARNESS_SEQUENTIAL_TOOLS=1 kill switch
	// and the HARNESS_TOOL_CONCURRENCY=<n> cap into this field before it
	// builds Config, for both `harness run` and `harness serve`; this
	// package neither reads nor knows about either variable. An embedder
	// that builds Config itself sets the field directly and gets no
	// environment handling at all.
	ToolConcurrency int
}

// Session is one conversation: an in-memory history plus the agent loop.
// Methods are safe for one caller at a time; Prompt must not be called
// concurrently with itself.
type Session struct {
	ID string

	cfg   Config
	tools map[string]Tool

	mu        sync.Mutex
	model     message.ModelRef
	effort    message.Effort // reasoning-effort level; swap with SetEffort
	history   []message.Message
	usage     provider.Usage // cumulative, across every turn (see appendWithUsage)
	createdAt time.Time
	// lastUsage/haveLastUsage carry the most recent model turn's own Usage
	// (input/output/cache tokens for that one request), distinct from the
	// cumulative usage field above — GET /session surfaces both (issue #62
	// layer 2) so an orchestrator can see the size of the request that JUST
	// went out, not only the running total. haveLastUsage is false until
	// the session's first completed turn (fresh session, or one reloaded
	// before any turn ever ran against it in any process).
	lastUsage     provider.Usage
	haveLastUsage bool

	// turnUnsettled is SessionManager.recoverInterruptedTurnLocked's
	// restart-recovery signal, replacing an earlier, unreliable
	// heuristic (hasUnansweredTurn, since removed) that tried to infer
	// "was this turn genuinely interrupted" from the trailing message's
	// own role. A live review proved that heuristic wrong in both
	// directions: it MISSED a genuine mid-tool-loop crash (a trailing
	// RoleTool/unresolved-ToolCall shape it never checked), and — once
	// widened to check those too — it then MISFIRED on an already-
	// SETTLED ordinary failure, since runAgenticLoop's plain (non-
	// interruptedTurnError) provider-error path appends nothing at all,
	// leaving the exact same trailing-RoleUser shape a genuine crash
	// would. Worse, several LEGITIMATE, fully-settled completion paths
	// (appendUnexecutedToolCallResults, interruptedToolResults) ALSO
	// leave a trailing RoleTool message — indistinguishable, by role
	// alone, from a genuine mid-tool-loop crash.
	//
	// This field sidesteps trailing-shape guessing entirely: true from
	// the moment ANY message is appended (appendWithUsage, the single
	// ingest choke point every append goes through — user, assistant, or
	// tool alike) until SessionManager.finalizeTurn runs to completion
	// for this node and explicitly marks it settled (markTurnSettled,
	// durably backed by the recChildTurnSettled record — see its own
	// doc comment in store.go) — REGARDLESS of what that turn's outcome
	// was (success, an ordinary failure that appends nothing, a
	// cancellation) or what trailing message shape resulted. finalizeTurn
	// running to completion IS the authoritative "this turn's outcome is
	// settled" signal; only a genuine crash — the process dying before
	// finalizeTurn ever gets to run — leaves this true on the next
	// reload, which is exactly the one case restart-recovery exists to
	// detect.
	//
	// Deliberately per-SESSION, not per-turn-attempt: a session only
	// ever has ONE turn in flight at a time (SessionManager's own
	// StatusRunning gating), so a simple bool — set true on ANY new
	// append, false only by finalizeTurn's own marker — is sufficient;
	// no sequence numbers or per-attempt bookkeeping needed. Only ever
	// meaningful for a non-root node (finalizeTurn only writes the
	// marker for one — see its own doc comment); recovery itself is
	// never invoked for a root (adoptReloadedLocked's own early return),
	// so an unmarked root session's turnUnsettled value is simply never
	// consulted.
	turnUnsettled bool

	// committedOutcome is the exact taskNotification finalizeTurn (or
	// recoverInterruptedTurnLocked itself) computed for s's most recent
	// turn — nil until first computed and durably committed
	// (recTaskOutcomeCommitted, store.go). Does DOUBLE DUTY, deliberately
	// surviving past the moment the turn it describes settles:
	//
	//   - WHILE that turn is still unsettled (hasUnfinalizedTurn() ==
	//     true): recovery's own crash-replay payload — see
	//     commitTurnOutcome/committedTurnOutcome's own doc comments and
	//     the crash-window table on recoverInterruptedTurnLocked's own
	//     doc comment (session_manager.go) for the full mechanism this
	//     closes: a live review finding that recovery's OWN
	//     reconstruction (from trailing history shape, or a generic
	//     "lost to restart" fallback) can DIVERGE from what finalizeTurn
	//     already computed and durably delivered before a crash struck
	//     INSIDE finalizeTurn's own deliver-then-settle sequence —
	//     producing a duplicate notification with a DIFFERENT payload
	//     than the one the parent may already have received.
	//   - ONCE settled: the last known terminal outcome — see
	//     SessionManager.restoreKnownStatusLocked, which a LATER
	//     adoption of this already-settled node (a live prod finding)
	//     uses to restore n.status/n.result/n.failReason correctly,
	//     rather than leaving adoptLocked's bare StatusIdle default
	//     uncorrected forever.
	//
	// Reset to nil the moment a NEW turn starts (see appendWithUsage/
	// appendMemoryOnly's own identical turnUnsettled=true side effect) —
	// THAT is the correct invalidation point (a genuinely new turn
	// supersedes whatever the previous one settled as), not "this turn
	// just settled": markTurnSettled/the recChildTurnSettled fold
	// deliberately do NOT also clear this, precisely so the second bullet
	// above has something to read.
	committedOutcome *taskNotification

	// spawnedChildIDs is every child id this session has ever Spawn'd —
	// appended to live (Spawn, session_manager.go) and folded back from
	// the durable recTaskSpawned audit trail on reload (store.go's
	// LoadSession). Never trimmed or deduplicated: Spawn writes exactly
	// one recTaskSpawned per child, so a straight append-only list is
	// already exact.
	//
	// Read by SessionManager.recoverCrashedChildrenLocked, which a live
	// prod finding added: without SOME durable way to answer "which
	// children did I spawn," a session whose PARENT is the only thing a
	// box ever touches after a restart (a read-only transcript GET, or a
	// later follow-up turn on the parent itself — never the crashed
	// child directly) had no way to discover that one of its children
	// crashed mid-turn and never got recovered — recoverInterruptedTurnLocked
	// only ever runs reactively, on next touch of the CRASHED node's own
	// id (see that method's own doc comment's "purely reactive" section).
	// spawnedChildIDs is what lets adopting the PARENT also sweep its own
	// children for exactly this case.
	spawnedChildIDs []string

	logFile        *os.File // session log; nil until first write (see store.go)
	logStarted     bool     // the log file exists on disk
	lastPersistErr error

	// Project-instruction segment, loaded once on the first Prompt (see
	// instructions.go). instrLoaded gates the one-time disk read; instrSeg is
	// the cached system-prompt segment (empty when none); instrErr records a
	// present-but-unusable instructions file so every Prompt fails alike;
	// instrPath is the display path of the source file (empty when none), used
	// by the session_info tool to report instruction provenance.
	instrLoaded bool
	instrSeg    string
	instrErr    error
	instrPath   string

	// turn counts model calls made in this session (from 1); lastSystem holds
	// the system segments assembled for the most recent model call. Both feed
	// OnRequest and the session_info tool (see session_info.go). Guarded by mu.
	turn       int
	lastSystem []string

	// skills is the structured catalog discovered on the first Prompt (name +
	// absolute SKILL.md path), used by the session_info tool. The advertised
	// prompt segment lives in skillsSeg below; this is the same catalog, kept
	// structured so session_info can report skill provenance.
	skills []skillInfo

	// Agent Skills catalog segment, discovered once on the first Prompt (see
	// skills.go). Same load-once-cache-error pattern as instructions:
	// skillsLoaded gates the one-time disk scan, skillsSeg is the cached
	// stage-1 catalog (empty when none), skillsErr records a discovery
	// failure so every Prompt fails alike.
	skillsLoaded bool
	skillsSeg    string
	skillsErr    error

	// Goal-loop state (see goal.go). goalActive is set while a goal is set but
	// neither achieved nor cleared; goalCondition holds the current goal's
	// completion condition. Restored on LoadSession from the goal.* records in
	// the session log. Guarded by mu.
	goalActive    bool
	goalCondition string

	// goalGen counts every RegisterGoal/UpdateGoal that establishes a new
	// condition text (a same-condition UpdateGoal is a no-op and does NOT
	// bump it — see UpdateGoal). PursueGoal snapshots (condition, goalGen,
	// goalActive) together at each turn boundary (see goalSnapshot) so an
	// evaluator verdict or worker-turn outcome computed against an earlier
	// snapshot can be told apart from the current goal even when the
	// condition text itself happens to collide, and discarded rather than
	// journaled — see goalSnapshot's doc comment. Deliberately runtime-only:
	// never persisted, never appears in a goal.* record, never restored on
	// LoadSession (a resumed session starts a fresh loop, which registers or
	// resumes against whatever condition the log folds to and gets a new
	// gen from that point forward — replay correctness comes from the
	// goal.updated fold, not from reproducing this counter's exact value).
	// Guarded by mu.
	goalGen uint64

	// goalParked mirrors the most recent goal.parked record's classified
	// reason and attempt count (see recordGoalParked/classifyGoalWorkerError
	// in goal.go) for the ambient status segment goal_parked_status.go
	// renders on a later Prompt call. True from the moment a worker turn
	// exit-parks the goal (recordGoalParked sets it, still under the same
	// s.mu critical section that journals goal.parked) until the NEXT
	// PursueGoal call clears it at entry — before that call's own first
	// worker turn ever runs (see PursueGoal's clearGoalParkedAtEntry call) —
	// or a clearGoal call resets it (DELETE /goal, or the context-overflow
	// clear branch immediately above the park branch in PursueGoal).
	//
	// Deliberately runtime-only: never persisted, never folded by
	// LoadSession, never appears in a goal.* record itself (goal.parked's
	// own Reason/Attempts fields are the durable source these mirror) — see
	// goal_parked_status.go's doc comment for the post-restart asymmetry
	// this implies (the boot-only goal.paused presentation, server-side,
	// covers visibility after a restart instead). Guarded by mu.
	goalParked         bool
	goalParkedReason   string
	goalParkedAttempts int

	// toolExecCount counts tool-call executions across the session's
	// lifetime: incremented once per call to runToolCall that actually
	// invokes a tool (i.e. not one blocked by a tool.execute.before deny),
	// whether the tool succeeds or returns an error result. The goal loop
	// (see goal.go's promptTurnWithRetry) snapshots this before and after
	// each worker-turn attempt to detect whether a failed attempt executed
	// any tool before failing — a retry re-issues the whole directive, which
	// is unsafe to do blindly once a tool has already run. Guarded by mu.
	toolExecCount int

	// compactHysteresis is the churn-guard flag (see docs/design/
	// context-compaction.md §2): set true the moment an AUTOMATIC
	// compaction actually folds something, cleared the next time
	// LastUsage().InputTokens is observed below the trigger threshold.
	// While true, the automatic trigger does not fire again — folding an
	// ever-shrinking prefix cannot relieve pressure that lives in the KEPT
	// region (a single giant tool result), so re-firing every turn would
	// just burn summarization round-trips. Deliberately NOT persisted: a
	// reload re-evaluates from scratch (see LoadSession), and the worst
	// post-reload cost is one extra summarization attempt. The explicit
	// /compact path (Compact called directly, not via maybeAutoCompact)
	// never reads or sets this — an operator override always runs.
	// Guarded by mu.
	compactHysteresis bool

	// contextWindowExplicit is true when the ORIGINAL Config.ContextWindowTokens
	// passed to NewSession/LoadSession was already positive — an operator
	// override. It is set once, at construction, and never changes again for
	// this session's lifetime: resolveContextWindow's precedence (explicit
	// config > model-derived > disabled) means an explicit value is pinned
	// even across a later model switch (see SetModel), so this flag is what
	// tells SetModel whether it is allowed to re-derive s.cfg.ContextWindowTokens
	// at all. contextWindowSource mirrors whichever branch resolveContextWindow
	// took most recently (contextWindowSource{Config,ModelDerived,Disabled}),
	// carried for the session-start and model-switch INFO log lines
	// (logContextWindowArmed, context_window.go). Guarded by mu.
	contextWindowExplicit bool
	contextWindowSource   string

	// toolConcurrency is the resolved (never-zero, never-negative) cap on
	// how many of one batch's tool calls run at once — see
	// Config.ToolConcurrency and resolveToolConcurrency (toolexec.go). Set
	// once in newSession and never changed again for the session's
	// lifetime; unlike the context window, no runtime event re-derives it.
	// 1 means strictly sequential. Read-only after construction, so no
	// lock is needed to read it from a running batch.
	toolConcurrency int

	// compactCount/lastCompactedAt track how many times this session has
	// been compacted and when the most recent one landed — durable via the
	// compact journal record (see store.go), so GET /session can show a UI
	// that compaction happened even after a restart. Guarded by mu.
	compactCount    int
	lastCompactedAt time.Time

	// promptQueue is the session's durable FIFO of prompts enqueued while
	// busy (see queue.go and docs/plans/2026-07-19-prompt-queue.md). Each
	// entry is delivered later either via a normal Prompt call (idle drain,
	// Task 3) or as a goal-turn-boundary interjection (Task 2) — a queued
	// prompt never enters s.history nor any provider request before then
	// (the plan's "Locked design decisions": queued prompts live in this
	// field and their own record types, never s.history). Restored on
	// LoadSession by folding the prompt.queued/prompt.dequeued records in ID
	// order (see store.go). Guarded by mu.
	promptQueue []QueuedPrompt

	// promptQueueNextID mints EnqueuePrompt's session-monotonic queue ID,
	// starting at 1 for a fresh session (set in newSession) and overridden by
	// LoadSession's fold to one past the highest prompt.queued ID it replays
	// — see LoadSession's recPromptQueued case. Guarded by mu.
	promptQueueNextID int64

	// deferredQueueRecords holds prompt-queue records whose memory
	// mutation already happened but whose disk write was deferred out
	// from under the tree-wide m.mu (see queueRecordDeferredLocked in
	// queue.go and SessionManager.deferPersist). FIFO, in memory-mutation
	// order. Guarded by mu.
	deferredQueueRecords []deferredQueueRecord

	// enqueueSeq is the durable-enqueue idempotency high-water mark (see
	// EnqueuePromptDurable in queue.go and promptRecord.Seq in store.go):
	// the largest caller-issued seq durably accepted. Monotonic; a seq at or
	// below it is a duplicate no-op. Rebuilt on replay by LoadSession.
	enqueueSeq int64

	// toolResults maps a retained tool result's handle (trh_N) to its
	// metadata (see toolresult.go). Content is NOT held here — the bytes
	// live in the per-session sidecar file. Rebuilt on resume by
	// LoadSession's toolresult.retained fold, which is what lets
	// read_tool_result serve a handle minted by a previous process.
	// Guarded by mu.
	toolResults map[string]toolResultMeta
	// mcpSelected is the set of namespaced MCP tool names whose schemas
	// this session has loaded (see mcp_lazy.go). Only consulted for a
	// server the request decided to DEFER: an eager server's tools are in
	// the array whether or not they appear here. Guarded by mu.
	mcpSelected map[string]bool
	// toolResultNextID mints the session-monotonic handle number, starting
	// at 1 for a fresh session (set in newSession) and advanced by
	// LoadSession past every handle seen in the log — the counter-survives-
	// resume requirement, mirroring promptQueueNextID exactly. IDs are
	// burned on a failed sidecar write and never reused. Guarded by mu.
	toolResultNextID int64
	// toolResultBytes is the running total of bytes retained in this
	// session, checked against Config.ToolResultRetainedBytes. Rebuilt on
	// resume from the same fold, so the ceiling is a session-lifetime one
	// rather than a per-process one. Guarded by mu.
	toolResultBytes int

	// taskNotifications is this session's pending queue of child-completion
	// signals not yet checked out for an in-flight turn attempt (see
	// taskdelivery.go): SessionManager.finalizeTurn appends to it from
	// whatever goroutine just finished driving a CHILD's turn. streamTurn
	// moves entries from here into taskNotificationsInFlight every model
	// call via checkoutTaskNotificationsSegment. Nil for a session with no
	// SessionManager, or one that has never had a child complete. Guarded
	// by mu.
	taskNotifications []taskNotification
	// taskNotificationsInFlight holds notifications already checked out
	// for the turn attempt currently in progress — moved back to
	// taskNotifications (requeueTaskNotifications) if that attempt fails,
	// or cleared (commitTaskNotifications) once it actually succeeds. See
	// checkoutTaskNotificationsSegment's doc comment for why this two-
	// phase handoff exists: a destructive single drain lost a notification
	// to a retried or discarded attempt. Guarded by mu.
	taskNotificationsInFlight []taskNotification

	// agentDefsLoaded/agentDefs/agentDefsErr cache AgentDefs' discovery
	// (agentdef.go), on the SAME load-once-cache-error pattern instrLoaded/
	// instrSeg/instrErr and skillsLoaded/skillsSeg/skillsErr already use —
	// but triggered by the `task` tool's first call, not by Prompt (see
	// AgentDefs' doc comment for why). Guarded by mu.
	agentDefsLoaded bool
	agentDefs       map[string]AgentDef
	agentDefsErr    error
}

// NewSession creates a session. Nothing touches the network, spawns
// processes, or writes to disk here — provider auth and plugin spawns happen
// on first use, and the session log is created on first message append.
func NewSession(cfg Config) *Session {
	s := newSession(cfg)
	s.ID = newID("ses")
	logContextWindowArmed(s.ID, s.model, s.cfg.ContextWindowTokens, s.contextWindowSource, "start")
	return s
}

// newSession builds a session minus its ID; NewSession and LoadSession
// share it.
func newSession(cfg Config) *Session {
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 8192
	}
	if cfg.StreamIdleTimeout == 0 {
		cfg.StreamIdleTimeout = 5 * time.Minute
	}
	if cfg.BashTimeout <= 0 {
		cfg.BashTimeout = 2 * time.Minute
	}
	// contextWindowExplicit records whether the CALLER set
	// Config.ContextWindowTokens, captured from the ORIGINAL value before
	// resolveContextWindow below overwrites it with the effective (possibly
	// model-derived) window. SetModel later needs this to decide whether a
	// model switch is ever allowed to touch the field again — see its doc
	// comment and resolveContextWindow's precedence.
	contextWindowExplicit := cfg.ContextWindowTokens > 0
	var contextWindowSource string
	cfg.ContextWindowTokens, contextWindowSource = resolveContextWindow(cfg.ContextWindowTokens, cfg.Model)
	s := &Session{
		cfg:                   cfg,
		model:                 cfg.Model,
		effort:                cfg.Effort,
		tools:                 make(map[string]Tool),
		createdAt:             time.Now().UTC(),
		promptQueueNextID:     1,
		contextWindowExplicit: contextWindowExplicit,
		contextWindowSource:   contextWindowSource,
		toolResultNextID:      1,
		toolResults:           make(map[string]toolResultMeta),
		toolConcurrency:       resolveToolConcurrency(cfg.ToolConcurrency),
	}
	for _, t := range []Tool{bashTool(cfg.BashTimeout, cfg.BashOutputCap), readFileTool(), writeFileTool(), editFileTool(), sessionInfoTool(), globTool(), grepTool(), lsTool()} {
		s.tools[t.Def.Name] = t
	}
	if cfg.Processes != nil {
		s.tools[processToolName] = processTool(cfg.Processes)
	}
	if cfg.GoalTool {
		s.tools[goalToolName] = goalTool()
	}
	if cfg.ModelTool {
		s.tools[modelToolName] = modelTool()
	}
	if mcpConfiguredCount(cfg.MCP) > 0 {
		// The def carries the search/select actions only when this
		// session's POLICY can defer (mcpPolicyCanDefer, mcp_lazy.go): a
		// session that defers nothing must not advertise an action with
		// nothing to act on. Policy is fixed for the session's life, so
		// the def stays byte-stable across requests.
		s.tools[mcpSessionToolName] = mcpTool(s.mcpPolicyCanDefer())
	}
	// task is registered here unconditionally whenever a SessionManager is
	// present; SessionManager itself withholds it post-construction for a
	// session at (or past) the depth limit, and an agent definition's
	// tools: restriction can remove it too (see installTaskToolLocked and
	// Spawn in session_manager.go) — newSession has no notion of depth or
	// agent definitions, so it is not the right place to make either call.
	if cfg.SessionManager != nil {
		s.tools[taskToolName] = taskTool()
	}
	// read_tool_result is registered only when retention can actually mint a
	// handle — a positive inline limit AND a SessionDir, the same condition
	// toolResultInlineLimit resolves. A session that can never produce a
	// handle must not advertise a tool whose only required argument is one.
	if s.toolResultInlineLimit() > 0 {
		s.tools[readToolResultToolName] = readToolResultTool()
	}
	for _, t := range cfg.Tools {
		s.tools[t.Def.Name] = t
	}
	return s
}

// SetModel swaps the model for subsequent requests. History transcodes
// automatically; there is no migration step. A no-op set to the current model
// changes nothing and emits no event.
//
// On a real change it persists the durable recModel resume record and emits
// EventModelChanged (carrying the new model), both while holding s.mu so event
// order matches log order — the same persist-and-emit-under-s.mu shape
// RegisterGoal uses. EventModelChanged is the ONE event every swap route
// (the `model` tool, a per-request prompt override, POST /session/{id}/model)
// funnels through, so the server journals every swap once via a single path.
// OnEvent must not call back into this Session.
//
// The automatic-compaction context window follows the swap: unless
// Config.ContextWindowTokens was set explicitly (contextWindowExplicit,
// pinned forever once true — see newSession), s.cfg.ContextWindowTokens is
// re-derived from ref via resolveContextWindow, exactly as it was at session
// start. A session that starts on a model with no metadata (compaction
// disabled) and switches to one that has it arms compaction from that point
// on, and the reverse disarms it — but ONLY when the re-derived (tokens,
// source) pair actually differs from what was already in effect does
// SetModel update s.cfg.ContextWindowTokens/contextWindowSource, clear
// compactHysteresis, and emit logContextWindowArmed's fresh INFO line (see
// that function's doc comment and context_window.go's package doc): a
// same-window switch (two models sharing the same metadata) is not an
// operator-facing event, and a window change invalidates the hysteresis
// churn-guard, which means "folding again won't relieve pressure at the
// window it latched under" (see compactHysteresis's doc comment) — a claim
// that no longer holds once the window itself has moved.
func (s *Session) SetModel(ref message.ModelRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref == s.model {
		return
	}
	s.model = ref
	s.persistModel(ref)
	if !s.contextWindowExplicit {
		nextTokens, nextSource := resolveContextWindow(0, ref)
		if nextTokens != s.cfg.ContextWindowTokens || nextSource != s.contextWindowSource {
			s.cfg.ContextWindowTokens, s.contextWindowSource = nextTokens, nextSource
			s.compactHysteresis = false
			logContextWindowArmed(s.ID, ref, nextTokens, nextSource, "model_switch")
		}
	}
	s.emit(Event{Type: EventModelChanged, Model: ref})
}

// ModelSupported reports whether ref names a configured provider — the same
// s.cfg.Providers.For check per-turn selection (see streamTurn) and the `model`
// tool use. It lets POST /session/{id}/model reject a swap to an unconfigured
// provider before SetModel runs, without the server importing the provider
// registry.
func (s *Session) ModelSupported(ref message.ModelRef) bool {
	_, err := s.cfg.Providers.For(ref)
	return err == nil
}

// Model returns the session's current model.
func (s *Session) Model() message.ModelRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

// configSnapshot returns a copy of s.cfg taken under s.mu, with Model
// overridden to the session's LIVE current model (s.model — see Model's
// doc comment: SetModel updates s.model, never s.cfg.Model, which stays
// pinned to whatever the session's ORIGINAL construction-time model was
// forever). It exists for SessionManager.Spawn, which needs to build a
// child's Config from its parent's: reading parent.session.cfg directly,
// unsynchronized, races SetModel's own writes under s.mu to
// s.cfg.ContextWindowTokens/contextWindowSource (see SetModel) — caught
// live by go test -race — and would have inherited the parent's STALE
// construction-time model besides, contradicting the design doc's
// "children inherit the parent's ... model" precedence. Every other
// Config field copies by value as a normal struct copy would; only Model
// gets the live override.
func (s *Session) configSnapshot() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	cfg.Model = s.model
	return cfg
}

// SetEffort swaps the reasoning-effort level for subsequent requests. A no-op
// set to the current level changes nothing and emits no event. The level rides
// every request the same way the model does — the adapter maps it to the
// provider's wire shape at transcode time, so there is no migration step.
//
// On a real change it persists the durable recEffort resume record and emits
// EventEffortChanged (carrying the new level), both while holding s.mu so event
// order matches log order — the same persist-and-emit-under-s.mu shape SetModel
// uses. EventEffortChanged is the ONE event every effort-swap route funnels
// through, so the server journals every swap once via a single path.
// OnEvent must not call back into this Session.
func (s *Session) SetEffort(e message.Effort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e == s.effort {
		return
	}
	s.effort = e
	s.persistEffort(e)
	s.emit(Event{Type: EventEffortChanged, Effort: e})
}

// Effort returns the session's current reasoning-effort level.
func (s *Session) Effort() message.Effort {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effort
}

// CreatedAt returns when the session was created (or, for a loaded session,
// when it was originally created per its log header).
func (s *Session) CreatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createdAt
}

// WorkDir returns the session's working directory: Config.WorkDir for a
// fresh session, or the value restored from the session log header for a
// loaded one (which wins over the Config.WorkDir the caller supplied to
// LoadSession — see store.go). It never changes after construction, so no
// lock is needed (consistent with direct s.cfg.WorkDir reads elsewhere, e.g.
// bash.go and filetools.go).
func (s *Session) WorkDir() string {
	return s.cfg.WorkDir
}

// ParentSession returns the session's lineage pointer (Config.ParentSession),
// restored from the header record on a loaded session — empty when the
// session has no recorded parent. See Config.ParentSession's doc comment.
func (s *Session) ParentSession() string {
	return s.cfg.ParentSession
}

// TaskParentID returns SessionManager's own tree-lineage pointer
// (Config.TaskParentID) — see that field's doc comment for why this is a
// completely different concept from ParentSession above, despite the
// similar name.
func (s *Session) TaskParentID() string {
	return s.cfg.TaskParentID
}

// hasTaskParent reports whether s is durably a non-root member of the
// subagent-sessions tree — the ONE predicate SessionManager uses, in
// exactly two places, to decide "does this node conceptually have a
// parent": adoptReloadedLocked's own root/non-root branch (which gates
// whether s is a recovery candidate at all) and finalizeTurn's
// settled-marker/commit-outcome gate (session_manager.go). Both MUST use
// this same helper rather than each re-deriving the answer their own way
// — a live review finding: finalizeTurn used to gate on
// sessionNode.parentID, the IN-MEMORY tree-structural pointer, instead of
// this durable one. The two normally agree, but adoptReloadedLocked's own
// "true depth is unrecoverable" case (its own doc comment) can leave a
// node's in-memory parentID blank even when it durably DOES have a real
// TaskParentID — for exactly that node, finalizeTurn's old gate silently
// skipped marking its turns settled at all, forever: hasUnfinalizedTurn()
// misread true on every later reload, even for turns that finished
// completely normally. Deliberately durable (Config.TaskParentID), never
// the in-memory pointer — see TaskParentID's own doc comment for why the
// two are distinct concepts in the first place.
func (s *Session) hasTaskParent() bool {
	return s.TaskParentID() != ""
}

// SpawnedChildIDs returns every child id s has ever Spawn'd, in Spawn
// order — see spawnedChildIDs' own doc comment for the durable fold and
// what reads this back.
func (s *Session) SpawnedChildIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.spawnedChildIDs...)
}

// HasHistoryOrSpawnedChildren reports whether s has any message history
// or has ever spawned a child, without allocating either full copy the
// way History()/SpawnedChildIDs() do — restoreKnownStatusLocked's own
// non-emptiness check (session_manager.go) needs only this boolean, not
// either slice's actual contents, and previously paid for two full
// slice copies (append([]T(nil), ...)) just to compare their lengths
// against zero and discard the result. A declined-thread follow-up from
// the subagent-sessions review.
func (s *Session) HasHistoryOrSpawnedChildren() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.history) > 0 || len(s.spawnedChildIDs) > 0
}

// recordSpawnedChildLocked appends childID to s.spawnedChildIDs — the
// live-path counterpart to LoadSession's own recTaskSpawned fold (see
// spawnedChildIDs' own doc comment), called once from
// SessionManager.Spawn right alongside its existing persistTaskSpawnLocked
// call, so SpawnedChildIDs() gives a complete answer whether s has been
// reloaded or has stayed live in the same process its whole life. Caller
// holds s.mu.
func (s *Session) recordSpawnedChildLocked(childID string) {
	s.spawnedChildIDs = append(s.spawnedChildIDs, childID)
}

// TaskAgentType and TaskToolNames return Config.TaskAgentType/TaskToolNames
// — see those fields' own doc comment.
func (s *Session) TaskAgentType() string {
	return s.cfg.TaskAgentType
}

func (s *Session) TaskToolNames() []string {
	return s.cfg.TaskToolNames
}

// TaskDepth returns Config.TaskDepth — see that field's own doc comment.
// 0 for a genuine root (which never has a TaskParentID to trigger a
// caller's cold-fallback branch in the first place) AND for a legacy child
// predating this field; a caller that needs to tell those apart already
// gates on hasTaskParent()/TaskParentID() first, exactly like every other
// Task* accessor here.
func (s *Session) TaskDepth() int {
	return s.cfg.TaskDepth
}

// hasUnfinalizedTurn reports whether s has a turn that started (any
// message got appended) without SessionManager.finalizeTurn ever running
// to completion for it — restart-recovery detection (see
// recoverInterruptedTurnLocked's own use of this and turnUnsettled's own
// doc comment for the full reasoning and the trailing-message-role
// heuristic this replaces).
//
// A false positive really is impossible now, unlike the heuristic this
// replaced: turnUnsettled is set true by every append (appendWithUsage/
// appendMemoryOnly, the two ingest choke points every append in this
// package goes through) and false ONLY by markTurnSettled, called
// exclusively from SessionManager.finalizeTurn (for an ordinary
// completion) or recoverInterruptedTurnLocked itself (once it has
// finished recovering this exact turn) — both of which are, by
// definition, the turn reaching a genuinely settled outcome. The ONLY
// way this reads true on a fresh reload is a crash before either of
// those ever got to run.
func (s *Session) hasUnfinalizedTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnUnsettled
}

// markTurnSettled clears turnUnsettled and durably records that this
// session's most recent turn attempt reached a settled outcome — see
// turnUnsettled's own doc comment and recChildTurnSettled's doc comment
// (store.go) for the full reasoning. The in-memory clear happens here,
// synchronously; the durable write is the caller's own responsibility
// (SessionManager.finalizeTurn/recoverInterruptedTurnLocked both run
// under m.mu and defer the actual persistTurnSettled call via
// SessionManager.deferPersist, mirroring every other durable write those
// methods make — see unlockAndFlushPersist's own doc comment for why).
func (s *Session) markTurnSettled() {
	s.mu.Lock()
	s.turnUnsettled = false
	// committedOutcome deliberately NOT cleared here — see its own doc
	// comment for why it now does double duty: recovery's own
	// crash-replay payload WHILE the turn it describes is still
	// unsettled, AND (once settled) the last known terminal outcome a
	// LATER adoption of this already-settled node restores n.status/
	// n.result/n.failReason from (SessionManager.restoreKnownStatusLocked)
	// — a live prod finding: adoptLocked's own StatusIdle default was
	// otherwise never corrected for a node that was already fully
	// settled BEFORE this specific adoption. appendWithUsage's own
	// identical field is what actually invalidates a STALE value, the
	// moment a genuinely NEW turn starts — that is the correct
	// invalidation point, not "the turn this describes just settled."
	s.mu.Unlock()
}

// persistTurnSettled durably writes the recChildTurnSettled marker —
// markTurnSettled's deferred counterpart, run after m.mu has been
// released (see that method's own doc comment).
func (s *Session) persistTurnSettled() {
	s.mu.Lock()
	if s.cfg.SessionDir != "" {
		if err := s.ensureLog(); err != nil {
			s.lastPersistErr = err
		} else if err := s.writeRecord(record{Type: recChildTurnSettled}); err != nil {
			s.lastPersistErr = err
		}
	}
	s.mu.Unlock()
}

// commitTurnOutcome records n, in memory, as the AUTHORITATIVE outcome
// SessionManager (finalizeTurn or recoverInterruptedTurnLocked itself)
// computed for s's current, still-unsettled turn — see committedOutcome's
// own doc comment for the full mechanism and why this exists. The durable
// write is the caller's own responsibility, deferred via
// SessionManager.deferPersist exactly like every other durable write
// those two methods make (see persistCommittedTurnOutcome).
func (s *Session) commitTurnOutcome(n taskNotification) {
	s.mu.Lock()
	nn := n
	s.committedOutcome = &nn
	s.mu.Unlock()
}

// persistCommittedTurnOutcome durably writes n as a recTaskOutcomeCommitted
// record — commitTurnOutcome's deferred counterpart. Takes n explicitly,
// the same captured-value convention every other deferPersist thunk in
// this package uses, rather than re-reading s.committedOutcome at persist
// time: by the time this runs, a LATER call in the SAME critical section
// (markTurnSettled, or a subsequent commitTurnOutcome for a retry) may
// already have overwritten or cleared it in memory, but the value that
// needs to land on disk is always the one THIS specific call was queued
// for.
func (s *Session) persistCommittedTurnOutcome(n taskNotification) {
	s.mu.Lock()
	s.persistTaskNotifyLocked(recTaskOutcomeCommitted, n)
	s.mu.Unlock()
}

// committedTurnOutcome returns s's currently committed outcome (see
// committedOutcome's own doc comment), if any.
func (s *Session) committedTurnOutcome() (taskNotification, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committedOutcome == nil {
		return taskNotification{}, false
	}
	return *s.committedOutcome, true
}

// settledSuccessResult reports whether s's own trailing history message is
// an unambiguous, natural completion of its last turn — narrowly used by
// SessionManager.recoverInterruptedTurnLocked to tell a genuine mid-turn
// crash apart from a crash that struck AFTER the turn actually finished but
// BEFORE finalizeTurn's own bookkeeping (the parent-facing notification,
// then the child_turn.settled marker — see hasUnfinalizedTurn's own doc
// comment) durably landed: the "notify->settled window" a live review named
// directly. Before this existed, recoverInterruptedTurnLocked always
// reported a StatusFailed "lost to restart" notification for ANY detected
// crash, including this one — telling a parent its successful child failed,
// which the review judged worse than a lost notification (a lost
// notification is at least honestly absent; a false failure actively
// misinforms, permanently, since nothing else will ever correct it).
//
// Deliberately narrow, and NOT a revival of the deleted hasUnansweredTurn's
// own broad "was this turn interrupted at all" gate — that heuristic was
// proven unreliable in BOTH directions (see turnUnsettled's own doc
// comment) precisely because it tried to answer that broad question from
// trailing shape alone. hasUnfinalizedTurn already answers the broad
// question reliably today; this helper only answers a narrower one — GIVEN
// that recovery is correctly firing, did this specific turn actually reach
// a natural end? — for exactly one unambiguous shape: a trailing
// RoleAssistant message carrying no ToolCall part. By construction (see
// runAgenticLoop, engine.go: the `if stop != provider.StopToolUse` and
// `len(results) == 0` branches are the ONLY paths that return a final
// asst without looping again, and both call appendUnexecutedToolCallResults
// immediately afterward, which is a no-op unless asst itself carries a
// ToolCall part), a trailing assistant message with no ToolCall part is not
// a probabilistic guess at "probably done" — it IS the exact shape
// runAgenticLoop produces on its one and only natural-return path when the
// model's own response requested no further action, whatever crashed
// afterward.
//
// Narrower than the full space of natural completions on purpose: the rarer
// NEP-5272 "wedge" shape (a non-StopToolUse stop reported ALONGSIDE
// ToolCall parts — see unexecutedToolCallStopReasonTextFmt's own doc
// comment) also completes naturally, but leaves a trailing SYNTHETIC
// RoleTool closer instead, one step removed from asst. That shape is
// deliberately NOT detected here: distinguishing a synthetic closer from a
// genuine, still-pending tool result would require pattern-matching
// synthesized text content, itself a heuristic this fix does not want to
// add another one of. A crash in that narrower window still falls back to
// the safe, honest "lost to restart" report below — never a false success,
// only a residual, explicitly accepted case of what this fix does not
// claim to cover.
func (s *Session) settledSuccessResult() (result string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) == 0 {
		return "", false
	}
	last := s.history[len(s.history)-1]
	if last.Role != message.RoleAssistant {
		return "", false
	}
	for _, p := range last.Parts {
		if _, isToolCall := p.(*message.ToolCall); isToolCall {
			return "", false
		}
	}
	return last.Parts.Text(), true
}

// hasTrailingSyntheticCloser reports whether s's own trailing history
// message is ALREADY one of recoverInterruptedTurnLocked's own synthetic
// closing messages (see isRecoverySyntheticCloser, session_manager.go) —
// used to guard against appending a SECOND one on a recovery-of-recovery
// retry (step 3 of the crash-window table on that method's own doc
// comment): a crash between that append's own durable write and the
// settled-marker's would otherwise leave the next recovery attempt
// re-appending the identical synthetic message into history on every
// retry until the settled marker finally lands. Named for "a" synthetic
// closer, not "the" lost-to-restart one specifically — recovery has two
// now (see canceledInterruptedText's own doc comment for the other),
// and this check must recognize whichever one a given turn's own
// recovery attempt actually appended.
func (s *Session) hasTrailingSyntheticCloser() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) == 0 {
		return false
	}
	return isRecoverySyntheticCloser(s.history[len(s.history)-1])
}

// Plugins returns a snapshot of this session's configured plugins — name,
// spawn state, registered tools, and subscribed hooks — for the session_info
// tool and GET /session. A session with no plugin host returns an empty
// slice, never nil, so the field always serializes to a JSON array.
func (s *Session) Plugins() []plugin.Info {
	if s.cfg.Hooks == nil {
		return []plugin.Info{}
	}
	infos := s.cfg.Hooks.Plugins()
	if infos == nil {
		return []plugin.Info{}
	}
	return infos
}

// Usage returns cumulative token usage across all turns.
func (s *Session) Usage() provider.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

// LastUsage returns the most recently completed model turn's own Usage
// (that one request's input/output/cache tokens, not the running total —
// see Usage), and whether any turn has completed yet (for this session,
// live or reloaded from its log — see appendWithUsage and LoadSession's
// recMessage case). ok is false for a session that has never completed a
// turn in any process.
func (s *Session) LastUsage() (usage provider.Usage, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsage, s.haveLastUsage
}

// LastActivityAt returns the timestamp of the most recently appended
// message (user, assistant, or tool), or CreatedAt if no message has been
// appended yet.
//
// This exists (issue #62 layer 3) because fleet monitors previously had no
// direct way to answer "is this session still doing something" — they
// sampled Session.Seq (server/journal.go) twice, a session apart, and
// compared, to distinguish quiet progress from a session wedged mid-turn.
// That double-sample is slow, racy against a session legitimately paused
// between polls (e.g. a goal loop's between-turn gap, worker turn done and
// the evaluator hasn't answered yet), and only ever answers a RELATIVE
// question ("did anything happen between my two samples"), never an
// absolute one ("how long has this been quiet"). LastActivityAt answers the
// absolute question directly from state the engine already holds: resident
// in memory for a live session, and — because every message record carries
// its own CreatedAt — equally available the moment a non-resident session
// is reloaded from its log, with no extra bookkeeping.
func (s *Session) LastActivityAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) == 0 {
		return s.createdAt
	}
	// A log written before message records carried CreatedAt replays with
	// zero timestamps — fall back to createdAt so a fleet monitor never
	// reads "0001-01-01" as infinite staleness.
	if t := s.history[len(s.history)-1].CreatedAt; !t.IsZero() {
		return t
	}
	return s.createdAt
}

// CompactionCount returns how many times this session has been compacted
// (live or replayed from its log — see LoadSession's recCompact case), and
// LastCompactedAt returns when the most recent one landed (the zero Time if
// never). Together they are what GET /session surfaces so a UI can tell
// compaction happened. See docs/design/context-compaction.md.
func (s *Session) CompactionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compactCount
}

// LastCompactedAt returns the timestamp of the most recent compaction, or
// the zero Time if this session has never been compacted.
func (s *Session) LastCompactedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCompactedAt
}

// toolExecutions returns the current tool-execution counter (see
// toolExecCount). It only ever increases, and only when a tool actually
// runs, never when one is blocked by tool.execute.before.
func (s *Session) toolExecutions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toolExecCount
}

// History returns a copy of the session's message history.
func (s *Session) History() []message.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]message.Message(nil), s.history...)
}

func (s *Session) append(m message.Message) {
	s.appendWithUsage(m, nil)
}

// appendWithUsage is append's usage-carrying variant, used for the one
// assistant message that ends a model turn (see Prompt): usage is the
// provider's per-turn Usage for that message, or nil for every other
// message (user, tool, or an interrupted partial assistant message with no
// completed turn to report). Recording it on the message record itself
// (see store.go's record.Usage) is what makes Session.Usage()/LastUsage()
// survive a process restart — LoadSession sums every record's Usage back
// into cumulative usage and keeps the last one seen as lastUsage, since the
// log has no separate cumulative-usage record to replay instead.
func (s *Session) appendWithUsage(m message.Message, usage *provider.Usage) {
	m.Normalize()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	s.history = append(s.history, m)
	// See turnUnsettled's own doc comment: any append means a turn has
	// started (or is still in progress) without yet being finalized.
	s.turnUnsettled = true
	// See committedOutcome's own doc comment: THIS is the "a new turn
	// started" invalidation point — appendWithUsage is the live turn-
	// driving append (runAgenticLoop, via Prompt), never
	// recoverInterruptedTurnLocked's own synthetic closing annotation
	// (that goes through appendMemoryOnly instead, which deliberately
	// does NOT clear this — see its own doc comment), so every call here
	// really does mean whatever outcome was committed for a PRIOR turn
	// no longer applies.
	s.committedOutcome = nil
	if usage != nil {
		s.usage.InputTokens += usage.InputTokens
		s.usage.OutputTokens += usage.OutputTokens
		s.usage.CacheReadTokens += usage.CacheReadTokens
		s.usage.CacheWriteTokens += usage.CacheWriteTokens
		s.lastUsage = *usage
		s.haveLastUsage = true
	}
	s.persistMessage(&m, usage)
	s.mu.Unlock()
}

// appendMemoryOnly is append's split-in-two sibling for
// recoverInterruptedTurnLocked specifically — the ONE caller in this
// package that needs a message append to interleave with
// SessionManager's own deferred-persist ordering (see
// SessionManager.deferPersist/unlockAndFlushPersist's own doc comment)
// instead of persisting synchronously the moment it is called, the way
// every other append in this package still correctly does. This is
// deliberately NOT a general split of Session.append/appendWithUsage —
// every other call site's own ordering requirements are untouched and
// unexamined by this change; broadening message-append persistence to
// defer-outside-m.mu generally is a substantially larger change than
// this one caller's own narrow crash-window requirement, and out of
// scope here.
//
// Normalizes and stamps CreatedAt exactly like appendWithUsage, appends
// to s.history, but does NOT call persistMessage — returns the
// normalized message so the caller can pass the IDENTICAL value to
// persistAppendedMessage later, once m.mu has been released, rather than
// risk a second Normalize/CreatedAt pass producing a subtly different
// persisted record than what is already sitting in memory.
func (s *Session) appendMemoryOnly(m message.Message) message.Message {
	m.Normalize()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	s.history = append(s.history, m)
	// See turnUnsettled's own doc comment — same as appendWithUsage.
	// recoverInterruptedTurnLocked's own closing-message append (this
	// method's one caller) relies on calling markTurnSettled AFTER this,
	// not before, so that call's turnUnsettled=false is what wins.
	s.turnUnsettled = true
	// Deliberately does NOT clear committedOutcome, unlike
	// appendWithUsage's own identical-looking line — see committedOutcome's
	// own doc comment. This method's one caller only ever appends its own
	// synthetic lostToRestartText closer for the SAME turn recovery is
	// still in the middle of settling, never a genuinely new turn's real
	// message — clearing here would erase the very commit a LATER
	// recovery-of-recovery pass needs to replay verbatim.
	s.mu.Unlock()
	return m
}

// persistAppendedMessage durably writes m — the deferred counterpart to
// appendMemoryOnly. m must be the EXACT value appendMemoryOnly returned
// (already normalized and timestamped), not a freshly-built one, so the
// durable record matches what is already in s.history byte-for-byte.
func (s *Session) persistAppendedMessage(m message.Message) {
	s.mu.Lock()
	s.persistMessage(&m, nil)
	s.mu.Unlock()
}

// accumulateDiscardedTurnUsage folds a billed-but-discarded turn's Usage
// into cumulative Usage() only — it never touches lastUsage/haveLastUsage.
// streamTurnWithRetry (prompt_retry.go) calls this for every attempt it
// discards as an empty turn (see emptyTurnError), whether that attempt is
// about to be retried or is the final one on budget exhaustion.
//
// This mirrors the accounting compact.go's errEmptyCompactionSummary skip
// path already established for the sibling "the call ran and cost real
// tokens even though it produced nothing usable" shape (see runCompaction's
// doc comment and AGENTS.md's "An empty summary is a graceful no-op..."):
// the call was real, the provider billed it in full (for an empty turn,
// typically a full input prefill plus the entire max_tokens output
// ceiling), and no tokens were refunded just because streamTurnWithRetry
// chose to discard the resulting message. Dropping this usage silently
// would undercount GET /session by the full cost of every discarded
// attempt — up to PromptRetries+1 fully-billed calls for one turn.
//
// lastUsage is deliberately left untouched, matching the compact.go
// precedent's own reasoning: maybeAutoCompact reads LastUsage as "how large
// is the next worker request" to decide whether to trigger, so it must keep
// reflecting the last REAL (actionable) prompt shape. Letting a discarded
// empty attempt set it would let a fully-billed max-output turn that
// produced nothing mask (or, worse, falsely trip) the very compaction
// signal it has nothing to do with — the harness would be sizing the next
// request off a turn that was thrown away.
//
// Like the compact.go skip path, this accumulation is live-only: there is
// no message to attach the usage to (the discarded attempt's assistant
// message is never appended to history — see emptyTurnError's doc
// comment), so persistMessage never runs for it and the total does not
// survive a process restart. Known residual, same tradeoff already accepted
// on the compaction skip path.
func (s *Session) accumulateDiscardedTurnUsage(usage provider.Usage) {
	s.mu.Lock()
	s.usage.InputTokens += usage.InputTokens
	s.usage.OutputTokens += usage.OutputTokens
	s.usage.CacheReadTokens += usage.CacheReadTokens
	s.usage.CacheWriteTokens += usage.CacheWriteTokens
	s.mu.Unlock()
}

func (s *Session) emit(ev Event) {
	ev.SessionID = s.ID
	if s.cfg.OnEvent != nil {
		s.cfg.OnEvent(ev)
	}
}

func (s *Session) emitStatus(status string) {
	if s.cfg.Hooks == nil {
		return
	}
	props, _ := json.Marshal(map[string]string{"status": status})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventSessionStatus,
		SessionID:  s.ID,
		Properties: props,
	}})
}

// emitFileEdited notifies plugins that a built-in file tool successfully
// wrote path (absolute).
func (s *Session) emitFileEdited(path string) {
	if s.cfg.Hooks == nil {
		return
	}
	props, _ := json.Marshal(plugin.FileEditedProperties{Path: path})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventFileEdited,
		SessionID:  s.ID,
		Properties: props,
	}})
}

// emitToolExecuteStart/emitToolExecuteEnd bracket the actual execution of a
// tool call (built-in or plugin-provided). They do not fire for calls denied
// by tool.execute.before, since those never execute.
func (s *Session) emitToolExecuteStart(tool, callID string) {
	if s.cfg.Hooks == nil {
		return
	}
	props, _ := json.Marshal(plugin.ToolExecuteStartProperties{Tool: tool, CallID: callID})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventToolExecuteStart,
		SessionID:  s.ID,
		Properties: props,
	}})
}

func (s *Session) emitToolExecuteEnd(tool, callID string, ok bool) {
	if s.cfg.Hooks == nil {
		return
	}
	props, _ := json.Marshal(plugin.ToolExecuteEndProperties{Tool: tool, CallID: callID, OK: ok})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventToolExecuteEnd,
		SessionID:  s.ID,
		Properties: props,
	}})
}

// emitSessionError notifies plugins that a prompt/turn terminated with an
// error. The error string is passed through plugin.SanitizeSessionError
// first: it caps the length and best-effort redacts obvious credential
// shapes (bearer tokens, Authorization header values, key=value secrets)
// that provider adapters can embed in wrapped HTTP errors — see
// SanitizeSessionError and PROTOCOL.md. This is best-effort, not a
// guarantee against every possible leak.
//
// context.Canceled is deliberately excluded: a cancelled context is an
// operator-initiated stop (POST /abort, DELETE /goal, server drain), not a
// failure — the server layer draws the same line (runPrompt/runGoal in
// server/handlers.go journal it as session.aborted / a clean stop, never
// session.error). Emitting session.error for every cancellation would be
// noisy and misleading to plugins reacting to it as a real fault.
func (s *Session) emitSessionError(err error) {
	if s.cfg.Hooks == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	props, _ := json.Marshal(plugin.SessionErrorProperties{Message: plugin.SanitizeSessionError(err.Error())})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventSessionError,
		SessionID:  s.ID,
		Properties: props,
	}})
}

// Prompt appends a user message and runs the agent loop — stream a turn,
// execute any tool calls, feed results back — until the model ends its turn.
// It returns the final assistant message. A thin, origin-less wrapper around
// PromptWithOrigin for the overwhelming majority of callers that never need
// to set one.
func (s *Session) Prompt(ctx context.Context, text string) (*message.Message, error) {
	return s.PromptWithOrigin(ctx, text, "")
}

// PromptEngineResume is Prompt's sibling for SessionManager.triggerResumeLocked's
// engine-initiated resume turn: identical behavior, but tags the appended
// user message with message.OriginEngine so a client can render it as a
// system notice rather than a human-typed bubble — see Message.Origin's own
// doc comment. The ONLY caller is triggerResumeLocked; every other synthetic
// or programmatic turn driver (the goal loop's own directive text, notably)
// still goes through plain Prompt, unchanged.
func (s *Session) PromptEngineResume(ctx context.Context, text string) (*message.Message, error) {
	return s.PromptWithOrigin(ctx, text, message.OriginEngine)
}

// PromptWithOrigin is Prompt/PromptEngineResume's shared, exported body,
// parameterized on the appended message's Origin tag. Exported (not just
// promptWithOrigin, package-private) so a caller that already HOLDS an
// origin value of its own — server/handlers.go's runPrompt, notably, which
// is reached from four call sites that each already know statically
// whether their own text is the engine's resume trigger or not — can pass
// it straight through in one call, rather than re-deriving Prompt vs.
// PromptEngineResume's own two-arm choice a second time on top of a choice
// the caller already made. One parameterized entry point for the value
// means a future third Origin value is handled in exactly one place.
func (s *Session) PromptWithOrigin(ctx context.Context, text string, origin string) (*message.Message, error) {
	// Load project instructions once, before mutating history: a
	// present-but-unusable AGENTS.md fails the prompt without recording a
	// user message or calling the provider.
	if err := s.ensureInstructions(); err != nil {
		s.emitSessionError(err)
		return nil, err
	}
	// Discover Agent Skills once, before mutating history: a malformed
	// SKILL.md or a duplicate skill name fails the prompt without recording a
	// user message or calling the provider (see skills.go).
	if err := s.ensureSkills(); err != nil {
		s.emitSessionError(err)
		return nil, err
	}
	// Automatic compaction check (docs/design/context-compaction.md §1):
	// runs on every call, bare or goal-loop-driven alike, since PursueGoal
	// drives everything through Prompt. Deliberately BEFORE the incoming
	// user message is appended below: a turn boundary always falls on a
	// completed-turn edge, the summary never has to account for a prompt
	// that hasn't been answered yet, and the just-arrived message can never
	// be folded into its own summary. Best-effort: a failed or skipped
	// compaction never blocks the real turn (see maybeAutoCompact).
	s.maybeAutoCompact(ctx)
	s.append(message.Message{
		ID:        newID("msg"),
		Role:      message.RoleUser,
		Parts:     message.Parts{&message.Text{Text: text}},
		CreatedAt: time.Now().UTC(),
		Origin:    origin,
	})
	return s.runAgenticLoop(ctx)
}

// runAgenticLoop drives the agentic loop — stream a turn, execute any tool
// calls, feed results back — against history AS IT STANDS, until the model
// ends its turn. It returns the final assistant message.
//
// This is Prompt's own loop body, split out unchanged: Prompt still calls it
// as its last step, right after appending the user message, so Prompt's own
// observable behavior — emitted events, emitStatus("busy")/("idle"), usage
// accounting, and the appendUnexecutedToolCallResults/DequeueAllPrompts
// ordering below — is identical to before the split.
//
// The split exists so a caller that already holds an appended, still-
// unanswered directive at the tail of history can drive that SAME message
// through the loop without appending a second copy. promptTurnWithRetry
// (goal.go) is that caller: a goal-loop retry whose tail is exactly one
// unanswered directive (see directiveReuseEligible) calls this directly
// instead of Prompt, so the retried attempt answers the existing message
// rather than duplicating it — see
// docs/design/goal-retry-directive-reuse.md.
func (s *Session) runAgenticLoop(ctx context.Context) (*message.Message, error) {
	s.emitStatus("busy")
	defer s.emitStatus("idle")

	for {
		// streamTurnWithRetry is a drop-in for streamTurn that smooths a
		// one-off retryable provider error (a momentary HTTP 5xx/429/529 or a
		// truncated stream) with a small, bounded budget (Config.PromptRetries)
		// so an interactive user never sees a transient blip. It returns the
		// SAME error shapes streamTurn does on the paths this loop already
		// handles below — an *interruptedTurnError (whose partial is appended
		// here), a context.Canceled, or an exhausted/non-retryable failure —
		// so everything below is unchanged. See its doc comment.
		asst, stop, usage, err := s.streamTurnWithRetry(ctx)
		if err != nil {
			// Whatever this attempt's own streamTurn calls checked out
			// (checkoutTaskNotificationsSegment, engine.go's streamTurn)
			// never reached a request that survived — return them to
			// pending so a LATER turn gets another chance, rather than
			// losing them the moment this one fails. See
			// requeueTaskNotifications' doc comment.
			s.requeueTaskNotifications()
			var interrupted *interruptedTurnError
			if errors.As(err, &interrupted) {
				// The turn died after the model already emitted one or
				// more tool_call blocks: append the model's intent and
				// synthetic (never silently dropped) results for it
				// before surfacing the failure, so history stays
				// protocol-valid for every later request build — see
				// interruptedTurnError's doc comment.
				s.append(*interrupted.partial)
				s.emit(Event{Type: EventMessage, Message: interrupted.partial})
				toolMsg := interruptedToolResults(interrupted.partial)
				s.append(toolMsg)
			}
			s.emitSessionError(err)
			return nil, err
		}
		// This attempt succeeded and its result is about to be kept
		// (appended below) — whatever was checked out for it really was
		// delivered in the request that produced asst. Commit BEFORE
		// appending, not after: the notification was already visible to
		// the MODEL that produced this very response, so it counts as
		// delivered regardless of what happens to asst next.
		s.commitTaskNotifications()
		s.appendWithUsage(*asst, &usage)
		s.emit(Event{Type: EventMessage, Message: asst, StopReason: stop, Usage: &usage})

		if stop != provider.StopToolUse {
			// See appendUnexecutedToolCallResults (NEP-5272): a provider
			// reporting a non-tool_use stop reason ALONGSIDE tool_use
			// blocks is exactly the shape that permanently wedged three
			// production boxes -- asst just got appended two lines above,
			// so if it carries any ToolCall parts, this appends their
			// synthetic results before Prompt ever returns, closing the
			// hole instead of leaving them orphaned in history.
			s.appendUnexecutedToolCallResults(asst, stop)
			return asst, nil
		}
		results := s.runToolCalls(ctx, asst)
		if len(results) == 0 {
			// tool_use stop with no tool calls: treat as end of turn
			// rather than looping forever. runToolCalls only ever omits a
			// ToolCall from results if asst carried none to begin with
			// (every ToolCall part it does find gets exactly one result),
			// so this call is a defensive no-op today -- kept for the same
			// reason as the branch above: a future producer of asst that
			// ever DOES leave a ToolCall unexecuted here must not get a
			// free pass on the orphan invariant.
			s.appendUnexecutedToolCallResults(asst, stop)
			return asst, nil
		}
		s.append(message.Message{
			ID:        newID("msg"),
			Role:      message.RoleTool,
			Parts:     results,
			CreatedAt: time.Now().UTC(),
		})
		// Tool-call-boundary queue drain (docs/plans/2026-07-19-prompt-queue.md's
		// "Design amendment: tool-call-boundary injection"): the model is
		// about to make ANOTHER provider request in THIS SAME turn (tool
		// results just landed, stop reason was StopToolUse and at least one
		// tool actually ran) — this is the earliest point a prompt that
		// arrived mid-tool-execution can be delivered without waiting for
		// the whole turn (or, in a goal loop, the whole worker turn) to
		// finish, matching Claude Code's mid-turn steering granularity. A
		// turn that ends with no tool calls never reaches this point at
		// all (see the two early returns above) — that path is unchanged
		// and left entirely to the server's tail drain / the goal loop's
		// own turn-boundary drain.
		//
		// DequeueAllPrompts drains the ENTIRE queue, FIFO, in one locked
		// operation and journals every prompt.dequeued(injected) record
		// BEFORE this method returns — so, exactly like the goal-boundary
		// drain in goal.go, a crash between that journal write and this
		// append can never double-deliver: the prompt is simply gone from
		// the queue on replay. The rendered content is the same labeled
		// "OPERATOR MESSAGES" block goal-turn-boundary injection uses
		// (operatorMessagesBlock, queue.go), differing only in the
		// trailing clause: this call site passes operatorContextTask, not
		// operatorContextGoal, since this loop has no goal directive to
		// hand back to — even when it happens to be driving a goal loop's
		// worker turn (see operatorMessagesBlock's doc comment).
		//
		// This appends a REAL, durable user message straight into history
		// (never an ephemeral segment like the managed-processes status
		// block near streamTurn below) — appending only, never touching an
		// earlier message, so any provider's prompt-cache prefix stays
		// intact exactly per the managed-processes precedent, except this
		// one really is delivered mail, not a disposable status line.
		if queued := s.DequeueAllPrompts("injected"); len(queued) > 0 {
			s.append(message.Message{
				ID:        newID("msg"),
				Role:      message.RoleUser,
				Parts:     message.Parts{&message.Text{Text: strings.TrimSuffix(operatorMessagesBlock(queued, operatorContextTask), "\n")}},
				CreatedAt: time.Now().UTC(),
			})
		}
	}
}

// streamTurn makes one model call and returns the assembled assistant
// message.
func (s *Session) streamTurn(ctx context.Context) (*message.Message, provider.StopReason, provider.Usage, error) {
	// Assembly order matters, and three steps are pinned:
	//
	//  1. chat.params runs first: it fixes params.Model, which both the
	//     provider lookup and system.transform below need. It still fires
	//     on every turn. system.transform, by contrast, now runs after
	//     provider resolution, so a turn naming an unconfigured provider
	//     returns WITHOUT firing it (it used to fire, then fail):
	//     assembling a system prompt for a request that is never sent buys
	//     nothing.
	//  2. Providers.For runs BEFORE the tool plan. The plan's
	//     s.cfg.MCP.Tools(ctx) call is what triggers a server's first
	//     connect attempt (see MCPManager.ensureConnected) — network dials,
	//     and a spawned child process for every stdio server. Resolving the
	//     provider first keeps a turn that names an unconfigured provider
	//     from spawning anything at all: it returns here, exactly as it did
	//     before the plan existed. Provider resolution is a pure map lookup,
	//     so paying for it first costs nothing.
	//  3. The tool plan runs before the system slice is finished and before
	//     the ambient segments: its catalog half IS a system segment (see
	//     mcp_lazy.go), and its Tools(ctx) call must precede
	//     mcpStatusSegment, or Status() is read against stale pre-connect
	//     state and this turn's own connect failure is reported one turn
	//     late.
	params := plugin.ChatParams{Model: s.Model()}
	if s.cfg.Hooks != nil {
		params = s.cfg.Hooks.ChatParams(ctx, &plugin.ChatParamsRequest{SessionID: s.ID, Params: params})
		if params.Model.IsZero() {
			params.Model = s.Model()
		}
	}

	prov, err := s.cfg.Providers.For(params.Model)
	if err != nil {
		return nil, "", provider.Usage{}, err
	}

	tools, mcpCatalog := s.toolDefsWithCatalog(ctx)

	system := append([]string(nil), s.cfg.System...)
	// Project instructions sit after the base system prompt and before any
	// hook-contributed segments (see ensureInstructions in instructions.go).
	if seg := s.instructionSegment(); seg != "" {
		system = append(system, seg)
	}
	// The Agent Skills catalog sits after project instructions and, like
	// them, before any hook-contributed segments (see ensureSkills in
	// skills.go).
	if seg := s.skillsSegment(); seg != "" {
		system = append(system, seg)
	}
	// The deferred-MCP catalog sits after the skills catalog — the same
	// progressive-disclosure stage-1 role — and, like it, before any
	// hook-contributed segments. Empty for every session that defers
	// nothing (see mcp_lazy.go).
	if mcpCatalog != "" {
		system = append(system, mcpCatalog)
	}
	if s.cfg.Hooks != nil {
		system = append(system, s.cfg.Hooks.SystemTransform(ctx, &plugin.SystemTransformRequest{
			SessionID: s.ID,
			Model:     params.Model,
		})...)
	}

	maxTokens := s.cfg.MaxTokens
	if params.MaxTokens != nil {
		maxTokens = *params.MaxTokens
	}
	// Ambient process-status, MCP-status, parked-goal-status, and
	// engine-identity injection (see processStatusSegment,
	// mcpStatusSegment, goalParkedSegment, identityStatusSegment):
	// appended ONLY to this
	// in-memory request copy — s.History() already returns a fresh slice
	// (engine.go's append(nil, s.history...)), and withAmbientStatus
	// clones (never mutates in place) the one message it touches, so the
	// durable s.history — and the message/journal log it is persisted
	// from — never sees this text. Each segment rides only the newest
	// user message so every earlier message, and therefore the cached
	// request prefix, is byte-identical to a request built with no
	// process ever started and every MCP server healthy.
	//
	// The tool plan already ran above (see the numbered ordering note at the
	// top of this function), so every segment below reads post-connect
	// state and req.Tools is the plan's own slice, never a second
	// computation.
	messages := s.History()
	if seg := processStatusSegment(s.cfg.Processes, s.cfg.WorkDir); seg != "" {
		messages = withAmbientStatus(messages, seg)
	}
	if seg := mcpStatusSegment(s.cfg.MCP); seg != "" {
		messages = withAmbientStatus(messages, seg)
	}
	if seg := goalParkedSegment(s); seg != "" {
		messages = withAmbientStatus(messages, seg)
	}
	if seg := identityStatusSegment(s.cfg.EngineVersion, s.cfg.StartedAt, s.cfg.SessionSync); seg != "" {
		messages = withAmbientStatus(messages, seg)
	}
	// Unlike the four segments above, this one CHECKS OUT pending
	// notifications rather than idempotently recomputing a status string —
	// see checkoutTaskNotificationsSegment's doc comment. Committing them
	// as delivered (or requeuing them on failure) happens one layer up, in
	// runAgenticLoop, once this WHOLE turn's outcome — including any
	// retries streamTurnWithRetry runs — is known.
	if seg := s.checkoutTaskNotificationsSegment(); seg != "" {
		messages = withAmbientStatus(messages, seg)
	}
	req := &provider.Request{
		Model:       params.Model,
		System:      system,
		Messages:    messages,
		Tools:       tools,
		Temperature: params.Temperature,
		TopP:        params.TopP,
		MaxTokens:   maxTokens,
		// Effort is read straight from session state, deliberately NOT routed
		// through the chat.params hook (v1 scope). The design gives per-model
		// level gating to the CALLER's own layer (the boxes API validates a
		// level against the model before POST /session/{id}/thinking), not to a
		// harness plugin, so a chat.params Effort override buys nothing the box
		// path uses. Adding Effort to plugin.ChatParams is a clean future
		// enhancement if a plugin ever needs to rewrite it per request.
		//
		// This reads s.Effort() fresh every request (and every tool round, since
		// runAgenticLoop rebuilds the request per round), so a SetModel swap to a
		// non-reasoning model while effort stays non-off ships a reasoning
		// control that model rejects — the SAME caller-gated trigger as the
		// off-toggle the adapters' downgrade strip handles. The caller therefore
		// re-validates/clears effort on every MODEL swap, not only before
		// POST /thinking; the boxes picker does this by clamping the level to the
		// new model's supported set on switch.
		Effort: s.Effort(),
		// SessionKey names this session for an adapter that forwards it as
		// a routing/cache-affinity hint (see provider.Request.SessionKey
		// doc comment; openaicompat sends it as the wire "user" field).
		SessionKey: s.ID,
	}

	// Record this turn's assembled system for the session_info tool, bump the
	// per-session turn counter, then hand the exact final request to the
	// observer immediately before the provider call. OnRequest must not mutate
	// req (it is shared with prov.Stream below).
	s.mu.Lock()
	s.turn++
	turn := s.turn
	s.lastSystem = append([]string(nil), system...)
	s.mu.Unlock()
	if s.cfg.OnRequest != nil {
		s.cfg.OnRequest(s.ID, turn, req)
	}

	// Idle-stream watchdog (see Config.StreamIdleTimeout and
	// armIdleWatchdog): armed BEFORE the dial so a Stream call that never
	// returns is bounded too, kicked on every event, cut by cancelling the
	// request's own child context — the same unblocking path a caller
	// abort takes through the adapter's HTTP body read.
	ctx, watch, release := s.armIdleWatchdog(ctx)
	defer release()
	stream, err := prov.Stream(ctx, req)
	if err != nil {
		return nil, "", provider.Usage{}, watch.explain(err)
	}
	defer stream.Close()

	// text and toolCalls accumulate this turn's content as it streams, so
	// that if the stream dies (or otherwise errors) before EventDone, any
	// tool_call already fully emitted is not simply lost — see the
	// EventToolCall case below and interruptedTurnError's doc comment for
	// why.
	var text strings.Builder
	var toolCalls []*message.ToolCall
	for {
		ev, err := stream.Next()
		if err != nil {
			// A watchdog-cut stream surfaces here as the child context's
			// cancellation; explain converts it into the classified
			// idle-timeout error (and passes every other failure — parent
			// aborts included — through untouched).
			err = watch.explain(err)
			if len(toolCalls) == 0 {
				// No tool call was ever recorded this turn: nothing can
				// be orphaned, so this is an ordinary turn failure —
				// identical to the pre-fix behavior.
				return nil, "", provider.Usage{}, err
			}
			return nil, "", provider.Usage{}, &interruptedTurnError{
				err:     err,
				partial: s.assemblePartial(text.String(), toolCalls),
			}
		}
		watch.kick()
		switch ev.Type {
		case provider.EventTextDelta:
			text.WriteString(ev.Text)
			s.emit(Event{Type: EventTextDelta, Text: ev.Text})
		case provider.EventReasoningDelta:
			s.emit(Event{Type: EventReasoningDelta, Text: ev.Text})
		case provider.EventToolCall:
			// A complete tool_use/tool_call block: the provider has
			// finished emitting its arguments (see
			// provider/anthropic/anthropic.go's content_block_stop and
			// provider/openaicompat/openaicompat.go's emitToolCalls), but
			// the turn has not reached EventDone yet, so the engine has
			// not (and must not, before the turn completes normally)
			// executed it. Recorded here purely so a later stream death
			// or error still has this call's identity to work with.
			toolCalls = append(toolCalls, ev.ToolCall)
		case provider.EventDone:
			return ev.Message, ev.StopReason, ev.Usage, nil
		}
	}
}

// assemblePartial builds the assistant message streamTurn returns (wrapped
// in an interruptedTurnError) when the stream errors after recording one or
// more tool calls but before EventDone. It mirrors the shape a provider
// adapter's own assemble (e.g. provider/anthropic/anthropic.go's
// stream.assemble) would produce for the same partial content: any
// accumulated text first, then the tool calls in emission order.
func (s *Session) assemblePartial(text string, toolCalls []*message.ToolCall) *message.Message {
	msg := &message.Message{
		ID:        newID("msg"),
		Role:      message.RoleAssistant,
		Model:     s.Model(),
		CreatedAt: time.Now().UTC(),
	}
	if text != "" {
		msg.Parts = append(msg.Parts, &message.Text{Text: text})
	}
	for _, tc := range toolCalls {
		msg.Parts = append(msg.Parts, tc)
	}
	return msg
}

// interruptedTurnErrorText is the Content text of the synthetic, is_error
// tool-role result the engine appends for every tool call recorded in a
// turn that ended abnormally before the engine could execute it (see
// interruptedTurnError). Exported as a constant (not exported from the
// package) so tests can assert on the exact string.
const interruptedTurnErrorText = "interrupted: tool call was never executed because the turn ended abnormally"

// interruptedTurnError is returned by streamTurn in place of the
// underlying stream/provider error when that error arrived after the
// stream had already emitted one or more complete tool_call blocks for the
// in-flight assistant message (via provider.EventToolCall) but before
// EventDone — i.e. before the engine could ever execute those calls.
//
// # Incident ses_01kx48z4rqfkpbwmzfdv1jzeg6
//
// A goal worker turn died with the Anthropic API 400 "tool_use ids were
// found without tool_result blocks immediately after", and every
// subsequent goal-loop retry then failed identically, killing the goal.
// The mechanism: a provider stream died (or the turn otherwise errored)
// after emitting one or more tool_call blocks but before the engine
// executed them. Before this fix, Prompt's error path
// (`if err != nil { return nil, err }`) simply discarded the assembled
// partial message, which sounds safe — nothing entered history — except
// that is exactly backwards from what actually poisons a session: the
// danger here is not a partial message appended without its result (the
// old truncated-Arguments incident's shape), it is that some OTHER call
// path (a provider adapter's own retry, a resumed session replaying a
// partially-journaled turn, a future change to this loop) could append
// such a message without this same care. Recording the tool calls here
// and synthesizing their results immediately — rather than leaving the
// model's already-emitted intent to either vanish or, worse, reappear
// unpaired from some other path later — is what keeps history
// self-consistent at ingest, mirroring the primary fix for the sibling,
// marshal-level incident (see message.Normalize's doc comment, "fix
// (message,engine): truncated ToolCall.Arguments must never poison
// history").
//
// Prompt handles this by appending partial (the assistant message,
// exactly as if the turn had completed with these tool calls) followed
// immediately by a synthetic tool-role message: one is_error ToolResult
// per recorded call, Content interruptedTurnErrorText. This preserves the
// model's visible intent (which tool it was calling, with what arguments)
// while keeping the transcript replayable — every subsequent request
// build sees a ToolCall immediately followed by its ToolResult, exactly
// as every provider wire protocol requires, instead of replaying the
// orphaned tool_use forever. The turn is still a failure: err (unwrapped
// via Unwrap) is what Prompt ultimately returns to its caller, unchanged
// from the caller's point of view — the goal loop's retry-count and
// tool-executed-before-failing bookkeeping (see promptTurnWithRetry) sees
// the same error it always would have, and toolExecCount is NOT
// incremented (these calls never ran), so a retry is exactly as safe as
// it always was for a turn that failed before executing anything.
//
// provider/anthropic/transcode.go and provider/openaicompat/transcode.go
// carry the defense-in-depth counterpart (message.ResolveOrphanToolCalls)
// for histories poisoned by any other producer; see that function's doc
// comment.
type interruptedTurnError struct {
	err     error
	partial *message.Message
}

func (e *interruptedTurnError) Error() string { return e.err.Error() }
func (e *interruptedTurnError) Unwrap() error { return e.err }

// interruptedToolResults builds the synthetic tool-role message Prompt
// appends immediately after an interruptedTurnError's partial assistant
// message: one is_error ToolResult per ToolCall part in partial, in order.
//
// Like every tool-result append, this message is persisted without an
// EventMessage emit — and since the interrupted calls never executed, no
// EventToolEnd fired either. A pure event-stream consumer therefore sees
// the partial assistant message with no following results for the
// interrupted calls until it reloads history (GET /message, LoadSession).
func interruptedToolResults(partial *message.Message) message.Message {
	return syntheticUnexecutedToolResults(partial, interruptedTurnErrorText)
}

// syntheticUnexecutedToolResults builds a tool-role message with one
// is_error ToolResult per ToolCall part in msg, in order, each carrying
// text. It is the one shared implementation behind every synthetic-result
// producer in this file: interruptedToolResults (a stream that died before
// EventDone, see interruptedTurnError above) and
// appendUnexecutedToolCallResults below (a stream that reached EventDone
// normally but with a stop reason other than StopToolUse, see NEP-5272) --
// both need the exact same "one result per unexecuted call" shape, and a
// second hand-rolled copy of this loop is exactly the kind of drift that
// leaves a future third case unpaired.
func syntheticUnexecutedToolResults(msg *message.Message, text string) message.Message {
	var results message.Parts
	for _, p := range msg.Parts {
		tc, ok := p.(*message.ToolCall)
		if !ok {
			continue
		}
		results = append(results, &message.ToolResult{
			CallID:  tc.CallID,
			Content: message.Parts{&message.Text{Text: text}},
			IsError: true,
		})
	}
	return message.Message{
		ID:        newID("msg"),
		Role:      message.RoleTool,
		Parts:     results,
		CreatedAt: time.Now().UTC(),
	}
}

// unexecutedToolCallStopReasonTextFmt is the Printf format behind the
// Content text of the synthetic, is_error tool-role result
// appendUnexecutedToolCallResults appends for a ToolCall the engine never
// executed because the turn's own stop reason wasn't StopToolUse. Kept as
// a package-level constant (rather than inlined) so a test can assert on
// the exact rendered string via fmt.Sprintf(unexecutedToolCallStopReasonTextFmt,
// stop) — see TestPersistTruncatedToolCallArguments — rather than
// duplicating the literal.
//
// # Incident NEP-5272 (boxes bumpy-grape, royal-cupcake, hyper-lemon, 2026-08-07)
//
// Three production boxes wedged permanently in one day. Wire capture from
// box hyper-lemon (session ses_01kze9vds5fxd89dtv4accqjcp, the
// Bedrock/bifrost provider path) showed a request with 44 tool_use blocks
// but only 43 tool_result blocks: the orphaned call sat at wire index 91,
// immediately followed by a plain user message. Anthropic's API then 400s
// every subsequent request with "tool_use ids were found without
// tool_result blocks immediately after" -- permanently, since nothing in
// this session's own history ever removed the orphan and every later
// Prompt call just appends new messages above it.
//
// The mechanism, unlike the sibling interruptedTurnError case above: the
// provider stream did NOT error and DID reach EventDone -- a completed,
// ordinary turn -- but its reported StopReason was something other than
// StopToolUse (StopEndTurn observed on the wire capture; nothing rules out
// StopMaxTokens or another value from a different route) while the
// assistant message it returned nonetheless carried one or more ToolCall
// parts. Session.Prompt's `if stop != provider.StopToolUse { return asst,
// nil }` early return appended asst (with its ToolCall parts) to history
// and returned -- with no following tool-role message ever appended for
// those calls. Every later request replay finds the same unpaired
// tool_use.
//
// The fix does NOT execute the orphaned calls: a StopMaxTokens stop can
// truncate ToolCall.Arguments mid-JSON (see the sibling incident fixed by
// "truncated ToolCall.Arguments must never poison history"), and running a
// tool against truncated, possibly invalid arguments is its own hazard --
// worse than a visible failure. Synthesizing an is_error result instead is
// deterministic, loses no information (the ToolCall itself, with the
// model's full original intent, stays in history right where it was), and
// converts a fatal, permanent wedge into an ordinary tool failure the
// model can see and react to on its very next turn -- exactly the
// "execute instead of synthesize" alternative considered and deliberately
// rejected here.
const unexecutedToolCallStopReasonTextFmt = "unexecuted: tool call was never run because the provider reported stop reason %q for this turn instead of tool_use"

// appendUnexecutedToolCallResults appends a synthetic tool-role message --
// one is_error ToolResult per ToolCall part found in asst, via
// syntheticUnexecutedToolResults -- if and only if asst carries at least
// one. It is a no-op for the overwhelmingly common case (a turn with no
// ToolCall parts at all), so every call site above can call it
// unconditionally right after appending asst to history, closing the
// NEP-5272 hole without disturbing any turn that never had an orphan risk
// in the first place.
//
// Like every other tool-result append in this file, this message is
// persisted without an EventMessage emit and without an EventToolEnd for
// the unexecuted calls (see interruptedToolResults's doc comment for why:
// a pure event-stream consumer sees the gap only until it reloads
// history). toolExecCount is never touched here -- these calls did not
// run, so a goal-loop retry of this same attempt remains exactly as safe
// as it always was (see promptTurnWithRetry's non-idempotency doc comment
// in goal.go).
func (s *Session) appendUnexecutedToolCallResults(asst *message.Message, stop provider.StopReason) {
	hasToolCall := false
	for _, p := range asst.Parts {
		if _, ok := p.(*message.ToolCall); ok {
			hasToolCall = true
			break
		}
	}
	if !hasToolCall {
		return
	}
	s.append(syntheticUnexecutedToolResults(asst, fmt.Sprintf(unexecutedToolCallStopReasonTextFmt, stop)))
}

// turnHasActionableContent reports whether asst carries content a caller
// can act on: a *message.Text part with non-empty Text, or a
// *message.ToolCall part. A message holding only a *message.Reasoning part
// (or no parts at all) is not actionable, even though the provider reported
// a clean EventDone — see emptyTurnError's doc comment for why this
// distinction exists.
//
// message.Part has five implementors: Text, ToolCall, Reasoning, Blob, and
// ToolResult. Blob and ToolResult are deliberately not checked here: no
// provider adapter emits either on an assistant turn today (a Blob is
// user/tool-supplied input; a ToolResult only ever appears on a tool-role
// message) — revisit this switch if a model-generated image (a Blob) ever
// lands on an assistant message. Whitespace-only Text (e.g. "   ") counts
// as actionable on purpose — the false-negative-safe direction: treating it
// as actionable risks the false negative of letting a genuinely degenerate
// whitespace-only reply through without a retry, which is a real but
// low-cost miss (the turn still carries whatever the model produced). A
// stricter "non-blank" definition would risk the opposite, worse mistake —
// a false positive that misclassifies real-but-sparse output as empty and
// burns the retry budget (and, at exhaustion, fails the turn outright) on
// content that never needed a retry.
func turnHasActionableContent(asst *message.Message) bool {
	for _, p := range asst.Parts {
		switch part := p.(type) {
		case *message.Text:
			if part.Text != "" {
				return true
			}
		case *message.ToolCall:
			return true
		}
	}
	return false
}

// emptyTurnError is streamTurnWithRetry's synthetic error for a turn that
// completed — streamTurn returned a nil error, so the provider stream
// itself never failed — but produced no actionable content per
// turnHasActionableContent: an assistant message with no non-empty Text and
// no ToolCall part, regardless of stop reason.
//
// # Incident: box fx-context-limits, session ses_01m0ga6v25f1h902fnmx98zhn3 (2026-08-20)
//
// Twice in the same session, sonnet-5's thinking consumed the entire
// max_tokens ceiling — output_tokens exactly 8192 (4096 EffortLow
// budget_tokens + 4096 thinkingCompletionMargin) — before emitting any text
// or tool call. The provider reported stop_reason "max_tokens" for a
// message that carried only a Reasoning part. Before this type existed,
// runAgenticLoop's `if stop != provider.StopToolUse { return asst, nil }`
// (see above, runAgenticLoop) treated this as an ordinary completed turn:
// no content validation, so the caller got back a message with nothing to
// show and the server journaled outcome:"completed" (server/handlers.go).
// The second occurrence was session-terminal — the task died silently with
// no error anywhere in the record.
//
// The identical hole is reachable through StopToolUse itself, not only a
// non-tool-use stop reason: provider/openaicompat's mapFinishReason maps
// the wire finish_reason "tool_calls" to StopToolUse unconditionally, so a
// proxied provider that reports "tool_calls" with an empty or dropped
// tool_calls array (observed on the bifrost→Fireworks path) produces the
// same zero-actionable-parts message under StopToolUse. runAgenticLoop's
// own `len(results) == 0` branch (see above, runAgenticLoop) already
// contemplates a StopToolUse turn with no ToolCall parts, but only to avoid
// looping forever on it — it still returns asst, nil, the same silent
// "success" this type exists to prevent. That is why
// turnHasActionableContent is NOT gated on the stop reason (see
// streamTurnWithRetry's doc comment, prompt_retry.go, for the full
// reasoning): gating on it would leave this StopToolUse shape uncaught.
//
// appendUnexecutedToolCallResults (NEP-5272, above) does not catch either
// shape: its hasToolCall guard is a no-op for a message with zero ToolCall
// parts, which both shapes are — that fix closes the orphaned-tool-call
// hole, not the no-content-at-all hole. The two never double-handle the
// same message: appendUnexecutedToolCallResults only ever runs on a message
// streamTurnWithRetry already accepted as actionable (it returned success),
// so by construction that message either carries a real ToolCall
// (appendUnexecutedToolCallResults' guard fires, as before) or real Text
// with no ToolCall (its guard stays a no-op, as before) — a
// zero-actionable-parts message never reaches runAgenticLoop at all now,
// successful or not.
//
// streamTurnWithRetry synthesizes this error when the condition above holds
// and routes it into the same bounded retry budget (Config.PromptRetries)
// used for a classified-retryable provider error, emitting the same
// EventTurnRestart before each retry. On budget exhaustion it returns this
// error unwrapped: server's turnEndOutcome (server/journal.go) maps every
// unrecognized error to outcome:"error", which is the correct terminal
// state — a completed-but-empty turn must never be recorded as success.
//
// A discarded attempt still billed real tokens (typically a full input
// prefill plus the entire max_tokens output ceiling), so streamTurnWithRetry
// folds its Usage into cumulative Session.Usage() via
// accumulateDiscardedTurnUsage before synthesizing this error — on every
// discarded attempt, retried or exhausted alike, never only the last one.
// See that function's doc comment for the accounting rule (mirrors the
// #136 empty-compaction-summary precedent) and why lastUsage is
// deliberately left untouched.
type emptyTurnError struct {
	stop         provider.StopReason
	outputTokens int
}

func (e *emptyTurnError) Error() string {
	return fmt.Sprintf("empty turn: provider reported stop reason %q with output_tokens=%d but produced no text and no tool call", e.stop, e.outputTokens)
}

// toolDefs merges built-in tools, MCP-provided tools, and plugin-provided
// ones, in that group order.
//
// The built-in group is SORTED BY NAME, and that sort is a prompt-cache
// requirement, not cosmetics. s.tools is a map, and Go randomizes map
// iteration on every range, so an unsorted build emitted a differently
// ordered tools array on every single request. Tools sit at the FRONT of the
// cached prefix on every provider (Anthropic caches tools, then system, then
// messages), so one reordering invalidated the WHOLE prefix and rewrote it:
// consecutive turns of one session each reported a full cache write and no
// cache read, for a byte-identical system prompt.
//
// The two other groups were already deterministic and keep their own order:
// MCP (MCPManager.rebuildToolsLocked sorts by server, then tool) and plugins
// (plugin.Host.Tools walks the configured instance slice). Sorting stays
// WITHIN the built-in group so adding an MCP server never reshuffles the
// built-in block that precedes it.
//
// The MCP group is the plan's defs (see mcp_lazy.go), which is the whole
// registry slice for an eager session and a FILTER of it -- order preserved
// -- for a session that defers. A filter of a deterministic slice is
// deterministic, so byte-stability holds either way: two requests that
// change no selection produce identical bytes.
//
// LOCKING: the caller must NOT hold s.mu. This is new as of deferral: for a
// session that can defer, the plan reaps stale selections under s.mu (see
// planMCPTools), and sync.Mutex is not reentrant. Before deferral this
// chain took no session lock at all, so a caller was free to call it under
// the lock. Both callers today are outside it by construction -- sessionInfo
// deliberately gathers tool names before its own Lock, and streamTurn builds
// the whole request before its s.mu section.
func (s *Session) toolDefs(ctx context.Context) []provider.ToolDef {
	defs, _ := s.toolDefsWithCatalog(ctx)
	return defs
}

// toolDefsWithCatalog is toolDefs plus the stage-1 MCP catalog segment that
// belongs in the same request's system prompt (see mcp_lazy.go). The two
// come from ONE plan, and therefore from one MCPRegistry.Tools call, because
// that call is what triggers a server's first connect attempt: computing
// them separately would dial twice and could disagree if a background retry
// committed between the two reads.
//
// The catalog is "" whenever nothing is deferred, which includes every
// session that did not opt into deferral at all.
func (s *Session) toolDefsWithCatalog(ctx context.Context) ([]provider.ToolDef, string) {
	defs := make([]provider.ToolDef, 0, len(s.tools))
	for _, t := range s.tools {
		defs = append(defs, t.Def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	plan := s.planMCPTools(ctx)
	defs = append(defs, plan.defs...)
	if s.cfg.Hooks != nil {
		for _, d := range s.cfg.Hooks.Tools() {
			defs = append(defs, provider.ToolDef{
				Name:        d.Name,
				Description: d.Description,
				InputSchema: d.InputSchema,
			})
		}
	}
	return defs, plan.catalog
}

// runToolCalls executes every tool call in an assistant message and returns
// the ToolResult parts, one per call, in CALL order. See toolexec.go for the
// executor itself (batch splitting, concurrency, per-key ordering, and the
// join where retention runs).
func (s *Session) runToolCalls(ctx context.Context, asst *message.Message) message.Parts {
	return s.runToolBatch(ctx, asst)
}

func (s *Session) runToolCall(ctx context.Context, tc *message.ToolCall) (message.Parts, bool) {
	s.emit(Event{Type: EventToolStart, ToolCall: tc})

	args := tc.Arguments
	if s.cfg.Hooks != nil {
		newArgs, deny := s.cfg.Hooks.ToolExecuteBefore(ctx, &plugin.ToolExecuteBeforeRequest{
			SessionID: s.ID, CallID: tc.CallID, Tool: tc.Name, Args: args,
		})
		if deny != "" {
			out := message.Parts{&message.Text{Text: deny}}
			s.emit(Event{Type: EventToolEnd, ToolCall: tc, Output: out, IsError: true})
			return out, true
		}
		if newArgs != nil {
			args = newArgs
		}
	}

	s.emitToolExecuteStart(tc.Name, tc.CallID)
	s.mu.Lock()
	s.toolExecCount++
	s.mu.Unlock()

	out, isErr := s.executeTool(ctx, tc, args)
	s.emitToolExecuteEnd(tc.Name, tc.CallID, !isErr)

	if s.cfg.Hooks != nil {
		out = s.cfg.Hooks.ToolExecuteAfter(ctx, &plugin.ToolExecuteAfterRequest{
			SessionID: s.ID, CallID: tc.CallID, Tool: tc.Name, Args: args, Output: out,
		})
	}
	s.emit(Event{Type: EventToolEnd, ToolCall: tc, Output: out, IsError: isErr})
	return out, isErr
}

func (s *Session) executeTool(ctx context.Context, tc *message.ToolCall, args json.RawMessage) (message.Parts, bool) {
	if t, ok := s.tools[tc.Name]; ok {
		out, err := t.Run(ctx, s, args)
		if err != nil {
			return message.Parts{&message.Text{Text: err.Error()}}, true
		}
		return out, false
	}
	if s.cfg.MCP != nil && isMCPToolName(tc.Name) {
		out, isErr, err := s.cfg.MCP.CallTool(ctx, tc.Name, args)
		if err != nil {
			return message.Parts{&message.Text{Text: err.Error()}}, true
		}
		// Use implies selection (see mcp_lazy.go): a call that ROUTED names
		// a real tool, so recording it keeps that tool's schema loaded when
		// an auto flip later defers its server. Without this, a tool the
		// model has been calling directly -- an eager server's tool needs no
		// select, and the model is told not to select a loaded one -- would
		// lose its definition mid-task the moment a second server connected
		// and pushed the catalog over the threshold.
		//
		// A nil err is the routed signal: CallTool resolves the binding
		// before it dials, so "unknown tool" and "server not configured"
		// both return an error above and record nothing. A tool-level
		// failure (isErr) DID route and is recorded, and so is a call whose
		// transport failed on a later attempt -- the next successful call
		// records it.
		//
		// The gate is per SERVER (see mcpToolUseImpliesSelection): a server
		// pinned eager can never flip, so a record for its tools could
		// never pay for itself. A plain eager config therefore records
		// nothing at all, and neither does a defer-capable session on a
		// call to one of its pinned-eager servers.
		if s.mcpToolUseImpliesSelection(tc.Name) {
			s.markMCPToolsSelected(tc.Name)
		}
		return out, isErr
	}
	if s.cfg.Hooks != nil {
		resp, err := s.cfg.Hooks.ExecuteTool(ctx, &plugin.ToolExecuteRequest{
			SessionID: s.ID, CallID: tc.CallID, Tool: tc.Name, Args: args,
		})
		if err != nil {
			return message.Parts{&message.Text{Text: err.Error()}}, true
		}
		return resp.Output, resp.IsError
	}
	return message.Parts{&message.Text{Text: fmt.Sprintf("unknown tool %q", tc.Name)}}, true
}

// MCPCall routes a plugin-initiated client/mcp.call request (explicit
// server + tool name, unnamespaced) through this session's configured MCP
// registry — the exact same connected clients a namespaced mcp__<server>__
// <tool> tool call would use (see executeTool). Returns an error when no
// MCP registry is configured at all; a configured-but-unreachable server
// surfaces as an ordinary error from the registry's CallServerTool.
func (s *Session) MCPCall(ctx context.Context, server, tool string, args json.RawMessage) (message.Parts, bool, error) {
	if s.cfg.MCP == nil {
		return nil, false, fmt.Errorf("engine: no MCP servers configured")
	}
	return s.cfg.MCP.CallServerTool(ctx, server, tool, args)
}

// shellEnv collects env additions from the shell.env hook chain.
func (s *Session) shellEnv(ctx context.Context, tool, command string) map[string]string {
	if s.cfg.Hooks == nil {
		return nil
	}
	return s.cfg.Hooks.ShellEnv(ctx, &plugin.ShellEnvRequest{
		SessionID: s.ID, Tool: tool, Command: command, Dir: s.cfg.WorkDir,
	})
}
