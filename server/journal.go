package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// Event is one server-sent event, matching the openapi Event schema. Durable
// records carry a non-zero Seq and are journaled and replayable; live events
// have no Seq and stream only while connected.
type Event struct {
	Type      string           `json:"type"`
	SessionID string           `json:"session_id"`
	Seq       int64            `json:"seq,omitempty"`
	Status    string           `json:"status,omitempty"`
	Message   *message.Message `json:"message,omitempty"`
	Model     message.ModelRef `json:"model,omitzero"`
	// Effort is a *message.Effort, not a bare message.Effort with omitempty,
	// for the same reason QueueLen below is a *int: an "effort" record must
	// tell "cleared to the provider default" (EffortUnset, an explicit
	// "effort":"") apart from "this event type never carries an effort level"
	// (key absent, every other record). A bare string with omitempty cannot —
	// encoding/json drops an empty string under omitempty regardless of intent.
	// So Publish's EventEffortChanged case ALWAYS sets it (even on a clear),
	// and a nil pointer is omitted on every other record. This mirrors the
	// "model" record, which never clears to empty.
	Effort *message.Effort `json:"effort,omitempty"`
	// ServiceTier is a *string, not a bare string with omitempty, mirroring
	// Effort immediately above for the identical reason: a "service_tier"
	// record must tell "cleared to the provider default" (an explicit
	// "service_tier":"") apart from "this event type never carries a
	// service-tier value" (key absent, every other record). Publish's
	// EventServiceTierChanged case ALWAYS sets it (even on a clear), and a
	// nil pointer is omitted on every other record type.
	ServiceTier *string           `json:"service_tier,omitempty"`
	Text        string            `json:"text,omitempty"`
	ToolCall    *message.ToolCall `json:"tool_call,omitempty"`
	Output      message.Parts     `json:"output,omitempty"`
	IsError     bool              `json:"is_error,omitempty"`
	Error       string            `json:"error,omitempty"`

	// request.meta fields: a durable, replayable record of the assembled model
	// request. SystemHash fingerprints the joined system segments; the full
	// System array is included only when the hash differs from the session's
	// previous request (the hash-on-change trick), so unchanged prompts stay
	// cheap. SystemLen is the byte length of the joined system, Segments the
	// count, Tools the offered tool names, and Messages the history length.
	SystemHash string   `json:"system_hash,omitempty"`
	SystemLen  int      `json:"system_len,omitempty"`
	Segments   int      `json:"segments,omitempty"`
	Tools      []string `json:"tools,omitempty"`
	Messages   int      `json:"messages,omitempty"`
	System     []string `json:"system,omitempty"`

	// Goal-loop fields, carried by the goal.* durable records (see goal.go).
	GoalCondition string `json:"goal_condition,omitempty"`
	GoalReason    string `json:"goal_reason,omitempty"`
	GoalMet       bool   `json:"goal_met,omitempty"`
	GoalTurn      int    `json:"goal_turn,omitempty"`
	GoalTurns     int    `json:"goal_turns,omitempty"`
	GoalAttempt   int    `json:"goal_attempt,omitempty"`
	// GoalEvalFailures is carried by goal.eval_failed only (see
	// engine/goal.go's "Round 6" doc section): the number of
	// CONSECUTIVE failed evaluator boundaries as of this record, inclusive.
	// goal.cleared itself never carries a count (even the terminal clear
	// that fires once this reaches goalEvalFailureLimit — its dedicated
	// GoalReason text names the limit instead); the tracker's folded
	// eval_failures resets to 0 on goal.eval/goal.achieved/goal.cleared/
	// goal.updated, mirroring GoalSummary.eval_failures. See
	// foldGoalRecordLocked/publishGoal.
	GoalEvalFailures int `json:"goal_eval_failures,omitempty"`
	// GoalRetryable/GoalRetryableClass/GoalWaiting are carried by
	// goal.stalled (see engine/goal.go and GitHub issue #61): GoalRetryable
	// is true when the failure was classified provider-retryable weather;
	// GoalRetryableClass names the classification (overloaded/rate_limited/
	// server_error); GoalWaiting is true while still within the retryable
	// budget and false on the final stalled record that reports the budget
	// exhausted (the goal loop is about to park a turn, not die).
	GoalRetryable      bool   `json:"goal_retryable,omitempty"`
	GoalRetryableClass string `json:"goal_retryable_class,omitempty"`
	GoalWaiting        bool   `json:"goal_waiting,omitempty"`
	// GoalPaused/GoalPauseReason are the "paused" presentation (see
	// GoalSummary.paused): GoalPaused is true when a goal is nominally
	// active but nothing is currently driving it, and GoalPauseReason names
	// why — "restart" (goal.paused: this server booted and found the goal
	// armed with no loop attached, see pauseArmedGoalsAtBoot) or
	// "provider-backoff" (goal.stalled: the retryable-backoff park machinery
	// in engine/goal.go is waiting out provider weather, mirrored from
	// GoalRetryable && GoalWaiting — no engine behavior changes, only this
	// observability). Carried on goal.paused (always true) and goal.stalled
	// (only while GoalRetryable && GoalWaiting) records/events.
	GoalPaused      bool   `json:"goal_paused,omitempty"`
	GoalPauseReason string `json:"goal_pause_reason,omitempty"`

	// Outcome carries the turn.end record's result: "completed" or "error".
	// Error (above) carries the sanitized failure detail when Outcome is
	// "error", empty on a clean completion. See runPrompt/runGoal's
	// recordTurnEnd.
	Outcome string `json:"outcome,omitempty"`

	// WorktreePath carries the workdir.worktree_kept / workdir.worktree_removed
	// records' worktree directory (see teardownWorktree and sweepWorktrees):
	// the path a 'worktree'-isolation session's tools ran in. Present only on
	// those two event types.
	WorktreePath string `json:"worktree_path,omitempty"`

	// Compaction fields (see docs/design/context-compaction.md §4 "Live
	// event surface"). Carried on the durable evtHistoryCompacted record:
	// CompactFirstID/CompactLastID name the folded range's boundary
	// messages, CompactTurnsFolded is the fold count, and
	// CompactSummaryID names the summary message — already delivered via a
	// preceding evtMessage record (see Publish/publishHistoryCompacted).
	// evtCompactionFailed (live only, never journaled) carries only Error.
	// evtCompactionStarted (also live only, never journaled) fires before
	// evtHistoryCompacted/evtCompactionFailed and carries the same
	// CompactFirstID/CompactLastID/CompactTurnsFolded, but never
	// CompactSummaryID — the summary does not exist yet when it fires.
	CompactFirstID     string `json:"compact_first_id,omitempty"`
	CompactLastID      string `json:"compact_last_id,omitempty"`
	CompactTurnsFolded int    `json:"compact_turns_folded,omitempty"`
	CompactSummaryID   string `json:"compact_summary_id,omitempty"`

	// Prompt-queue fields, carried by the prompt.queued/prompt.dequeued
	// durable records (see engine/queue.go and docs/plans/2026-07-19-prompt-
	// queue.md). QueueID is the queue-assigned, session-monotonic prompt ID.
	// QueueText is the queued prompt text, carried on both events. QueueReason
	// is empty on prompt.queued and one of "delivered" (idle drain),
	// "injected" (goal-turn-boundary injection), or "cleared" (DELETE
	// /session/{id}/queue) on prompt.dequeued. QueueLen is the queue depth
	// remaining immediately after this event.
	//
	// QueueLen is a *int, not a plain int, unlike this struct's other
	// event-scoped optional numeric fields (SystemLen, GoalTurn, etc., which
	// use a bare int with omitempty): those fields never carry a genuinely
	// meaningful zero — e.g. GoalTurn is 1-based and never legitimately 0 on
	// the events that set it — so an absent key and a present zero can never
	// be confused. QueueLen is different: a prompt.dequeued that drains the
	// queue's last entry legitimately reports 0, and a consumer must be able
	// to tell "the queue is now empty" (explicit 0) apart from "this event
	// type never carries a queue depth" (key absent, every other event type).
	// A bare int with omitempty cannot express that distinction — Go's
	// encoding/json omits a zero int under omitempty regardless of intent —
	// so publishQueue (the only writer of this field) always populates a
	// non-nil pointer, giving prompt.queued/prompt.dequeued an unambiguous
	// wire value while every other event type still omits the key entirely
	// (nil pointer + omitempty). This mirrors the same nil-vs-zero idiom
	// provider.Request already uses for Temperature/TopP (provider/
	// provider.go), the codebase's precedent for "zero is a meaningful,
	// distinct-from-absent value."
	QueueID     int64  `json:"queue_id,omitempty"`
	QueueText   string `json:"queue_text,omitempty"`
	QueueReason string `json:"queue_reason,omitempty"`
	QueueLen    *int   `json:"queue_len,omitempty"`
	// QueueSeq mirrors engine.Event.QueueSeq: the caller-issued idempotency
	// sequence carried on a prompt.queued record from a durable enqueue
	// (POST /session/{id}/enqueue, engine.Session.EnqueuePromptDurable) —
	// see docs/plans/2026-07-21-durable-enqueue.md. 0/omitted on a plain
	// enqueue (prompt_async) and on every prompt.dequeued.
	QueueSeq int64 `json:"queue_seq,omitempty"`
}

// Durable and live event types (a superset of engine.Event types plus the
// server-owned lifecycle records).
const (
	evtSessionCreated = "session.created"
	evtSessionStatus  = "session.status"
	evtSessionError   = "session.error"
	evtSessionAborted = "session.aborted"
	evtTurnEnd        = "turn.end"
	evtMessage        = "message"
	evtModel          = "model"
	evtEffort         = "effort"
	// evtServiceTier mirrors evtEffort: the durable observability record
	// journaled for every SetServiceTier swap (see the
	// engine.EventServiceTierChanged case in Publish below).
	evtServiceTier  = "service_tier"
	evtRequestMeta  = "request.meta"
	evtGoalSet      = "goal.set"
	evtGoalUpdated  = "goal.updated"
	evtGoalEval     = "goal.eval"
	evtGoalStalled  = "goal.stalled"
	evtGoalAchieved = "goal.achieved"
	evtGoalCleared  = "goal.cleared"
	// evtGoalEvalFailed mirrors engine.EventGoalEvalFailed (see
	// engine/goal.go's "Round 6" doc section): journaled once per
	// failed evaluator boundary — a provider error the retryable-class
	// in-boundary retry couldn't ride out, or two consecutive unparseable
	// replies. Below goalEvalFailureLimit consecutive failures this is
	// advisory only (the goal stays active); at the limit a goal.cleared
	// with a dedicated reason follows instead, and the server maps the
	// terminal error to the turn.end outcome outcomeEvaluatorExhausted.
	evtGoalEvalFailed = "goal.eval_failed"
	// evtGoalPaused is journaled once per boot for every session whose
	// journal shows an active goal but which has no running loop attached
	// in this process (see pauseArmedGoalsAtBoot) — the durable, honest
	// record that this server found the goal armed-but-unattended, distinct
	// from goal.stalled (which is a live worker-turn retry event, not a
	// boot-time observation). Always carries GoalPauseReason "restart".
	evtGoalPaused = "goal.paused"
	// evtGoalParked mirrors engine.EventGoalParked (see engine/goal.go's
	// "Round 7" doc section): journaled once per exit-parked
	// worker turn — either exhaustion tier (deterministic goalWorkerRetries
	// or retryable-class goalRetryableMaxAttempts) — WITHOUT a following
	// goal.cleared: the goal stays active. Unlike evtGoalPaused above (a
	// boot-time OBSERVATION that no loop is attached), this is a LIVE event:
	// the loop that just parked emitted it on its own way out. The server
	// maps it onto the third "paused" arm (pause_reason "worker_failure",
	// see pauseReasonWorkerFailure) and, at runGoal's tail, onto the
	// turn.end outcome outcomeWorkerParked — the loop resumes on the next
	// ordinary activity via the existing activity-driven auto-arm
	// (maybeAutoArmGoal), exactly like a restart pause resumes via an
	// operator's re-POST.
	evtGoalParked = "goal.parked"

	// evtPromptDequeued mirrors engine.EventPromptDequeued (see
	// engine/queue.go): a queued prompt was popped off the head for
	// delivery (idle drain, goal-turn injection, or a durable clear). See
	// publishQueue, which journals the enqueue side too but does so by
	// forwarding ev.Type (engine.EventPromptQueued) directly — there is no
	// server-local mirror constant for it, since nothing in this package
	// ever needs to name that string apart from the engine's own constant.
	evtPromptDequeued = "prompt.dequeued"

	// evtWorktreeKept is journaled whenever a 'worktree'-isolation session's
	// worktree is left in place at teardown (session end or the serve-start
	// sweep) because it has uncommitted changes or unpushed commits — the
	// durable record of WorktreePath so an orchestrator can find and finish
	// the work rather than lose it silently. evtWorktreeRemoved is journaled
	// on the ordinary clean-teardown path, purely for observability.
	evtWorktreeKept    = "workdir.worktree_kept"
	evtWorktreeRemoved = "workdir.worktree_removed"

	// evtHistoryCompacted is the durable record of a successful compaction
	// (see engine/compact.go's Session.Compact and docs/design/context-
	// compaction.md §4): journaled after the summary message itself has
	// already flowed through an evtMessage record, via Publish's routing of
	// engine.EventHistoryCompacted — the "reconciliation signal" telling a
	// replaying tailer which prefix the just-seen summary message replaced.
	evtHistoryCompacted = "history.compacted"
	// evtCompactionFailed is compaction's fire-and-forget failure
	// counterpart — live only, never journaled (a failed compaction never
	// mutates durable state, so there is nothing to reconcile on replay).
	evtCompactionFailed = "compaction.failed"
	// evtCompactionStarted mirrors engine.EventCompactionStarted: fired
	// once, immediately before Compact's blocking summarization call
	// begins, so a live client can show a "compacting now" indicator — see
	// that constant's doc comment for why it is always followed by exactly
	// one of evtHistoryCompacted or evtCompactionFailed. Live only, like
	// evtCompactionFailed above — never journaled, since a "started" that
	// never resolves has nothing durable to reconcile on replay.
	evtCompactionStarted = "compaction.started"
)

const journalName = "events.jsonl"

// outcomeMaxTurnsExceeded is the turn.end outcome recorded when a goal loop
// exhausts GoalOptions.MaxTurns without the evaluator ever returning MET
// (engine/goal.go's PursueGoal returns GoalResult{Achieved:false, Reason:
// "max turns"}, a NIL error). It is deliberately distinct from "completed":
// a goal that gave up after burning its turn budget is not "done", and
// surfacing it as completed would recreate the exact "idle because done vs.
// idle because it died/gave up" ambiguity this primitive exists to remove.
// See runGoal.
const outcomeMaxTurnsExceeded = "max_turns_exceeded"

// outcomeContextExhausted is the turn.end outcome recorded when a prompt or
// goal-worker turn fails on a classified provider.ErrKindContextOverflow
// error (issue #62): the request as built cannot fit the model's context
// window. Distinct from the generic "error" outcome so a poller can react
// to it specifically (e.g. rotate the session before the next attempt
// hits the identical cliff) without string-matching last_turn.error — the
// same reasoning outcomeMaxTurnsExceeded above already establishes for its
// own non-generic terminal case. It is deterministic (retrying fails
// identically), unlike an ordinary "error", which may or may not be — see
// engine/goal.go's promptTurnWithRetry, which fails fast on this
// classification rather than retrying.
const outcomeContextExhausted = "context_exhausted"

// outcomeEvaluatorExhausted is the turn.end outcome recorded when a goal
// loop's evaluator has failed at goalEvalFailureLimit consecutive turn
// boundaries (engine/goal.go's "Round 6" doc section): a durable,
// probably-permanent evaluator outage. Distinct from the generic "error" for
// the same reason outcomeContextExhausted and outcomeMaxTurnsExceeded are —
// a poller reacting to "the evaluator itself is broken" (e.g. surfacing an
// operator alert rather than just retrying the goal) needs to tell this
// apart from an ordinary worker-turn failure without string-matching
// last_turn.error or GoalReason. Unlike every failed boundary below the
// limit (which is advisory only — no turn.end at all, the loop just keeps
// running), this terminal always clears the goal and always emits
// session.error alongside this outcome — see runGoal's default branch.
const outcomeEvaluatorExhausted = "evaluator_exhausted"

// outcomeWorkerParked is the turn.end outcome recorded when a goal loop
// exit-parks a worker turn instead of clearing the goal (engine/goal.go's
// "Round 7" doc section): either exhaustion tier — deterministic
// (goalWorkerRetries) or retryable-class (goalRetryableMaxAttempts) —
// without the evaluator ever running. Distinct from the generic "error" for
// the same reason outcomeContextExhausted/outcomeMaxTurnsExceeded/
// outcomeEvaluatorExhausted are: a poller needs to tell "this goal is
// merely paused, waiting for the next ordinary activity to resume it" apart
// from an operator-facing dead terminal. UNLIKE outcomeEvaluatorExhausted,
// this outcome does NOT mean the goal was cleared — it stays fully active
// (see goalTracker.pausedWorker/pauseView) — so a poller must not treat it
// as a reason to give up on the goal, only as a reason to expect it to
// resume on its own the next time this session sees any activity.
const outcomeWorkerParked = "worker_parked"

// turnEndOutcome decides the turn.end outcome for a non-nil, non-cancelled
// prompt/goal-worker error: outcomeEvaluatorExhausted when the engine's
// PursueGoal returned its evaluator-exhausted terminal sentinel (see
// engine.IsGoalEvaluatorExhausted), outcomeWorkerParked when it returned its
// worker-parked sentinel (engine.IsGoalWorkerParked), outcomeContextExhausted
// when it classified the error via provider.IsContextOverflow, otherwise the
// generic "error" every other failure has always recorded. Shared by
// runPrompt and runGoal so the two turn-ending paths can never drift on
// this — runPrompt can never actually observe either goal-loop sentinel (it
// has no evaluator and no worker-retry budget), but sharing one function
// keeps the two paths from drifting on the ordering/precedence of these
// classifications as new ones are added. The three sentinel/classification
// checks are mutually exclusive by construction (each wraps a distinct
// engine terminal), so their relative order here does not matter.
func turnEndOutcome(err error) string {
	if engine.IsGoalEvaluatorExhausted(err) {
		return outcomeEvaluatorExhausted
	}
	if engine.IsGoalWorkerParked(err) {
		return outcomeWorkerParked
	}
	if provider.IsContextOverflow(err) {
		return outcomeContextExhausted
	}
	return "error"
}

// Publish maps an engine event onto the journal/SSE stream. Message events
// trigger a journal sync (which durably records every new canonical message,
// including the user and tool messages the engine does not surface via
// OnEvent); the live deltas fan out to connected clients only.
func (s *Server) Publish(ev engine.Event) {
	switch ev.Type {
	case engine.EventMessage:
		s.syncMessages(ev.SessionID)
	case engine.EventTextDelta:
		s.publishLive(Event{Type: engine.EventTextDelta, SessionID: ev.SessionID, Text: ev.Text})
	case engine.EventReasoningDelta:
		s.publishLive(Event{Type: engine.EventReasoningDelta, SessionID: ev.SessionID, Text: ev.Text})
	case engine.EventTurnRestart:
		// A base-loop retry (engine/prompt_retry.go) is about to re-stream a
		// turn whose partial text/reasoning deltas already reached this
		// subscriber. Forward the marker live so an SSE client drops the
		// stale partial before the retry's deltas arrive — see
		// engine.EventTurnRestart. Live only (Seq 0); it is never journaled.
		s.publishLive(Event{Type: engine.EventTurnRestart, SessionID: ev.SessionID})
	case engine.EventToolStart:
		s.publishLive(Event{Type: engine.EventToolStart, SessionID: ev.SessionID, ToolCall: ev.ToolCall})
	case engine.EventToolEnd:
		s.publishLive(Event{
			Type: engine.EventToolEnd, SessionID: ev.SessionID,
			ToolCall: ev.ToolCall, Output: ev.Output, IsError: ev.IsError,
		})
	case engine.EventModelChanged:
		// Every model swap — the `model` tool, a per-request prompt override,
		// or POST /session/{id}/model — funnels through SetModel's single
		// EventModelChanged emit, so this ONE case journals the durable
		// "model" observability record for all three. The handlers no longer
		// emit evtModel themselves (see handlePrompt), so a swap journals
		// exactly once, never twice.
		s.emitDurable(Event{Type: evtModel, SessionID: ev.SessionID, Model: ev.Model})
	case engine.EventEffortChanged:
		// Every effort swap funnels through SetEffort's single
		// EventEffortChanged emit, so this ONE case journals the durable
		// "effort" observability record — the same single-path shape the
		// EventModelChanged case above uses. Effort is set explicitly (a
		// pointer) even on a clear to EffortUnset, so the record always
		// carries the "effort" key — see the Event.Effort field comment.
		eff := ev.Effort
		s.emitDurable(Event{Type: evtEffort, SessionID: ev.SessionID, Effort: &eff})
	case engine.EventServiceTierChanged:
		// Every service-tier swap funnels through SetServiceTier's single
		// EventServiceTierChanged emit, so this ONE case journals the
		// durable "service_tier" observability record — the same
		// single-path shape the EventEffortChanged case above uses.
		// ServiceTier is set explicitly (a pointer) even on a clear to "",
		// so the record always carries the "service_tier" key — see the
		// Event.ServiceTier field comment.
		tier := ev.ServiceTier
		s.emitDurable(Event{Type: evtServiceTier, SessionID: ev.SessionID, ServiceTier: &tier})
	case engine.EventGoalSet, engine.EventGoalUpdated, engine.EventGoalEval, engine.EventGoalStalled, engine.EventGoalAchieved, engine.EventGoalCleared, engine.EventGoalEvalFailed, engine.EventGoalParked:
		s.publishGoal(ev)
	case engine.EventPromptQueued, engine.EventPromptDequeued:
		s.publishQueue(ev)
	case engine.EventHistoryCompacted:
		s.publishHistoryCompacted(ev)
	case engine.EventCompactionFailed:
		s.publishLive(Event{Type: evtCompactionFailed, SessionID: ev.SessionID, Error: ev.Text})
	case engine.EventCompactionStarted:
		// Live only, like evtCompactionFailed above — see
		// engine.EventCompactionStarted's doc comment. Carries the same
		// fold-range fields the paired evtHistoryCompacted/
		// evtCompactionFailed will carry, minus CompactSummaryID (the
		// summary does not exist yet).
		s.publishLive(Event{
			Type:               evtCompactionStarted,
			SessionID:          ev.SessionID,
			CompactFirstID:     ev.CompactFirstID,
			CompactLastID:      ev.CompactLastID,
			CompactTurnsFolded: ev.CompactTurnsFolded,
		})
	}
}

// publishHistoryCompacted journals the durable history.compacted record for
// a successful compaction (see engine/compact.go's Session.Compact). It is
// called synchronously from within Session.Compact's own emit sequence —
// AFTER the summary message's EventMessage has already been published (see
// Publish's engine.EventMessage case, which journals via syncMessages) — so
// the journal order is always summary message, then history.compacted,
// exactly as docs/design/context-compaction.md §4 requires.
func (s *Server) publishHistoryCompacted(ev engine.Event) {
	s.emitDurable(Event{
		Type:               evtHistoryCompacted,
		SessionID:          ev.SessionID,
		CompactFirstID:     ev.CompactFirstID,
		CompactLastID:      ev.CompactLastID,
		CompactTurnsFolded: ev.CompactTurnsFolded,
		CompactSummaryID:   ev.CompactSummaryID,
	})
}

// publishGoal journals a durable goal.* record and folds the event into the
// per-session goal tracker that backs the Session JSON goal field.
//
// goal.stalled is non-terminal (see engine/goal.go's state machine: STALLED
// is a transient sub-state a worker-turn retry passes through on its way
// back to ACTIVE or on to CLEARED) — it updates lastReason and attempt only,
// leaving active/achieved untouched, so a client watching Session.goal sees
// the goal still active while the loop retries.
//
// goal.parked is also non-terminal in the same "goal stays
// active" sense, but unlike goal.stalled it means the loop has actually
// EXITED — no further goal.stalled/goal.eval will arrive until something
// external (the activity-driven auto-arm, or an operator's re-POST) starts
// a fresh loop. See the pausedWorker fold below and pauseView's precedence.
func (s *Server) publishGoal(ev engine.Event) {
	out := &Event{
		Type:               ev.Type,
		SessionID:          ev.SessionID,
		GoalCondition:      ev.GoalCondition,
		GoalReason:         ev.GoalReason,
		GoalMet:            ev.GoalMet,
		GoalTurn:           ev.GoalTurn,
		GoalTurns:          ev.GoalTurns,
		GoalAttempt:        ev.GoalAttempt,
		GoalRetryable:      ev.GoalRetryable,
		GoalRetryableClass: ev.GoalRetryableClass,
		GoalWaiting:        ev.GoalWaiting,
		GoalEvalFailures:   ev.GoalEvalFailures,
	}
	// goal.stalled carries the provider-backoff pause presentation
	// (deliverable 2(b)): a retryable-class stall still within its backoff
	// budget IS the park machinery waiting out provider weather — pure
	// observability over engine/goal.go's existing GoalRetryable/GoalWaiting
	// fields, no behavior change. See goalTracker.pauseView, which derives
	// the same thing for Session JSON from the folded state below.
	if ev.Type == engine.EventGoalStalled && ev.GoalRetryable && ev.GoalWaiting {
		out.GoalPaused = true
		out.GoalPauseReason = pauseReasonProviderBackoff
	}
	// goal.parked carries the worker-failure pause presentation directly on
	// the live event too (Task 2), mirroring the goal.stalled case
	// above and the boot-only goal.paused record: an SSE watcher sees the
	// pause the instant the loop exits, without waiting for a GET /session
	// poll. GoalAttempt reuses the engine event's GoalAttempts (plural, the
	// TOTAL attempt count for the exhausted turn) — by construction this is
	// always the same number the final goal.stalled record for this turn
	// already reported (see engine/goal.go's recordGoalParked), so no
	// separate wire field is needed.
	if ev.Type == engine.EventGoalParked {
		out.GoalAttempt = ev.GoalAttempts
		out.GoalPaused = true
		out.GoalPauseReason = pauseReasonWorkerFailure
	}
	s.mu.Lock()
	g := s.goalState[ev.SessionID]
	if g == nil {
		g = &goalTracker{}
		s.goalState[ev.SessionID] = g
	}
	switch ev.Type {
	case engine.EventGoalSet:
		g.condition = ev.GoalCondition
		g.active = true
		g.achieved = false
		g.turns = 0
		g.lastReason = ""
		g.attempt = 0
		g.retryable = false
		g.retryableClass = ""
		g.waiting = false
		g.pausedRestart = false
		g.pausedWorker = false
		g.evalFailures = 0
	case engine.EventGoalUpdated:
		// A condition change mid-loop, nothing else: no pause/retry/waiting
		// reset — this must not fake a state transition (see
		// engine/goal.go's UpdateGoal and the plan's Task 4). eval_failures
		// IS reset here per the design (Task 2): the consecutive-failure
		// streak is measured against a condition, and UpdateGoal is exactly
		// the event that invalidates the evaluator calls that streak counted
		// (see engine/goal.go's generation-guard doc comment). pausedWorker
		// (Task 2) resets here too — an UpdateGoal against a
		// worker-parked goal is itself a re-arm (the loop is about to be
		// resumed with the new condition), so the stale park presentation
		// must not linger.
		g.condition = ev.GoalCondition
		g.evalFailures = 0
		g.pausedWorker = false
	case engine.EventGoalEval:
		g.turns = ev.GoalTurn
		g.lastReason = ev.GoalReason
		g.attempt = 0
		g.retryable = false
		g.retryableClass = ""
		g.waiting = false
		g.evalFailures = 0
		g.pausedWorker = false
	case engine.EventGoalStalled:
		g.lastReason = ev.GoalReason
		g.attempt = ev.GoalAttempt
		g.retryable = ev.GoalRetryable
		g.retryableClass = ev.GoalRetryableClass
		g.waiting = ev.GoalWaiting
	case engine.EventGoalEvalFailed:
		// See goalTracker.evalFailures' doc comment: folded straight from
		// the record's own count, which makes this idempotent on replay —
		// exactly like every other goal.* fold here — rather than an
		// increment this function would have to guard against double-
		// applying.
		g.evalFailures = ev.GoalEvalFailures
	case engine.EventGoalParked:
		// The worker-failure pause arm (Task 2): the loop just
		// exited without clearing the goal. lastReason/attempt/retryable/
		// retryableClass are folded from this record exactly like
		// goal.stalled's own fields (see the doc comment on GoalAttempt's
		// reuse above) — waiting is explicitly false, never re-derived from
		// the event (which does not set it): a park is never "still
		// waiting", it already gave up on this turn. See
		// goalTracker.pauseView's precedence (worker_failure over
		// provider-backoff) for why this alone is enough to stop a stale
		// GoalRetryable/GoalWaiting pair from misreporting provider-backoff.
		g.pausedWorker = true
		g.lastReason = ev.GoalReason
		g.attempt = ev.GoalAttempts
		g.retryable = ev.GoalRetryable
		g.retryableClass = ev.GoalRetryableClass
		g.waiting = false
	case engine.EventGoalAchieved:
		g.active = false
		g.achieved = true
		g.turns = ev.GoalTurns
		g.lastReason = ev.GoalReason
		g.attempt = 0
		g.retryable = false
		g.retryableClass = ""
		g.waiting = false
		g.pausedRestart = false
		g.pausedWorker = false
		g.evalFailures = 0
	case engine.EventGoalCleared:
		g.active = false
		g.pausedRestart = false
		g.pausedWorker = false
		g.evalFailures = 0
	}
	s.emitDurableLocked(out)
	s.mu.Unlock()

	// Structured goal-lifecycle logging (Options.Logger, see the field
	// report on its doc comment). Deliberately runs AFTER s.mu is released
	// above, not under it: emitDurableLocked's own journal write/fanout is
	// cheap and bounded, but Options.Logger is caller-supplied I/O (a file, a
	// pipe, a remote sink) with no such guarantee — a stalled stderr (full
	// pipe, suspended terminal) blocking a log write while holding s.mu would
	// wedge every other handler and SSE fanout on the server, not just this
	// session's. See emitDurable's matching fix for the session.error choke
	// point and its doc comment for why "log after unlock" is the rule for
	// every logging call site in this file, not just this one.
	//
	// goal.parked and goal.stalled are the background-loop analogue of a
	// plain turn dying silently — a parked goal has already exited and is
	// waiting on external activity to resume it, exactly the "operator
	// tailing the log concluded the box was dead" scenario, so both log at
	// WARN with the same turn/attempt/reason/retry-budget shape the
	// docs/goal-loop.md's structured-logging contract requires. The
	// set/achieved/cleared transitions are ordinary lifecycle transitions, not
	// failures, so INFO.
	//
	// goal.eval also logs at INFO, one line per completed worker turn (the
	// evaluator runs exactly once per turn — see engine/goal.go's
	// PursueGoal): this is the per-turn heartbeat a goal-supervised session
	// needs, since recordTurnEnd's own turn.end line fires only once per LOOP
	// EXIT for a goal loop (runGoal's tail), not once per worker turn — see
	// recordTurnEnd's doc comment. Without this, a long-running goal produced
	// no periodic log line at all between "goal set" and its eventual
	// terminal record, which is exactly backwards for the primary
	// long-running shape this server serves. goal.updated and
	// goal.eval_failed remain unlogged here: goal.updated is a mid-loop
	// condition edit with no lifecycle/failure signal of its own, and
	// goal.eval_failed's advisory-failure semantics (the goal stays active
	// below goalEvalFailureLimit) are already durably recorded via the
	// goal.eval_failed record itself even without a log line — revisit if a
	// field report asks for it.
	switch ev.Type {
	case engine.EventGoalParked:
		s.logWarn("goal parked", "session", ev.SessionID, "reason", ev.GoalReason,
			"attempts", ev.GoalAttempts, "retryable_class", ev.GoalRetryableClass)
	case engine.EventGoalStalled:
		s.logWarn("goal stalled", "session", ev.SessionID, "reason", ev.GoalReason,
			"attempt", ev.GoalAttempt, "waiting", ev.GoalWaiting)
	case engine.EventGoalAchieved:
		s.logInfo("goal achieved", "session", ev.SessionID, "reason", ev.GoalReason, "turns", ev.GoalTurns)
	case engine.EventGoalSet:
		s.logInfo("goal set", "session", ev.SessionID, "condition", ev.GoalCondition)
	case engine.EventGoalCleared:
		s.logInfo("goal cleared", "session", ev.SessionID, "reason", ev.GoalReason)
	case engine.EventGoalEval:
		s.logInfo("goal eval", "session", ev.SessionID, "met", ev.GoalMet, "reason", ev.GoalReason)
	}
}

// publishQueue journals a durable prompt.queued/prompt.dequeued record (see
// engine/queue.go). There is deliberately no per-session tracker folded
// here, unlike publishGoal: GET /session's queued count still reads
// engine.Session.QueuedPrompts() directly (see buildSession), which is
// authoritative for both a live resident session (its own promptQueue slice,
// mutex-guarded) and a freshly LoadSession-replayed one (folded from the same
// prompt.queued/dequeued records in the session's own log — see store.go).
// Deriving the count from the engine session itself rather than duplicating
// it into a server-side fold makes "live fold and boot replay agree" true by
// construction — there is only one source of truth to ever drift from,
// unlike goalState (which exists because Session JSON needs server-derived
// presentation, e.g. the paused view, that the engine does not itself track).
// GET /session/{id}/wait's until=idle condition does not read queue depth
// either (see Server.queueDrainPending's own doc comment for why not, and
// what it reads instead). This function's own job is unchanged: make the
// events visible on the durable journal/SSE stream for observability and
// replay.
func (s *Server) publishQueue(ev engine.Event) {
	queueLen := ev.QueueLen
	s.emitDurable(Event{
		Type:        ev.Type,
		SessionID:   ev.SessionID,
		QueueID:     ev.QueueID,
		QueueText:   ev.QueueText,
		QueueReason: ev.QueueReason,
		QueueLen:    &queueLen,
		QueueSeq:    ev.QueueSeq,
	})
}

// recordTurnEnd journals a durable turn.end record for sessionID and updates
// the in-memory lastTurn summary GET /session/{id} and /session/status read
// (see Server.lastTurn). outcome is "completed" or "error"; turnErr is the
// triggering error on failure (sanitized here via
// plugin.SanitizeSessionError — never credentials or request bodies — before
// it is journaled, streamed, or exposed), nil on a clean completion.
//
// This is the "idle because done" vs "idle because the turn died" wire
// contract: today, three plain-prompt turns died mid-stream (final assistant
// message reasoning-only, no text, no tool call) and every monitor had to
// infer death from message part shapes. turn.end plus Session.last_turn make
// that heuristic unnecessary — a poller reads the outcome directly instead
// of reverse-engineering it from transcript content.
//
// It also logs (Options.Logger, see the field report on its doc comment):
// INFO for outcome "completed", WARN for every other outcome — session and
// outcome always, error only when non-empty (a completed turn has none, so
// omitting it there keeps a clean run's line free of a stray empty attr).
// The log call runs AFTER s.mu is released above, never under it — see
// emitDurable's doc comment for why a logging call site must never run
// inside a locked section.
//
// This is NOT one log line per worker turn for a goal-supervised session,
// despite runPrompt's and runGoal's tails both calling it: runPrompt calls
// it once per plain-prompt turn (so for that shape it is exactly the "one
// line per turn" heartbeat), but runGoal calls it exactly once per goal LOOP
// EXIT — after PursueGoal itself returns, whether that took one worker turn
// or several hundred — recording the loop's own terminal outcome (achieved,
// cleared, parked, max-turns-exceeded, evaluator-exhausted, or a plain
// error). Since goal-supervised sessions are this server's primary
// long-running shape, the per-worker-turn heartbeat instead comes from
// publishGoal's own logging: "goal eval" at INFO (the evaluator runs exactly
// once per completed worker turn) and "goal stalled" at WARN (a worker-turn
// retry) — see publishGoal's doc comment. Usage/token counts are
// deliberately not threaded in here: they are not already available at this
// call site (only outcome and the sanitized error string are), and this
// fix's scope is "log what exists", not "thread new state through the
// engine for logging".
func (s *Server) recordTurnEnd(sessionID, outcome string, turnErr error) {
	errStr := ""
	if turnErr != nil {
		errStr = plugin.SanitizeSessionError(turnErr.Error())
	}
	s.mu.Lock()
	s.lastTurn[sessionID] = &turnOutcome{outcome: outcome, error: errStr}
	s.emitDurableLocked(&Event{Type: evtTurnEnd, SessionID: sessionID, Outcome: outcome, Error: errStr})
	s.mu.Unlock()

	if outcome == "completed" {
		s.logInfo("turn end", "session", sessionID, "outcome", outcome)
		return
	}
	attrs := []any{"session", sessionID, "outcome", outcome}
	if errStr != "" {
		attrs = append(attrs, "error", errStr)
	}
	s.logWarn("turn end", attrs...)
}

// onChildTurnEnd is engine.SessionManager's ChildTurnObserver — installed
// via SetChildTurnObserver in server.New, called once a CHILD's own
// Spawn/Send/SendOrQueue-driven turn settles (see that hook's own doc
// comment, engine/session_manager.go). It gives a child the SAME
// turn.end/session.error/session.aborted/session.status wire events
// runPrompt already emits for a root, mirroring runPrompt's own
// err/cancellation switch exactly (server/handlers.go) so a client
// watching a child's SSE stream sees the identical vocabulary it already
// gets for a root — a child previously emitted NONE of these, forcing a
// caller to poll session.info for busy-state instead.
//
// syncMessages runs first, exactly like runPrompt's own "catch any
// message not yet journaled" call: a child's Config.OnEvent is normally
// wired to this server's Publish (inherited from its parent's own
// Config, set at session construction — see Spawn's configSnapshot call)
// so its messages ordinarily already streamed to the journal as they
// were produced; this is the same harmless, idempotent backstop
// runPrompt's tail relies on for a root, not a new requirement.
//
// canceled reports session.aborted instead of turn.end, matching
// runPrompt's context.Canceled branch (which also skips recordTurnEnd
// entirely) — a child's Cancel-driven settle is the SAME "the caller
// asked for this to stop" shape a root's aborted turn is, not an
// ordinary completion or failure.
// onChildTurnStart is engine.SessionManager's ChildTurnStartObserver —
// installed via SetChildTurnStartObserver in server.New, called once a
// CHILD's own Spawn/Send/SendOrQueue-driven turn is ADMITTED to run
// (see that hook's own doc comment, engine/session_manager.go). It
// emits the EXACT SAME event this server's own root admission path
// already emits at the identical moment for a root — see, for one
// example among several identical call sites, session_tree.go's
// sendTextToRoot ("s.emitDurable(Event{Type: evtSessionStatus,
// SessionID: id, Status: "busy"})") — so a client watching a child's
// SSE stream sees the identical busy signal it already gets for a
// root, not just the settle-side turn.end/session.status(idle) pair
// onChildTurnEnd (below) already provides. A child previously emitted
// NONE of these: a caller had to poll session.info to learn a child
// had even started.
func (s *Server) onChildTurnStart(id string) {
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
}

func (s *Server) onChildTurnEnd(id string, msg *message.Message, err error, canceled bool) {
	s.syncMessages(id)
	switch {
	case canceled:
		s.emitDurable(Event{Type: evtSessionAborted, SessionID: id})
	case err == nil:
		s.recordTurnEnd(id, "completed", nil)
	default:
		s.emitDurable(Event{Type: evtSessionError, SessionID: id, Error: err.Error()})
		s.recordTurnEnd(id, turnEndOutcome(err), err)
	}
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "idle"})
}

// requestSnapshot is the latest fully-assembled model request for a session,
// held in memory only (never persisted) to answer GET /session/{id}/request.
type requestSnapshot struct {
	model       message.ModelRef
	system      []string
	tools       []string
	messages    int
	temperature *float64
	topP        *float64
	maxTokens   int
}

// OnRequest is the engine's OnRequest callback (wired per session at
// construction). It runs synchronously in the engine's prompt goroutine with
// the exact final request about to hit the provider. It journals a durable
// request.meta record — including the full system segments only when their hash
// differs from this session's previous request — and stashes the latest
// assembled request in memory for the /request endpoint. It does not mutate req.
func (s *Server) OnRequest(sessionID string, _ int, req *provider.Request) {
	tools := make([]string, len(req.Tools))
	for i, td := range req.Tools {
		tools[i] = td.Name
	}
	sort.Strings(tools)

	joined := strings.Join(req.System, "\n")
	sum := sha256.Sum256([]byte(joined))
	hash := hex.EncodeToString(sum[:])
	system := append([]string(nil), req.System...)

	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.lastReqHash[sessionID] != hash
	s.lastReqHash[sessionID] = hash
	s.lastRequest[sessionID] = &requestSnapshot{
		model:       req.Model,
		system:      system,
		tools:       tools,
		messages:    len(req.Messages),
		temperature: req.Temperature,
		topP:        req.TopP,
		maxTokens:   req.MaxTokens,
	}
	ev := &Event{
		Type:       evtRequestMeta,
		SessionID:  sessionID,
		Model:      req.Model,
		SystemHash: hash,
		SystemLen:  len(joined),
		Segments:   len(req.System),
		Tools:      tools,
		Messages:   len(req.Messages),
	}
	if changed {
		ev.System = system
	}
	s.emitDurableLocked(ev)
}

// syncMessages appends a durable message record for every message in the
// session's history not yet journaled, in order. It is the single journaling
// path for messages — used live (on each assistant turn) and at boot
// (reconcile) — so the "journal mirrors the session log" invariant cannot
// drift. It is idempotent: already-journaled message IDs are skipped.
//
// sessionID's *engine.Session is resolved via Server.liveSessionObject, not
// a bare s.sessions[sessionID] read — see liveSession's own doc comment for
// why:
// a session SessionManager.Spawn drives (the `task` tool, or session.create's
// parent_id form) is never inserted into s.sessions at all, ever, since it is
// never claimed through claimForPrompt/handleCreate — so a plain residency
// read here would silently and PERMANENTLY drop every message from a
// spawned child's own turn, for every child, always, not merely a
// timing-dependent subset. A live audit surfaced this as "children spawned
// after a long idle gap show zero events" — that framing is really just the
// shape most likely to go unmasked: a child an orchestrator never happens to
// prompt directly afterward (the ordinary fire-and-forget task-tool shape —
// the PARENT gets the completion notification, not the child) never becomes
// s.sessions-resident by ANY other path either, so nothing ever backfills
// it. A child that DOES later receive a direct HTTP prompt (claimForPrompt's
// own cold-load path) becomes resident then, and that turn's own
// "s.syncMessages(id) // catch any message not yet journaled" call
// (runPrompt) backfills its entire history at once — which is why some
// children "journal fine" and others show zero rows: whether an external
// caller happens to touch the child again, not spawn timing itself, decided
// it. The snapshot's SessionManager half makes a spawned child's own
// messages reach the journal as they stream in, the same turn they are
// produced, regardless of whether anything ever prompts it directly again.
// This path never falls through to a disk load (liveSessionObject has no
// such tier, unlike Server.lookup): journaling is for a session something
// in this process is actively driving, never a reason to re-read a log from
// disk. It also skips the manager read entirely for a resident session —
// this runs per journaled message, and residency already answers (see
// withManagerIfUnresolved).
//
// Lock-ordering invariant: server.mu is a LEAF with respect to a session's
// own mutex — this function must never call a session method that acquires
// it while holding server.mu. The engine emits goal.* (and other) events
// while Session.mu is held (see engine/goal.go), and those events flow
// straight into Server.Publish, which acquires server.mu: that is the
// session.mu -> server.mu order. If server.mu ever called back into the
// session's lock (server.mu -> session.mu) the two orders would form a cycle
// and deadlock under concurrency (see TestGoalEmitVsSyncMessagesNoDeadlock).
// So both sess.History() and sess.PersistErr() are read here in an
// unlocked window, server.mu is re-acquired only for this server's own
// bookkeeping (seen-message index, journal, last-seen persist error), and
// Options.OnError is invoked outside server.mu too — an OnError handler that
// happened to call back into the session could otherwise re-enter the same
// cycle.
func (s *Server) syncMessages(sessionID string) {
	sess := s.liveSessionObject(sessionID)
	if sess == nil {
		return
	}
	history := sess.History()
	persistErr := sess.PersistErr()

	s.mu.Lock()
	for i := range history {
		m := history[i]
		if s.isSeenLocked(sessionID, m.ID) {
			continue
		}
		s.markSeenLocked(sessionID, m.ID)
		s.emitDurableLocked(&Event{Type: evtMessage, SessionID: sessionID, Message: &m})
	}
	reportErr := s.checkPersistErrLocked(sessionID, persistErr)
	s.mu.Unlock()

	if reportErr != nil {
		s.reportError(reportErr)
	}
}

// transcriptSyncedThrough answers the "race-closed bootstrap" a client needs
// to tail-load a session's transcript and then resume GET /event strictly
// after it, with no REPLAY window that can re-deliver (or, symmetrically,
// permanently drop) a message straddling the two reads — the tail-load
// versus live-stream race the console's duplicate-render bug traces to (see
// the meetneptune/boxes repo's docs/console-read-path.md, and
// handleMessages' ?stream_from=1 branch, this function's only caller).
//
// It is syncMessages (above) PLUS one extra locked read: sess.History() and
// sess.PersistErr() are read in the exact same unlocked window, under the
// exact same lock-ordering invariant, that syncMessages documents — do not
// invert it. Every message in that snapshot not yet journaled is then
// journaled under one s.mu hold, exactly as syncMessages does, and the
// caller-visible watermark is sampled in that SAME critical section,
// immediately after the journaling loop: nothing can be appended to this
// session's journal between "the snapshot is fully durable" and "the
// watermark was read," because emitDurableLocked never runs without s.mu.
//
// liveFrom is a SECOND, separate cursor — see docs/design/live-event-tip-
// cursor.md for the full argument this doc comment only summarizes. seq
// (the message watermark above) only ever counts a message present in the
// returned history, so a session with a lot of OTHER durable journal
// activity under the same id — any event type, or an evtMessage excluded
// from history for any reason — sits far below the box-global event tip,
// and a client resuming GET /event from it replays all of that activity
// as an unwanted backlog. liveFrom is that tip instead, computed as
// max(seq, tipAtStart): tipAtStart is s.seq sampled (via currentSeq)
// before this function reads anything else, so it is a fresh, empty
// upper bound before this call has journaled or observed a single byte.
// Both terms are individually proven, in the design doc, never to reach
// the seq of a record this call must keep replayable — seq by the same
// proof this function's own doc comment already gives for it, tipAtStart
// because any record excluded from the history this call returns was, by
// definition, appended to the session strictly after this call's own
// sess.History() read, which itself runs strictly after tipAtStart's
// already-released critical section — so their max inherits the same
// guarantee. In the common case (nothing durable for this session
// predates this call) tipAtStart is 0 or otherwise below seq, and
// liveFrom collapses to exactly seq.
//
// It returns the SAME history slice it just journaled — never a second
// sess.History() call, and never syncMessages followed by a re-read — both
// of which would reopen an identical race one level down instead of closing
// it: this function's whole point is that the returned history and the
// returned seq describe the exact same instant.
//
// lookupSession, not liveSessionObject: a cold (on-disk, non-resident)
// session must answer this exactly like handleMessages' unparameterized read
// already does. This is a GET, not a journaling trigger tied to a live turn,
// so a caller must never be told "no such session" just because nothing in
// this process happens to be driving it right now.
//
// seqs is a THIRD, additive result (see docs/design/transcript-tail-seqs.md):
// the durable journal seq of each entry in history, in the same order,
// 0 for one with none (a message.IsSyntheticOrphanID load-time repair, or —
// in principle, never observed in practice since the loop below journals
// every other entry before this is sampled — one this call could not
// journal for some other reason). It exists so a caller that BUDGETS this
// history down to a shorter tail (meetneptune/boxes's byte-budget
// console-bootstrap read, which trims client-side after this call returns
// the whole thing) can still learn which durable seq its own kept window
// starts at, and page backward from a real anchor on its FIRST "load
// older" request instead of re-fetching this same newest page to merely
// discover one. It is sampled from s.journal in the SAME locked section as
// seq/liveFrom, after the journaling loop above has run, so a message this
// very call just journaled already has an entry to find.
func (s *Server) transcriptSyncedThrough(id string) (history []message.Message, seq int64, liveFrom int64, seqs []int64, ok bool) {
	sess, ok := s.lookupSession(id)
	if !ok {
		return nil, 0, 0, nil, false
	}
	// Sampled first, before anything else this function does — see the
	// doc comment above for why tipAtStart must precede sess.History().
	tipAtStart := s.currentSeq()

	history = sess.History()
	persistErr := sess.PersistErr()

	if s.transcriptSyncRace != nil {
		// Test-only seam: let a test force a concurrent Publish(EventMessage)
		// call to land deterministically in the (now safe) gap between the
		// unlocked reads above and the s.mu hold below. Always nil in
		// production.
		s.transcriptSyncRace()
	}

	s.mu.Lock()
	for i := range history {
		m := history[i]
		// A message.IsSyntheticOrphanID entry (message.ResolveOrphanToolCalls'
		// load-time repair, folded into sess.History() for a cold-loaded
		// session — see engine.LoadSession) exists only to keep a REQUEST
		// protocol-valid; it is never itself persisted to the session's own
		// log, so it has no durable identity to journal against. Giving it a
		// seq would violate the same rule durableOnly (handlers.go) already
		// enforces for the before_seq/limit page — "a page must never give
		// one a seq, whichever path produced the page" — and, worse, is not
		// even idempotent the way a real message's journaling is: nothing
		// backs its "seen" mark across a restart, since it is re-derived
		// fresh on every load rather than replayed from events.jsonl.
		if message.IsSyntheticOrphanID(m.ID) {
			continue
		}
		if s.isSeenLocked(id, m.ID) {
			continue
		}
		s.markSeenLocked(id, m.ID)
		s.emitDurableLocked(&Event{Type: evtMessage, SessionID: id, Message: &m})
	}
	reportErr := s.checkPersistErrLocked(id, persistErr)
	seq = s.transcriptWatermarkLocked(id, history)
	liveFrom = tipAtStart
	if seq > liveFrom {
		liveFrom = seq
	}
	seqs = s.messageSeqsLocked(id, history)
	s.mu.Unlock()

	if reportErr != nil {
		s.reportError(reportErr)
	}
	return history, seq, liveFrom, seqs, true
}

// messageSeqsLocked returns, parallel to history, the durable journal seq of
// each entry — 0 for one with none, e.g. a message.IsSyntheticOrphanID
// load-time repair, which transcriptSyncedThrough's own journaling loop
// (above) deliberately never assigns a seq to. Caller holds s.mu, and must
// call this AFTER that loop has run: a message this very call journals must
// already be in s.journal for this scan to find it.
//
// A linear scan of s.journal, exactly like transcriptWatermarkLocked's own
// — this runs once per transcriptSyncedThrough call, alongside a scan that
// function already pays for the same session.
func (s *Server) messageSeqsLocked(sessionID string, history []message.Message) []int64 {
	bySeq := make(map[string]int64, len(history))
	for _, ev := range s.journal {
		if ev.Type != evtMessage || ev.SessionID != sessionID || ev.Message == nil {
			continue
		}
		bySeq[ev.Message.ID] = ev.Seq
	}
	seqs := make([]int64, len(history))
	for i := range history {
		seqs[i] = bySeq[history[i].ID]
	}
	return seqs
}

// transcriptWatermarkLocked returns the durable seq up to which sessionID's
// message transcript is FULLY represented by history: the highest seq among
// this session's journaled evtMessage records whose message ID appears in
// history — capped below any compaction summary history excludes (see the
// "compaction can reorder" section below). Returns 0 when history has
// nothing journaled yet (a brand-new or still cold session): the safe
// answer, not a gap — see the doc comment on transcriptSyncedThrough's only
// caller, handleMessages, for why 0 can never itself straddle a message.
//
// This is deliberately NOT sessionSeqLocked(sessionID) — the plain highest
// durable seq recorded for the session, of ANY event type. sess.History()
// and sess.PersistErr() in transcriptSyncedThrough above are read OUTSIDE
// s.mu (the same lock-ordering invariant syncMessages documents), so a
// concurrent syncMessages call for the SAME session — racing in that
// unlocked window with a FRESHER sess.History() snapshot that already
// includes a message this call's own (now stale) snapshot does not — can win
// the race for s.mu and durably journal that message before this call ever
// acquires it. sessionSeqLocked's raw, type-agnostic max would then already
// count that message's seq, even though the message is absent from the
// `history` this call is about to return: exactly the gap a client resuming
// GET /event?from=<that seq> would never recover from, since a message with
// seq <= the reported watermark is never replayed (sse.go: `ev.Seq > from`).
// See TestTranscriptStreamFrom_ConcurrentJournalDuringSnapshot, which forces
// this exact interleaving via the transcriptSyncRace seam.
//
// Restricting the max to message IDs actually present in history closes
// that gap FOR A PLAIN APPEND: every message in history was appended to the
// session no later than this call's sess.History() snapshot, so any message
// NOT in history was necessarily appended strictly after it, and every
// syncMessages-family loop (this one included) journals a session's
// messages in history ARRAY order, under one s.mu hold — so as long as
// history only ever grows at the tail, a later-appended message can never
// be assigned a lower seq than an earlier one, whichever call performs the
// journaling.
//
// Compaction (engine/compact.go's Session.Compact) breaks that "only grows
// at the tail" assumption: it SPLICES a new summary message into an EARLIER
// array position, replacing the folded range, then journals the resulting
// history in array order — so the summary can receive a LOWER seq than a
// message that already sat later in this call's own (pre-compaction) stale
// snapshot, even though the summary was created (compacted) after that
// snapshot was taken. A summary excluded from history purely by this
// reordering must still end up with seq > the returned watermark, or its
// paired history.compacted reconciliation record (see publishHistoryCompacted)
// would sit at seq <= watermark while absent from history — permanently
// unrecoverable via SSE resume, unlike an ordinary excluded message, which
// self-heals by arriving live. So: for every evtHistoryCompacted record for
// this session whose CompactSummaryID is NOT in history, the watermark is
// capped to strictly below that summary's own journaled seq (looked up by
// ID) — even if that lowers it below some in-history message's already-
// higher seq. The tradeoff is deliberate and asymmetric: a message in
// history dropping below the cap is merely redelivered live once more (the
// ordinary duplicate this endpoint exists to reduce, still bounded and
// self-correcting by message ID), never lost — whereas letting the summary
// slip above the watermark is unrecoverable. See
// TestTranscriptStreamFrom_CompactionDuringSnapshotStaysRecoverable.
//
// # The two-emit sandwich window
//
// A compaction does not journal its summary and its reconciliation record
// atomically: engine/compact.go emits the summary as an ordinary evtMessage
// first, then EventHistoryCompacted separately, and server-side each goes
// through its own Publish call — its own separate s.mu critical section.
// A bootstrap read can acquire s.mu in the gap between the two: it observes
// the summary's evtMessage (so any stale-history message journaled after it
// in that same gap can raise `highest` past the summary's seq) but not yet
// the evtHistoryCompacted record, so pendingCeilings above is empty and the
// paragraph's cap never engages — stream_from would land above the summary
// while the summary is itself excluded from history, exactly the
// unrecoverable gap this function exists to prevent. Closing this requires
// no second event: a summary excluded from history is identifiable from its
// OWN evtMessage record via engine.IsCompactionSummaryID, independent of
// whether its evtHistoryCompacted record has arrived yet. summaryCeiling
// below captures that directly, in the same first pass, from the message's
// own seq — and is folded into the final cap alongside pendingCeilings, the
// lower of the two winning. The two mechanisms stay complementary rather
// than redundant: pendingCeilings still covers a summary loaded from store
// (its evtHistoryCompacted record present without a matching in-memory
// evtMessage), and summaryCeiling covers the newly-widened sandwich window
// above. See TestTranscriptWatermarkLocked_CompactionSummarySandwich.
// Caller holds s.mu.
func (s *Server) transcriptWatermarkLocked(sessionID string, history []message.Message) int64 {
	inHistory := make(map[string]bool, len(history))
	for i := range history {
		inHistory[history[i].ID] = true
	}

	var highest int64
	// pendingCeilings collects, for every evtHistoryCompacted record whose
	// summary is absent from history, that summary's OWN message ID — its
	// seq is looked up in a second pass below, once highest is known, so the
	// common (no concurrent compaction) case never pays for the lookup.
	var pendingCeilings []string
	// summaryCeiling caps the watermark directly from a compaction summary's
	// own evtMessage record, without waiting for its evtHistoryCompacted
	// record — see the "two-emit sandwich window" section above. -1 means no
	// absent summary evtMessage was seen.
	summaryCeiling := int64(-1)
	for _, ev := range s.journal {
		if ev.SessionID != sessionID {
			continue
		}
		switch ev.Type {
		case evtMessage:
			if ev.Message == nil {
				continue
			}
			if !inHistory[ev.Message.ID] {
				if engine.IsCompactionSummaryID(ev.Message.ID) && (summaryCeiling == -1 || ev.Seq < summaryCeiling) {
					summaryCeiling = ev.Seq
				}
				continue
			}
			if ev.Seq > highest {
				highest = ev.Seq
			}
		case evtHistoryCompacted:
			if !inHistory[ev.CompactSummaryID] {
				pendingCeilings = append(pendingCeilings, ev.CompactSummaryID)
			}
		}
	}
	ceiling := summaryCeiling
	if len(pendingCeilings) > 0 {
		for _, ev := range s.journal {
			if ev.Type != evtMessage || ev.SessionID != sessionID || ev.Message == nil {
				continue
			}
			for _, id := range pendingCeilings {
				if ev.Message.ID == id && (ceiling == -1 || ev.Seq < ceiling) {
					ceiling = ev.Seq
				}
			}
		}
	}
	if ceiling != -1 && ceiling-1 < highest {
		return ceiling - 1
	}
	return highest
}

// emitDurable assigns the next sequence number, journals the event, and fans
// it out to connected clients.
//
// The session.error log line lives here, AFTER s.mu is released, rather than
// in emitDurableLocked itself: emitDurableLocked runs with s.mu held, and
// Options.Logger is caller-supplied I/O (a file, a pipe, a suspended
// terminal) with no bound on how long a write can block — logging under
// s.mu would let a stalled sink wedge the server's single global mutex,
// taking every other handler and SSE fanout down with it. See the doc
// comment on emitDurableLocked for why this is the only clean single point
// for it: every current session.error emission (handlers.go, both call
// sites) goes through this exact function, never emitDurableLocked directly,
// so this covers every record exactly once. A future call site that needs
// to emit session.error from inside a function that already holds s.mu
// (calling emitDurableLocked directly, the way recordTurnEnd and publishGoal
// do for their own event types) must add its own post-unlock log call
// mirroring this one — emitDurableLocked deliberately never logs anything
// itself, so there is no single choke point below this wrapper to rely on.
func (s *Server) emitDurable(ev Event) int64 {
	s.mu.Lock()
	s.emitDurableLocked(&ev)
	s.mu.Unlock()
	if ev.Type == evtSessionError {
		s.logWarn("session error", "session", ev.SessionID, "error", ev.Error)
	}
	return ev.Seq
}

// emitDurableLocked is emitDurable's critical section: assigns the next
// sequence number, journals the event, fans it out, and wakes waiters — all
// under s.mu, and nothing else. It deliberately does no logging of its own
// (see emitDurable's doc comment above): every logging call site in this
// file runs after its caller's s.mu section ends, never inside one, so a
// slow Options.Logger sink can never block the mutex every handler and SSE
// fanout depends on. Caller holds s.mu.
func (s *Server) emitDurableLocked(ev *Event) {
	s.seq++
	ev.Seq = s.seq
	s.writeJournalLocked(*ev)
	s.journal = append(s.journal, *ev)
	s.fanoutLocked(*ev)
	s.notifyWaitersLocked(ev.SessionID)
}

// notifyWaitersLocked wakes every GET /session/{id}/wait long-poll registered
// for sessionID (or, in principle, unfiltered) so it re-checks its condition
// against current state — never by pushing the new state itself, since a
// waiter always re-derives state fresh (see Server.waitSnapshot). That is
// what makes the non-blocking, coalescing send below safe: a dropped or
// coalesced wake can never strand a waiter, because the next one (or the one
// already pending) causes the same fresh re-check. Caller holds s.mu.
func (s *Server) notifyWaitersLocked(sessionID string) {
	for wt := range s.waiters {
		if wt.session != "" && wt.session != sessionID {
			continue
		}
		select {
		case wt.ch <- struct{}{}:
		default:
		}
	}
}

// publishLive fans a non-durable event out to clients without journaling it.
func (s *Server) publishLive(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fanoutLocked(ev)
}

// fanoutLocked delivers ev to every matching subscriber. A slow subscriber's
// event is dropped rather than blocking the publisher; that client can
// reconnect with from=<last seq> to recover durable records. Caller holds s.mu.
func (s *Server) fanoutLocked(ev Event) {
	for sub := range s.subs {
		if sub.session != "" && sub.session != ev.SessionID {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

// writeJournalLocked appends one record to events.jsonl. Write failures are
// recorded in s.lastErr and, when Options.OnError is set, forwarded (wrapped
// with "journal write: %w") — never fatal either way. Caller holds s.mu.
func (s *Server) writeJournalLocked(ev Event) {
	if s.jf == nil {
		return
	}
	b, err := json.Marshal(ev)
	if err != nil {
		s.lastErr = err
		s.reportError(fmt.Errorf("journal write: %w", err))
		return
	}
	if _, err := s.jf.Write(append(b, '\n')); err != nil {
		s.lastErr = err
		s.reportError(fmt.Errorf("journal write: %w", err))
	}
}

// checkPersistErrLocked folds an already-read sess.PersistErr() value (see
// the lock-ordering comment on syncMessages — it must be read outside
// server.mu, never here) into the per-session last-seen-error bookkeeping,
// returning the error to forward to Options.OnError (wrapped with "session
// %s persist: %w") the first time it appears for this session or changes
// from the last-forwarded error, or nil if it has already been forwarded or
// there is none — so a persistently-failing write is reported once rather
// than on every syncMessages call. The caller invokes OnError itself, after
// releasing server.mu. Caller holds s.mu.
func (s *Server) checkPersistErrLocked(sessionID string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if s.lastPersistErr[sessionID] == msg {
		return nil
	}
	s.lastPersistErr[sessionID] = msg
	return fmt.Errorf("session %s persist: %w", sessionID, err)
}

func (s *Server) markSeenLocked(sessionID, msgID string) {
	m := s.seen[sessionID]
	if m == nil {
		m = make(map[string]bool)
		s.seen[sessionID] = m
	}
	m[msgID] = true
}

func (s *Server) isSeenLocked(sessionID, msgID string) bool {
	return s.seen[sessionID][msgID]
}

// sessionSeqLocked returns the highest durable seq recorded for a session, or 0.
// Caller holds s.mu.
func (s *Server) sessionSeqLocked(sessionID string) int64 {
	var max int64
	for _, ev := range s.journal {
		if ev.SessionID == sessionID && ev.Seq > max {
			max = ev.Seq
		}
	}
	return max
}

// reconcile loads the existing journal into memory, then appends message
// records for any session-log message missing from it — the crash-recovery
// path for a process that died between the engine's session-log append and the
// server's journal append. Runs once, at New, before any client connects.
func (s *Server) reconcile() error {
	if s.opts.SessionDir == "" {
		return nil // in-memory only; nothing to reconcile
	}
	path := filepath.Join(s.opts.SessionDir, journalName)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.loadJournal(data)

	if err := os.MkdirAll(s.opts.SessionDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	s.jf = f

	// Ids only. This pass loads every session in full anyway, so reading
	// each session's metadata index first would be pure waste — and it
	// would refold and rewrite a stale sidecar for every session in the
	// directory before the load that supersedes it.
	ids, err := engine.ListSessionIDs(s.opts.SessionDir)
	if err != nil {
		return err
	}
	for _, id := range ids {
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			continue // unreadable log: skip, do not fail the whole boot
		}
		history := sess.History()
		for i := range history {
			m := history[i]
			if s.isSeenLocked(id, m.ID) {
				continue
			}
			s.markSeenLocked(id, m.ID)
			s.emitDurableLocked(&Event{Type: evtMessage, SessionID: id, Message: &m})
		}
	}
	return nil
}

// loadJournal parses an existing events.jsonl into memory: replay buffer,
// highest seq, the seen-message index, each session's last_turn (see
// Server.lastTurn), and each session's goalState tracker. Records are
// replayed in file order, so the last record of a given kind seen for a
// session as the scan proceeds is — by construction — its most recent one;
// a later record simply overwrites (or, for the goal state machine below,
// folds into) an earlier one in the map, with no need to track sequence
// numbers here. A truncated final line (crash mid-write) is tolerated.
//
// Rebuilding lastTurn/goalState here (rather than leaving them to be set
// only by a live event flowing through publishGoal/recordTurnEnd) is what
// makes last_turn and an active goal survive a process restart: otherwise a
// durable, replayable record would still exist on disk while the in-memory
// field it drives silently reset to absent — exactly the kind of
// restart-loses-state gap those fields exist to prevent an orchestrator
// from hitting. This is a pure extension of the existing single replay pass
// — no new I/O, per the startup-budget rule (see reconcile, which already
// reads events.jsonl once).
//
// The goal.* folding below mirrors publishGoal exactly (same field
// assignments, same state-machine transitions) but without its locking or
// fan-out: loadJournal runs once at construction, before the server is
// reachable by any client, so there is no concurrent access to guard
// against and nothing to notify yet.
func (s *Server) loadJournal(data []byte) {
	lines := bytes.Split(data, []byte("\n"))
	for i, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			if i == len(lines)-1 {
				break // truncated final line: crash mid-write, ignore
			}
			continue
		}
		s.journal = append(s.journal, ev)
		if ev.Seq > s.seq {
			s.seq = ev.Seq
		}
		if ev.Type == evtMessage && ev.Message != nil {
			s.markSeenLocked(ev.SessionID, ev.Message.ID)
		}
		if ev.Type == evtTurnEnd {
			s.lastTurn[ev.SessionID] = &turnOutcome{outcome: ev.Outcome, error: ev.Error}
		}
		s.foldGoalRecordLocked(ev)
	}
}

// foldGoalRecordLocked applies one journal record to its session's
// goalTracker, exactly mirroring publishGoal's switch (see that function's
// doc comment for the per-case reasoning) so a replayed goal.* record
// leaves goalState in the identical shape a live event would have. A no-op
// for any non-goal record type. Despite the name it takes no lock itself —
// "Locked" here follows the repo's convention for a helper that assumes it
// is safe to mutate s's maps directly (loadJournal runs single-threaded, at
// construction, before any client can reach the server).
func (s *Server) foldGoalRecordLocked(ev Event) {
	switch ev.Type {
	case evtGoalSet, evtGoalUpdated, evtGoalEval, evtGoalStalled, evtGoalAchieved, evtGoalCleared, evtGoalEvalFailed, evtGoalParked:
	default:
		return
	}
	g := s.goalState[ev.SessionID]
	if g == nil {
		g = &goalTracker{}
		s.goalState[ev.SessionID] = g
	}
	switch ev.Type {
	case evtGoalSet:
		g.condition = ev.GoalCondition
		g.active = true
		g.achieved = false
		g.turns = 0
		g.lastReason = ""
		g.attempt = 0
		g.retryable = false
		g.retryableClass = ""
		g.waiting = false
		g.pausedRestart = false
		g.pausedWorker = false
		g.evalFailures = 0
	case evtGoalUpdated:
		// Mirrors publishGoal's evtGoalUpdated case exactly: condition,
		// eval_failures, and pausedWorker reset, nothing else.
		g.condition = ev.GoalCondition
		g.evalFailures = 0
		g.pausedWorker = false
	case evtGoalEval:
		g.turns = ev.GoalTurn
		g.lastReason = ev.GoalReason
		g.attempt = 0
		g.retryable = false
		g.retryableClass = ""
		g.waiting = false
		g.evalFailures = 0
		g.pausedWorker = false
	case evtGoalStalled:
		g.lastReason = ev.GoalReason
		g.attempt = ev.GoalAttempt
		g.retryable = ev.GoalRetryable
		g.retryableClass = ev.GoalRetryableClass
		g.waiting = ev.GoalWaiting
	case evtGoalEvalFailed:
		// Mirrors publishGoal's evtGoalEvalFailed case exactly: folded
		// straight from the record's own count, idempotent on replay.
		g.evalFailures = ev.GoalEvalFailures
	case evtGoalParked:
		// Mirrors publishGoal's evtGoalParked case exactly (see its doc
		// comment): GoalAttempt here already carries the record's total
		// attempt count (publishGoal maps engine's GoalAttempts into this
		// same wire field before journaling), so this replay-path read needs
		// no separate field either.
		g.pausedWorker = true
		g.lastReason = ev.GoalReason
		g.attempt = ev.GoalAttempt
		g.retryable = ev.GoalRetryable
		g.retryableClass = ev.GoalRetryableClass
		g.waiting = false
	case evtGoalAchieved:
		g.active = false
		g.achieved = true
		g.turns = ev.GoalTurns
		g.lastReason = ev.GoalReason
		g.attempt = 0
		g.retryable = false
		g.retryableClass = ""
		g.waiting = false
		g.pausedRestart = false
		g.pausedWorker = false
		g.evalFailures = 0
	case evtGoalCleared:
		g.active = false
		g.pausedRestart = false
		g.pausedWorker = false
		g.evalFailures = 0
	}
}

// pauseArmedGoalsAtBoot is deliverable 2(a)'s boot-time fix for the
// operator trap: after loadJournal has replayed events.jsonl (via
// reconcile), any session whose goalTracker reads active is, by
// construction, a goal this freshly-started process has never attached a
// loop to (a live loop's own claim/spawn happens only inside handleGoal,
// well after New returns) — so it is marked paused=true/pause_reason=
// "restart" and a durable goal.paused record is appended, so the journal's
// history reads honestly instead of silently omitting why the goal never
// progressed. Runs once, single-threaded, before the server is reachable by
// any client (same discipline as reconcile/loadJournal) — no locking needed.
func (s *Server) pauseArmedGoalsAtBoot() {
	for id, g := range s.goalState {
		if !g.active || g.pausedRestart {
			continue
		}
		g.pausedRestart = true
		s.emitDurableLocked(&Event{
			Type:            evtGoalPaused,
			SessionID:       id,
			GoalCondition:   g.condition,
			GoalPaused:      true,
			GoalPauseReason: pauseReasonRestart,
		})
	}
}
