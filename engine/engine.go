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
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// Hooks is the slice of the plugin host the engine uses. *plugin.Host
// satisfies it; tests use fakes. A nil Hooks disables all hook dispatch.
type Hooks interface {
	ChatParams(ctx context.Context, req *plugin.ChatParamsRequest) plugin.ChatParams
	SystemTransform(ctx context.Context, req *plugin.SystemTransformRequest) []string
	ShellEnv(ctx context.Context, req *plugin.ShellEnvRequest) map[string]string
	ToolExecuteBefore(ctx context.Context, req *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string)
	ToolExecuteAfter(ctx context.Context, req *plugin.ToolExecuteAfterRequest) message.Parts
	ExecuteTool(ctx context.Context, req *plugin.ToolExecuteRequest) (*plugin.ToolExecuteResponse, error)
	Emit(events []plugin.Event)
	Tools() []plugin.ToolDef
}

// Tool is a built-in (in-process) tool.
type Tool struct {
	Def provider.ToolDef
	Run func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error)
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

// Config configures a Session.
type Config struct {
	Providers provider.Registry
	Model     message.ModelRef // initial model; swap any time with SetModel
	System    []string         // base system prompt segments
	MaxTokens int              // per-response cap; defaults to 8192
	WorkDir   string           // working directory for built-in tools

	// SessionDir is where session logs are persisted, one JSONL file per
	// session. Empty disables persistence entirely.
	SessionDir string

	// ParentSession is an opaque provenance pointer to the session this one
	// continues from — a re-dispatch after a failed goal, a follow-up fix
	// picked up on a fresh box. It is set once at creation (like WorkDir),
	// persisted on the session header record, and restored by LoadSession
	// (see store.go); it is NOT required to name a session that exists on
	// this server or in this process at all — lineage may cross boxes, so
	// the engine never validates or dereferences it. Empty means no
	// lineage. See Session.ParentSession.
	ParentSession string

	Hooks Hooks // optional plugin host
	// OnEvent is optional; called synchronously, keep it fast. The goal.*
	// events (see goal.go) are emitted while Session.mu is held so the event
	// stream can never invert relative to the journaled log order under a
	// concurrent ClearGoal/evaluation race — so OnEvent must NEVER call back
	// into the Session that raised the event (Prompt, ClearGoal, ActiveGoal,
	// etc.), which would deadlock on that same mutex.
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
	OnRequest func(turn int, req *provider.Request)

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

	// ContextWindowTokens is the model's context window size, in tokens.
	// Zero (the default, a fresh Config's zero value) disables automatic
	// compaction entirely: the engine has no built-in per-model table, so
	// this is opt-in. When positive, Prompt checks LastUsage against
	// ContextWindowTokens * CompactionThreshold at the top of every call
	// and compacts first if over. See docs/design/context-compaction.md.
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

	// enqueueSeq is the durable-enqueue idempotency high-water mark (see
	// EnqueuePromptDurable in queue.go and promptRecord.Seq in store.go):
	// the largest caller-issued seq durably accepted. Monotonic; a seq at or
	// below it is a duplicate no-op. Rebuilt on replay by LoadSession.
	enqueueSeq int64
}

// NewSession creates a session. Nothing touches the network, spawns
// processes, or writes to disk here — provider auth and plugin spawns happen
// on first use, and the session log is created on first message append.
func NewSession(cfg Config) *Session {
	s := newSession(cfg)
	s.ID = newID("ses")
	return s
}

// newSession builds a session minus its ID; NewSession and LoadSession
// share it.
func newSession(cfg Config) *Session {
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 8192
	}
	if cfg.BashTimeout <= 0 {
		cfg.BashTimeout = 2 * time.Minute
	}
	s := &Session{
		cfg:               cfg,
		model:             cfg.Model,
		tools:             make(map[string]Tool),
		createdAt:         time.Now().UTC(),
		promptQueueNextID: 1,
	}
	for _, t := range []Tool{bashTool(cfg.BashTimeout, cfg.BashOutputCap), readFileTool(), writeFileTool(), editFileTool(), sessionInfoTool()} {
		s.tools[t.Def.Name] = t
	}
	if cfg.Processes != nil {
		s.tools[processToolName] = processTool(cfg.Processes)
	}
	if cfg.GoalTool {
		s.tools[goalToolName] = goalTool()
	}
	if mcpConfiguredCount(cfg.MCP) > 0 {
		s.tools[mcpSessionToolName] = mcpTool()
	}
	for _, t := range cfg.Tools {
		s.tools[t.Def.Name] = t
	}
	return s
}

// SetModel swaps the model for subsequent requests. History transcodes
// automatically; there is no migration step.
func (s *Session) SetModel(ref message.ModelRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref == s.model {
		return
	}
	s.model = ref
	s.persistModel(ref)
}

// Model returns the session's current model.
func (s *Session) Model() message.ModelRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
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
	// Normalize before this message enters history at all: see
	// message.Message.Normalize's doc comment. This is the single ingest
	// choke point every message passes through (user, assistant, and tool
	// messages alike), so it is the right place to scrub a
	// present-but-zero-length Reasoning.ProviderData entry before it can
	// diverge an in-memory history from what LoadSession would reload.
	m.Normalize()
	if m.CreatedAt.IsZero() {
		// Every production provider adapter (anthropic, openaicompat, ...)
		// already stamps CreatedAt on the assistant message it assembles;
		// this is a backstop for any caller (a bare Tool, a test fixture, a
		// future adapter) that doesn't, so LastActivityAt (see engine.go)
		// never silently reports zero for an otherwise-ordinary message.
		m.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	s.history = append(s.history, m)
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
// It returns the final assistant message.
func (s *Session) Prompt(ctx context.Context, text string) (*message.Message, error) {
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
	})
	s.emitStatus("busy")
	defer s.emitStatus("idle")

	for {
		asst, stop, usage, err := s.streamTurn(ctx)
		if err != nil {
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
		s.appendWithUsage(*asst, &usage)
		s.emit(Event{Type: EventMessage, Message: asst, StopReason: stop, Usage: &usage})

		if stop != provider.StopToolUse {
			return asst, nil
		}
		results := s.runToolCalls(ctx, asst)
		if len(results) == 0 {
			// tool_use stop with no tool calls: treat as end of turn
			// rather than looping forever.
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
	params := plugin.ChatParams{Model: s.Model()}
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
	if s.cfg.Hooks != nil {
		params = s.cfg.Hooks.ChatParams(ctx, &plugin.ChatParamsRequest{SessionID: s.ID, Params: params})
		if params.Model.IsZero() {
			params.Model = s.Model()
		}
		system = append(system, s.cfg.Hooks.SystemTransform(ctx, &plugin.SystemTransformRequest{
			SessionID: s.ID,
			Model:     params.Model,
		})...)
	}

	prov, err := s.cfg.Providers.For(params.Model)
	if err != nil {
		return nil, "", provider.Usage{}, err
	}

	maxTokens := s.cfg.MaxTokens
	if params.MaxTokens != nil {
		maxTokens = *params.MaxTokens
	}
	// Ambient process-status, MCP-status, and parked-goal-status injection
	// (see processStatusSegment, mcpStatusSegment, goalParkedSegment):
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
	// toolDefs is computed FIRST, before either segment, and reused below
	// as req.Tools rather than recomputed: for MCP, toolDefs ->
	// s.cfg.MCP.Tools(ctx) is what actually TRIGGERS a server's first
	// connect attempt (see MCPManager.ensureConnected). Computing
	// mcpStatusSegment before that trigger would read Status() against
	// stale pre-connect state, silently delaying THIS turn's own connect
	// failure from surfacing until the NEXT turn — ordering toolDefs
	// first means a server that fails its first attempt on turn 1 is
	// reported in turn 1's own request, not turn 2's.
	tools := s.toolDefs(ctx)
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
	req := &provider.Request{
		Model:       params.Model,
		System:      system,
		Messages:    messages,
		Tools:       tools,
		Temperature: params.Temperature,
		TopP:        params.TopP,
		MaxTokens:   maxTokens,
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
		s.cfg.OnRequest(turn, req)
	}

	stream, err := prov.Stream(ctx, req)
	if err != nil {
		return nil, "", provider.Usage{}, err
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
	var results message.Parts
	for _, p := range partial.Parts {
		tc, ok := p.(*message.ToolCall)
		if !ok {
			continue
		}
		results = append(results, &message.ToolResult{
			CallID:  tc.CallID,
			Content: message.Parts{&message.Text{Text: interruptedTurnErrorText}},
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

// toolDefs merges built-in tools, MCP-provided tools, and plugin-provided
// ones.
func (s *Session) toolDefs(ctx context.Context) []provider.ToolDef {
	var defs []provider.ToolDef
	for _, t := range s.tools {
		defs = append(defs, t.Def)
	}
	if s.cfg.MCP != nil {
		defs = append(defs, s.cfg.MCP.Tools(ctx)...)
	}
	if s.cfg.Hooks != nil {
		for _, d := range s.cfg.Hooks.Tools() {
			defs = append(defs, provider.ToolDef{
				Name:        d.Name,
				Description: d.Description,
				InputSchema: d.InputSchema,
			})
		}
	}
	return defs
}

// runToolCalls executes every tool call in an assistant message, in order,
// and returns the ToolResult parts.
func (s *Session) runToolCalls(ctx context.Context, asst *message.Message) message.Parts {
	var results message.Parts
	for _, p := range asst.Parts {
		tc, ok := p.(*message.ToolCall)
		if !ok {
			continue
		}
		out, isErr := s.runToolCall(ctx, tc)
		results = append(results, &message.ToolResult{
			CallID:  tc.CallID,
			Content: out,
			IsError: isErr,
		})
	}
	return results
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
