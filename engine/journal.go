// GET /session/{id}/journal support: a read-only, sanitized projection of a
// session's own durable log (store.go's `record` type) for the debugging
// class the restart-recovery work (PR #145's task-notification
// checkout/commit/requeue mechanism, PR #147's crashed-child recovery fix)
// kept needing pod-exec to see directly — recovery markers
// (recoverInterruptedTurnLocked's own synthetic closing messages), the
// task-notification queued/delivered/committed trail, and turn-settlement
// records. See server/session_journal.go's handleJournal for the HTTP surface this
// backs.
//
// This is deliberately a SEPARATE, curated type from store.go's own
// `record` — never the raw internal wire format — for two reasons: it never
// carries a message's full content (GET /session/{id}/message already owns
// that), and every field that can carry a raw provider/tool error string is
// sanitized through plugin.SanitizeSessionError before it ever leaves this
// package, exactly like every other session-error surface (OnError,
// session.error — see engine.go).

package engine

import (
	"fmt"
	"os"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
)

// JournalRecord is one line of a session's durable log, reshaped for
// read-only external consumption. Seq is the record's 1-based position in
// the log (scanLog's own `line`) — stable across reloads, since the log is
// append-only and never rewritten, so it doubles as a pagination cursor
// (see server/session_journal.go's handleJournal: `from` filters Seq > from).
// Type is one of the recXxx constants in store.go (e.g. "session",
// "message", "goal.stalled", "task.notify_queued") — a client MUST
// tolerate a type it does not recognize, same contract as every other
// wire-facing kind this codebase exposes (see server/journal.go's Event.Type
// doc comment for the identical convention on the live/SSE side).
//
// Only the fields relevant to Type are populated; every other field is its
// zero value (and omitted on the wire via omitempty/omitzero) — mirroring
// server/journal.go's Event struct, the established shape for a single flat
// record type carrying many event kinds' worth of optional fields.
type JournalRecord struct {
	Seq       int       `json:"seq"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Session header (Type == recSession) fields.
	WorkDir       string `json:"workdir,omitempty"`
	ParentSession string `json:"parent_session,omitempty"`
	TaskParentID  string `json:"task_parent_id,omitempty"`
	TaskAgentType string `json:"task_agent_type,omitempty"`
	// TaskDepth mirrors Config.TaskDepth's own doc comment (engine.go): the
	// child's durable tree depth, recorded by Spawn and restored by
	// LoadSession the same omit/restore rule as TaskParentID/TaskAgentType
	// above (0 on a legacy header predating this field — never a real
	// child's true depth, which is always >= 1). Exposed here so this
	// debug endpoint can show the exact durable value
	// server.lineageJSONFor's wire derivation now prefers over the
	// m.maxDepth refusal sentinel.
	TaskDepth int `json:"task_depth,omitempty"`

	// Message identity (Type == recMessage) -- never content; see
	// GET /session/{id}/message for the message's own full parts.
	// RecoveryMarker is true when this message is recognizable as one of
	// recoverInterruptedTurnLocked's own synthetic closers
	// (isRecoverySyntheticCloser, session_manager.go) — the durable trace of
	// a restart/cancel recovery that, before this endpoint existed, was
	// visible only by pod-exec'ing in and grepping the raw session log for
	// its fixed wording.
	MessageID      string `json:"message_id,omitempty"`
	MessageRole    string `json:"message_role,omitempty"`
	RecoveryMarker bool   `json:"recovery_marker,omitempty"`

	// Model (Type == recSession or recModel) / effort (Type == recSession or
	// recEffort).
	Model message.ModelRef `json:"model,omitzero"`
	// Effort is a *message.Effort, not a bare message.Effort with
	// omitempty, mirroring server/journal.go's identical Event.Effort field
	// (see its own doc comment): a recEffort record's SetEffort clear to
	// the provider default writes Effort == "" — an explicit, meaningful
	// wire value ("effort":"") — which a bare string with omitempty would
	// indistinguishably drop, reading identically to a record type that
	// never carries an effort level at all. projectJournalRecord always
	// sets a non-nil pointer on recSession/recEffort (even for an empty
	// value) and leaves it nil on every other record type.
	Effort *message.Effort `json:"effort,omitempty"`

	// Goal trace (Type is one of recGoalSet/Updated/Eval/Stalled/Achieved/
	// Cleared/EvalFailed/Parked). GoalReason is sanitized: goal.stalled and
	// goal.eval_failed carry a raw provider/tool err.Error() here (see
	// goalRecord.Reason's own doc comment, store.go) — SanitizeSessionError
	// is applied unconditionally to every goal record's Reason (a no-op on
	// the other event types' already-classified text) rather than
	// special-casing which ones need it, so this can never silently miss a
	// future record type that starts carrying raw text too.
	GoalCondition      string `json:"goal_condition,omitempty"`
	GoalReason         string `json:"goal_reason,omitempty"`
	GoalMet            bool   `json:"goal_met,omitempty"`
	GoalTurn           int    `json:"goal_turn,omitempty"`
	GoalTurns          int    `json:"goal_turns,omitempty"`
	GoalAttempt        int    `json:"goal_attempt,omitempty"`
	GoalAttempts       int    `json:"goal_attempts,omitempty"`
	GoalRetryable      bool   `json:"goal_retryable,omitempty"`
	GoalRetryableClass string `json:"goal_retryable_class,omitempty"`
	GoalWaiting        bool   `json:"goal_waiting,omitempty"`
	GoalEvalFailures   int    `json:"goal_eval_failures,omitempty"`

	// Prompt queue (Type == recPromptQueued or recPromptDequeued).
	PromptID     int64  `json:"prompt_id,omitempty"`
	PromptReason string `json:"prompt_reason,omitempty"`
	PromptSeq    int64  `json:"prompt_seq,omitempty"`

	// Task delivery -- the subagent-sessions checkout/commit/requeue trail
	// (Type is one of recTaskSpawned/recTaskNotifyQueued/
	// recTaskNotifyDelivered/recTaskOutcomeCommitted). TaskFailReason is
	// sanitized unconditionally, same reasoning as GoalReason above, even
	// though taskNotification.FailReason is already documented as
	// classified (#82-rule) text — defense in depth costs nothing here.
	ChildID        string `json:"child_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	TaskStatus     string `json:"task_status,omitempty"`
	TaskFailReason string `json:"task_fail_reason,omitempty"`
	TaskCanceled   bool   `json:"task_canceled,omitempty"`

	// Compaction (Type == recCompact). The folded summary message's own
	// content is not repeated here -- it already flowed through a preceding
	// recMessage record.
	CompactFirstID     string `json:"compact_first_id,omitempty"`
	CompactLastID      string `json:"compact_last_id,omitempty"`
	CompactTurnsFolded int    `json:"compact_turns_folded,omitempty"`

	// Retained tool result pointer (Type == recToolResultRetained).
	ToolResultHandle string `json:"tool_result_handle,omitempty"`
	ToolResultTool   string `json:"tool_result_tool,omitempty"`
	ToolResultBytes  int    `json:"tool_result_bytes,omitempty"`

	// recChildTurnSettled carries no payload beyond Type/Seq -- a pure
	// marker, exactly like its own doc comment (store.go) describes.
}

// LoadJournal reads sessionDir/id's session log and returns its full record
// history, oldest first, reshaped into JournalRecord (see that type's own
// doc comment for why this is a curated projection, never store.go's raw
// `record`). It never partially fails: like LoadSession, a corrupt or
// truncated FINAL line (a crash mid-write) is silently tolerated, and a
// corrupt line anywhere else is a hard error.
//
// A session with no log file yet — SessionSync writes lazily, so a session
// created but never prompted/persisted has none (see Session.Persist's own
// doc comment) — reports (nil, an *os.PathError wrapping fs.ErrNotExist),
// exactly like os.ReadFile: a caller that already confirmed the session
// exists some other way (server/session_journal.go's handleJournal calls s.lookup
// first) should treat that as "no records yet" and answer an empty list,
// not a hard error.
func LoadJournal(sessionDir, id string) ([]JournalRecord, error) {
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	data, err := os.ReadFile(sessionPath(sessionDir, id))
	if err != nil {
		return nil, err
	}
	var out []JournalRecord
	err = scanLog(data, func(rec record, line int, isLast bool) error {
		out = append(out, projectJournalRecord(line, rec))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// projectJournalRecord reshapes one raw store.go `record` (seq is its
// scanLog-assigned 1-based line number) into its JournalRecord projection —
// see that type's own doc comment for the sanitization and
// content-exclusion rules every case below follows.
func projectJournalRecord(seq int, rec record) JournalRecord {
	out := JournalRecord{Seq: seq, Type: rec.Type, CreatedAt: rec.CreatedAt}
	switch rec.Type {
	case recSession:
		out.WorkDir = rec.WorkDir
		out.ParentSession = rec.ParentSession
		out.TaskParentID = rec.TaskParentID
		out.TaskAgentType = rec.TaskAgentType
		out.TaskDepth = rec.TaskDepth
		out.Model = rec.Model
		out.Effort = effortPtr(rec.Effort)
	case recMessage:
		if rec.Message != nil {
			out.MessageID = rec.Message.ID
			out.MessageRole = string(rec.Message.Role)
			out.CreatedAt = rec.Message.CreatedAt
			out.RecoveryMarker = isRecoverySyntheticCloser(*rec.Message)
		}
	case recModel:
		out.Model = rec.Model
	case recEffort:
		out.Effort = effortPtr(rec.Effort)
	case recGoalSet, recGoalUpdated, recGoalEval, recGoalStalled, recGoalAchieved, recGoalCleared, recGoalEvalFailed, recGoalParked:
		if rec.Goal != nil {
			out.GoalCondition = rec.Goal.Condition
			out.GoalReason = plugin.SanitizeSessionError(rec.Goal.Reason)
			out.GoalMet = rec.Goal.Met
			out.GoalTurn = rec.Goal.Turn
			out.GoalTurns = rec.Goal.Turns
			out.GoalAttempt = rec.Goal.Attempt
			out.GoalAttempts = rec.Goal.Attempts
			out.GoalRetryable = rec.Goal.Retryable
			out.GoalRetryableClass = rec.Goal.RetryableClass
			out.GoalWaiting = rec.Goal.Waiting
			out.GoalEvalFailures = rec.Goal.EvalFailures
		}
	case recPromptQueued, recPromptDequeued:
		if rec.Prompt != nil {
			out.PromptID = rec.Prompt.ID
			out.PromptReason = rec.Prompt.Reason
			out.PromptSeq = rec.Prompt.Seq
		}
	case recTaskSpawned:
		if rec.TaskSpawn != nil {
			out.ChildID = rec.TaskSpawn.ChildID
			out.Agent = rec.TaskSpawn.Agent
		}
	case recTaskNotifyQueued, recTaskNotifyDelivered, recTaskOutcomeCommitted:
		if rec.TaskNotify != nil {
			out.ChildID = rec.TaskNotify.ChildID
			out.Agent = rec.TaskNotify.Agent
			out.TaskStatus = string(rec.TaskNotify.Status)
			out.TaskFailReason = plugin.SanitizeSessionError(rec.TaskNotify.FailReason)
			out.TaskCanceled = rec.TaskNotify.Canceled
		}
	case recCompact:
		if rec.Compact != nil {
			out.CompactFirstID = rec.Compact.FirstID
			out.CompactLastID = rec.Compact.LastID
			out.CompactTurnsFolded = rec.Compact.TurnsFolded
		}
	case recToolResultRetained:
		if rec.ToolResult != nil {
			out.ToolResultHandle = rec.ToolResult.Handle
			out.ToolResultTool = rec.ToolResult.Tool
			out.ToolResultBytes = rec.ToolResult.Bytes
		}
	case recChildTurnSettled:
		// Pure marker, no payload -- see store.go's own doc comment.
	}
	return out
}

// effortPtr returns a non-nil pointer to a local copy of e, always -- even
// when e is EffortUnset ("") -- so JournalRecord.Effort's own doc comment
// holds: a cleared effort renders as an explicit "effort":"" wire value,
// never an omitted key indistinguishable from "this record type never
// carries an effort level."
func effortPtr(e message.Effort) *message.Effort {
	return &e
}
