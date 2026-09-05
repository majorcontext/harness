// Session logs use append-only JSONL. The engine creates them on the first write.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// Record types, one JSON object per line.
const (
	recSession      = "session"
	recMessage      = "message"
	recModel        = "model"
	recEffort       = "effort"
	recServiceTier  = "service_tier"
	recGoalSet      = "goal.set"
	recGoalUpdated  = "goal.updated"
	recGoalEval     = "goal.eval"
	recGoalStalled  = "goal.stalled"
	recGoalAchieved = "goal.achieved"
	recGoalCleared  = "goal.cleared"
	// recGoalEvalFailed is one failed evaluator boundary: a provider error the
	// in-boundary retryable
	// retry couldn't ride out, or two consecutive unparseable replies. Like
	// recGoalStalled it is a pure trace record — it never by itself changes
	// goalActive (see LoadSession's fold below); only a later goal.cleared
	// (the terminal horizon) or goal.eval/goal.achieved (a recovered
	// boundary) does that.
	recGoalEvalFailed = "goal.eval_failed"
	// recGoalParked is the terminal PursueGoal reaches when a worker turn
	// exhausts either exhaustion tier (deterministic goalWorkerRetries or
	// retryable-class goalRetryableMaxAttempts) WITHOUT clearing the
	// goal: the goal stays active, and PursueGoal returns instead of
	// looping or waiting further. Like recGoalStalled/recGoalEvalFailed it
	// is a pure trace record on resume — LoadSession folds it as trace,
	// never touching goalActive (see the fold switch below) — a park is
	// resumed by an external caller starting a fresh PursueGoal call, not
	// by anything in this package.
	recGoalParked = "goal.parked"
	// recPromptQueued/recPromptDequeued are the prompt-queue records (see
	// queue.go and docs/plans/2026-07-19-prompt-queue.md): one prompt.queued
	// per EnqueuePrompt call, one prompt.dequeued per pop (whatever the
	// reason — delivered/injected/cleared). Queued text never becomes a
	// recMessage until delivered, so these are the only durable trace of a
	// pending prompt.
	recPromptQueued   = "prompt.queued"
	recPromptDequeued = "prompt.dequeued"
	// recCompact is the compaction record (see compact.go and docs/design/
	// context-compaction.md §2 "Journal shape"): one per successful
	// Session.Compact call, carrying the full summary message inline (not
	// a separate recMessage) and the summarization call's own Usage.
	recCompact = "compact"
	// recToolResultRetained is one retained tool result's durable POINTER
	// record (see toolresult.go and docs/plans/2026-08-19-tool-result-
	// handles.md §5): handle, source tool, and size. Deliberately not the
	// content — the bytes live in the per-session sidecar file, precisely
	// so LoadSession's full-log replay never pays for them. It is what
	// makes the trh_N counter, the handle metadata, and the retained-bytes
	// total survive a process restart.
	recToolResultRetained = "toolresult.retained"
	// recMCPToolsSelected records the namespaced MCP tool names a session
	// has loaded the schemas of (see mcp_lazy.go and docs/design/
	// mcp-lazy-tools.md §5). It follows recToolResultRetained: engine-
	// internal session state, journaled and folded by LoadSession, with no
	// engine event and no server journal mapping -- a selection is not a
	// lifecycle transition a dashboard renders, it is state a resumed
	// session must not lose.
	//
	// TWO writers produce it, and an implementation that wires only the
	// first loses the guarantee the second exists for:
	//
	//   - the mcp tool's select action, one record per call that adds
	//     names (selected and pending together -- they differ only in
	//     whether the tool is reachable right now, which the durable state
	//     has no reason to keep apart), and
	//   - a routed MCP tool call, when its own name enters the set (use
	//     implies selection), which is what keeps a tool the model is
	//     already using loaded across an auto flip.
	//
	// The record is written when a name ENTERS the set, not once per
	// session: a repeat call finds the name already there and writes
	// nothing, while a tool that was reaped (see reapMCPSelections) and
	// then used again enters a second time and writes again. Replay
	// dedups, so the restored set is identical either way.
	recMCPToolsSelected = "mcp.tools_selected"
	// recTaskSpawned/recTaskNotifyQueued/recTaskNotifyDelivered are the
	// subagent-sessions task-delivery records (see session_manager.go's
	// Spawn and taskdelivery.go), two follow-ups from PR #145's
	// architecture review landing as one journal mechanism:
	//
	//   - "Child journal records": before these existed, a task spawn and
	//     its eventual delivery were visible ONLY as ordinary conversation
	//     text (the injected "[tasks: ...]" EngineContext block) — no
	//     durable, structured, independently-queryable trace of "child X
	//     spawned by Y at T" or "child X delivered to Y, status=done, at
	//     T2" existed, distinct from a human/log-reader grepping rendered
	//     text.
	//   - "Notification persistence": before these existed, a completed
	//     child's pending "must still notify parent" signal lived ONLY in
	//     Session.taskNotifications, an in-memory slice — if the parent
	//     process crashed or was evicted after the child finished but
	//     BEFORE the parent's own next turn checked the notification out,
	//     nothing durable recorded that a delivery was owed at all: a
	//     silent, permanent drop.
	//
	// recTaskSpawned is written once, on the PARENT's log, at Spawn —
	// an informational audit trail ("child X spawned by Y at T") that ALSO,
	// as of a live prod finding, folds into Session.spawnedChildIDs
	// (engine.go): the durable record recoverCrashedChildrenLocked
	// (session_manager.go) consults to discover a freshly-adopted node's
	// own children and check each for a crashed, never-recovered turn —
	// see that method's own doc comment for the full mechanism this closes
	// (a box that only ever GETs/resumes the PARENT after a restart, never
	// touching the crashed CHILD directly, used to leave that child's
	// "lost to restart" notification undelivered forever — the "reactive,
	// not proactive" scope cut this design doc's own "Accepted scope cut"
	// bullet had flagged as future work, escalated after live prod
	// evidence). recTaskNotifyQueued/recTaskNotifyDelivered mirror
	// recPromptQueued/recPromptDequeued's own queued/dequeued shape
	// exactly, and DOUBLE as the notification-persistence fix: LoadSession
	// folds queued-minus-delivered (keyed by ChildID — a child notifies
	// its parent exactly once terminally) directly back into
	// Session.taskNotifications, so a pending, undelivered notification
	// survives a parent-side crash/restart across the exact same reload
	// path recPromptQueued's own un-matched-record fold already
	// established for the prompt queue.
	recTaskSpawned         = "task.spawned"
	recTaskNotifyQueued    = "task.notify_queued"
	recTaskNotifyDelivered = "task.notify_delivered"
	// recChildTurnSettled is a pure marker record (no payload — see
	// recGoalCleared/recGoalAchieved's identical shape), written by
	// SessionManager.finalizeTurn for a non-root node on EVERY terminal
	// outcome (success, ordinary provider error, cancellation) — see
	// Session.turnUnsettled's own doc comment for the durability problem
	// this closes: recoverInterruptedTurnLocked's restart-recovery gate
	// used to infer "was this turn genuinely interrupted" from the
	// TRAILING MESSAGE's own role, a heuristic a live review proved
	// unreliable in both directions (a genuine mid-tool-loop crash can
	// leave a trailing role the ORIGINAL heuristic missed; a properly
	// SETTLED ordinary failure can leave the SAME trailing shape a crash
	// would, since runAgenticLoop's plain-error path appends nothing at
	// all). This record instead marks the fact directly: finalizeTurn
	// running to completion for a node IS the authoritative "this turn's
	// outcome is settled" signal, independent of whatever trailing
	// message shape resulted.
	recChildTurnSettled = "child_turn.settled"
	// recTaskOutcomeCommitted carries the EXACT taskNotification payload
	// (reusing taskNotifyRecord's shape, via record.TaskNotify — the same
	// field recTaskNotifyQueued/recTaskNotifyDelivered use) that
	// finalizeTurn (or recoverInterruptedTurnLocked itself) computed for
	// a non-root node's current, still-unsettled turn — see
	// Session.committedOutcome's own doc comment (engine.go) and the
	// crash-window table on recoverInterruptedTurnLocked's own doc
	// comment (session_manager.go) for the full mechanism. Written
	// BEFORE any delivery attempt, so a crash anywhere in the
	// deliver-then-settle sequence that follows still leaves a later
	// recovery attempt with the AUTHORITATIVE, already-computed payload
	// to replay verbatim, instead of reconstructing a possibly-DIFFERENT
	// one from trailing-history-shape heuristics — the fix for a live
	// review finding: recovery's own reconstruction could diverge from
	// what finalizeTurn already computed (and possibly already
	// delivered) before a crash struck between finalizeTurn's own
	// persist steps, producing a duplicate notification with a
	// DIFFERENT payload than the one the parent may already have
	// received.
	recTaskOutcomeCommitted = "task.outcome_committed"
	// recClaudeCodeSessionID records the Claude Code CLI's OWN session id
	// for a delegated session (see engine/claude_code_backend.go and
	// Session.claudeCodeCLISessionID's own doc comment) — written once,
	// the first time a delegated turn's system/init event reports it, and
	// folded back on LoadSession so --resume keeps naming the same CLI
	// session across a process restart. Mirrors recModel/recEffort
	// exactly: a no-op record type with no lifecycle meaning beyond "the
	// value changed", replayed last-writer-wins.
	recClaudeCodeSessionID = "claude_code.session_id"
	// recClaudeCodeHistoryWatermark records
	// Session.claudeCodeHistoryWatermark (see its own doc comment) —
	// written at the end of every delegated turn that actually started,
	// folded back on LoadSession alongside recClaudeCodeSessionID so a
	// process restart does not lose track of how much history the CLI's
	// resumed session has already incorporated. Mirrors
	// recClaudeCodeSessionID exactly: a no-op record type with no
	// lifecycle meaning beyond "the value changed", replayed
	// last-writer-wins.
	recClaudeCodeHistoryWatermark = "claude_code.history_watermark"
	// recClaudeCodeUsage carries one delegated turn's AGGREGATE Usage (see
	// Session.applyClaudeCodeUsage's own doc comment for why this is a
	// dedicated record rather than riding a recMessage the way a native
	// turn's usage does) — one per completed delegated turn's "result"
	// event. Mirrors recCompact's own "Usage independent of any single
	// message" precedent (record.Usage's doc comment), except this DOES
	// fold into lastUsage on replay, unlike recCompact — see
	// applyClaudeCodeUsage's doc comment for why that divergence is safe
	// here.
	recClaudeCodeUsage = "claude_code.usage"
)

// record is one line of a session log file.
type record struct {
	Type      string    `json:"type"`
	ID        string    `json:"id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	// WorkDir carries Config.WorkDir on the session header record only. It is
	// omitted (and so absent from every record written before this field
	// existed) when empty, which is also how LoadSession recognizes a legacy
	// header with nothing to restore.
	WorkDir string `json:"workdir,omitempty"`
	// ParentSession carries Config.ParentSession on the session header
	// record only, same rule as WorkDir: omitted when empty, and an empty
	// value on load means "nothing to restore" (a legacy header, or a
	// session created with no lineage) rather than "the caller's Config.
	// ParentSession should be cleared".
	ParentSession string `json:"parent_session,omitempty"`
	// TaskParentID carries Config.TaskParentID on the session header
	// record only, same omit/restore rule as ParentSession — but a
	// COMPLETELY DIFFERENT concept (see Config.TaskParentID's doc
	// comment): SessionManager's own tree-lineage pointer, never the
	// opaque provenance pointer ParentSession carries.
	TaskParentID string `json:"task_parent_id,omitempty"`
	// TaskAgentType/TaskToolNames carry Config.TaskAgentType/TaskToolNames
	// on the session header record only — see those fields' own doc
	// comment. Same omit/restore rule as ParentSession/TaskParentID above
	// for TaskAgentType (a legacy header with no field at all restores
	// as if this child was never restricted — the SAME already-accepted
	// gap TaskParentID's own doc comment describes for a session
	// predating that field).
	//
	// TaskToolNames is a *[]string, not a plain []string — deliberately,
	// to fix a live review finding. omitempty on a plain []string omits
	// BOTH nil (no restriction recorded — every record type OTHER than a
	// session header, and a session header for an unrestricted session)
	// AND a non-nil, LEN-ZERO slice (a real, deliberate zero-tool
	// restriction — reachable via Spawn's parent-effective-set
	// INTERSECTION, session_manager.go, whenever a restricted parent's
	// tool set and a child definition's tools are disjoint) to the
	// identical omitted-field wire shape. On reload,
	// restoreTaskToolRestrictionLocked treats an absent TaskToolNames as
	// "fall back to re-resolving TaskAgentType's definition" — which
	// re-applies that definition's OWN tools with no parent intersection
	// at all, over-granting the reloaded child every tool its restricted
	// parent could never reach. omitempty on a POINTER, by contrast, only
	// omits a NIL pointer — a non-nil pointer to an empty slice still
	// marshals as `[]`, round-tripping distinctly from "absent" while an
	// unset restriction (nil pointer) still omits cleanly on every other
	// record type exactly as before (an earlier revision of this fix
	// dropped omitempty entirely instead, which is what a plain []string
	// would have required — but that put `"task_tool_names":null` on
	// every single record of every type, breaking two tests asserting
	// exact log-line shapes; the pointer keeps the field's presence tied
	// to whether IT specifically was ever set, not the whole record
	// type). See the write site (Persist) for how a nil Config.
	// TaskToolNames becomes a nil pointer, and a non-nil one — including
	// empty — becomes a pointer to it.
	TaskAgentType string    `json:"task_agent_type,omitempty"`
	TaskToolNames *[]string `json:"task_tool_names,omitempty"`
	// TaskDepth carries Config.TaskDepth on the session header record
	// only — see that field's own doc comment. Same omit/restore rule as
	// TaskAgentType above: omitted (zero value) for a legacy header
	// predating this field, in which case LoadSession leaves Config's own
	// TaskDepth (also 0 — no caller ever pre-populates it) untouched
	// rather than claiming a false "depth 0".
	TaskDepth int              `json:"task_depth,omitempty"`
	Message   *message.Message `json:"message,omitempty"`
	Model     message.ModelRef `json:"model,omitzero"`
	// Effort carries the reasoning-effort level on the session header record
	// (the level at create time) and on a recEffort record (a SetEffort
	// change). Omitted when EffortUnset, so a legacy log with no effort
	// restores as EffortUnset (provider default) — unchanged behavior.
	Effort message.Effort `json:"effort,omitempty"`
	// ServiceTier carries the Codex speed-tier value on the session header
	// record (the value at create time) and on a recServiceTier record (a
	// SetServiceTier change). Omitted when empty, so a legacy log with no
	// service_tier restores as "" (provider default) — unchanged behavior.
	// Mirrors Effort exactly, but opaque and unvalidated (see
	// provider.Request.ServiceTier).
	ServiceTier string      `json:"service_tier,omitempty"`
	Goal        *goalRecord `json:"goal,omitempty"`
	// Prompt carries a prompt.queued/prompt.dequeued record's payload (see
	// promptRecord and queue.go). nil on every other record type.
	Prompt *promptRecord `json:"prompt,omitempty"`
	// TaskSpawn carries a recTaskSpawned record's payload (see
	// taskSpawnRecord). nil on every other record type.
	TaskSpawn *taskSpawnRecord `json:"task_spawn,omitempty"`
	// TaskNotify carries a recTaskNotifyQueued/recTaskNotifyDelivered
	// record's payload (see taskNotifyRecord). nil on every other record
	// type.
	TaskNotify *taskNotifyRecord `json:"task_notify,omitempty"`
	// Usage carries the provider's per-turn Usage on the message record for
	// the assistant message ending a model turn (nil for every other
	// message: user, tool, or an interrupted partial assistant message —
	// see Session.appendWithUsage). It is the only way Session.Usage() and
	// Session.LastUsage() survive a process restart: LoadSession sums every
	// record's Usage back into cumulative usage and keeps the last one seen
	// (see issue #62 layer 2) — the log carries no separate cumulative-
	// usage record to replay instead.
	//
	// On a recCompact record, Usage instead carries the SUMMARIZATION
	// call's own spend (see compact.go's Session.Compact and docs/design/
	// context-compaction.md's "Usage accounting"): LoadSession's replay
	// adds it into cumulative usage ONLY, never into lastUsage/
	// haveLastUsage — unlike recMessage replay, which sets both. A
	// reloaded session must not report the small summarization call as its
	// "last request size", or the automatic trigger's re-fire check would
	// misread the session as small right after a reload.
	Usage *provider.Usage `json:"usage,omitempty"`
	// Compact carries a recCompact record's payload (see compactRecord).
	// nil on every other record type.
	Compact *compactRecord `json:"compact,omitempty"`
	// ToolResult carries a recToolResultRetained record's payload (see
	// toolResultRecord). nil on every other record type.
	ToolResult *toolResultRecord `json:"tool_result,omitempty"`
	// MCPTools carries a recMCPToolsSelected record's payload: the
	// namespaced tool names this record adds to the session's selected set.
	// nil on every other record type.
	MCPTools []string `json:"mcp_tools,omitempty"`
	// ClaudeCodeSessionID carries a recClaudeCodeSessionID record's
	// payload: the Claude Code CLI's own session id (see
	// Session.claudeCodeCLISessionID). Empty on every other record type.
	ClaudeCodeSessionID string `json:"claude_code_session_id,omitempty"`
	// ClaudeCodeHistoryWatermark carries a recClaudeCodeHistoryWatermark
	// record's payload (see Session.claudeCodeHistoryWatermark). Zero on
	// every other record type; also indistinguishable from an explicit
	// watermark of 0, which is harmless — persistClaudeCodeHistoryWatermark
	// is never called with 0 in practice (a delegated turn always appends
	// at least the pending trigger message before this is recorded).
	ClaudeCodeHistoryWatermark int `json:"claude_code_history_watermark,omitempty"`
	// ClaudeCodeCostUSD carries a recClaudeCodeUsage record's own
	// per-turn total_cost_usd (see Session.applyClaudeCodeUsage and
	// message.SubscriptionUsage.SessionCostUSD's own doc comment) — a
	// pointer, not a plain float64, so a record written before this field
	// existed decodes to nil (no cost accrual on replay) rather than
	// being indistinguishable from an explicit zero-cost turn written by
	// the current code, which always sets this non-nil (even to &0.0).
	ClaudeCodeCostUSD *float64 `json:"claude_code_cost_usd,omitempty"`
}

// applyGoalRecord folds one goal.* record into the durable goal state a
// resumed session restores: an active goal is one set without a later
// goal.achieved or goal.cleared, and per Claude Code semantics the run
// counters reset, so nothing else carries over. It is the ONE
// implementation of that rule, shared by LoadSession's replay and the
// session metadata index's own fold (index.go).
//
// Every other goal.* record type (goal.eval, goal.stalled,
// goal.eval_failed, goal.parked) is per-turn trace with no resume state,
// and returns the state unchanged — in particular a park never clears the
// goal, live or on replay.
func applyGoalRecord(active bool, condition string, recType string, g *goalRecord) (bool, string) {
	switch recType {
	case recGoalSet:
		active = true
		if g != nil {
			condition = g.Condition
		}
	case recGoalUpdated:
		// Only meaningful while active (see UpdateGoal): rewrites the
		// restored condition in place, same as the live path.
		if active && g != nil {
			condition = g.Condition
		}
	case recGoalAchieved, recGoalCleared:
		active, condition = false, ""
	}
	return active, condition
}

// toolResultRecord carries the durable payload of a toolresult.retained
// record (see toolresult.go's writeRetainedToolResult). It is a POINTER
// record: Handle names the sidecar file holding the actual bytes, and
// Bytes/Lines are the metadata read_tool_result reports back and the
// per-session retained-bytes ceiling is accumulated from. Tool is the name
// of the tool whose result was retained, carried so a resumed session's
// read_tool_result output can still name its source.
type toolResultRecord struct {
	Handle string `json:"handle"`
	Tool   string `json:"tool,omitempty"`
	Bytes  int    `json:"bytes,omitempty"`
	Lines  int    `json:"lines,omitempty"`
	// Head carries toolResultMeta.Head (see toolresult.go) so a resumed
	// session's compaction retained-results index (compact.go, review
	// finding F3) can still name a handle recognizably without re-opening
	// its sidecar file. Omitted (empty string) on a record written by an
	// older binary — a resumed session's index just shows no head text for
	// that one handle, never an error.
	Head string `json:"head,omitempty"`
}

// compactRecord carries the durable payload of a "compact" record (see
// compact.go's Session.Compact and docs/design/context-compaction.md §2
// "Journal shape"). Summary is the full message.Message to splice in,
// carried inline — not a lightweight marker followed by a separate
// recMessage — so a crash between two records can never leave a dangling
// reference (see §3 "Crash discipline").
type compactRecord struct {
	FirstID     string          `json:"first_id"`
	LastID      string          `json:"last_id"`
	TurnsFolded int             `json:"turns_folded"`
	Summary     message.Message `json:"summary"`
}

// goalRecord carries the durable payload of a goal.* record (see goal.go).
type goalRecord struct {
	Condition string `json:"condition,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Met       bool   `json:"met,omitempty"`
	Turn      int    `json:"turn,omitempty"`
	Turns     int    `json:"turns,omitempty"`
	// Attempt is the 1-based worker-turn retry attempt on a goal.stalled
	// record (see promptTurnWithRetry in goal.go).
	Attempt int `json:"attempt,omitempty"`
	// Retryable marks a goal.stalled record whose failure was classified as
	// provider-retryable weather (see provider.RetryableError and GitHub
	// issue #61) rather than a deterministic failure — set on every
	// retryable-class stalled record, including the final one recorded when
	// promptTurnWithRetry's retryable budget (goalRetryableMaxAttempts) is
	// exhausted. RetryableClass names the provider's classification
	// (overloaded/rate_limited/server_error, see provider.RetryableClass).
	// Waiting is true for every retryable-class stall EXCEPT that final
	// exhausted one, so a reader distinguishes "still waiting out provider
	// weather" from "gave up waiting and is parking the turn" without
	// decoding Reason text. Both are false/empty on an ordinary
	// deterministic-path stall, unchanged from before this field existed.
	Retryable      bool   `json:"retryable,omitempty"`
	RetryableClass string `json:"retryable_class,omitempty"`
	Waiting        bool   `json:"waiting,omitempty"`
	// EvalFailures carries a goal.eval_failed record's consecutive-failure
	// count (see goal.go's recordGoalEvalFailed):
	// the number of CONSECUTIVE failed evaluator boundaries as of this one,
	// inclusive, reset to zero the moment a later boundary parses a verdict
	// or the generation changes (an UpdateGoal). The terminal goal.cleared
	// record that fires once this reaches goalEvalFailureLimit never carries
	// a count itself — its Reason text names the limit instead (see
	// server/journal.go's GoalEvalFailures doc comment for the mirrored
	// server-side fold).
	EvalFailures int `json:"eval_failures,omitempty"`
	// Attempts carries a goal.parked record's total attempt count for the
	// exhausted turn (see goal.go's recordGoalParked) — distinct from
	// Attempt (singular), which is goal.stalled's 1-based PER-ATTEMPT
	// counter. Reason on a goal.parked record is deliberately CLASSIFIED
	// (see classifyGoalWorkerError), never the raw err.Error() text
	// goal.stalled/goal.eval_failed carry: a park is a durable,
	// potentially long-lived terminal an operator-facing surface (GET
	// /session's pause presentation, a dashboard) may read well after the
	// fact, so unlike those two per-attempt trace records it must never
	// echo a provider's raw error text (request IDs, endpoint URLs, or
	// other vendor-specific detail with no fixed shape). Retryable/
	// RetryableClass (above) are reused unchanged from goal.stalled's
	// convention: set on a retryable-tier park, zero/empty on a
	// deterministic-tier one.
	Attempts int `json:"attempts,omitempty"`
}

// promptRecord carries the durable payload of a prompt.queued/
// prompt.dequeued record (see queue.go). ID is the queue-assigned,
// session-monotonic
// prompt ID. Text is the queued prompt, carried on BOTH record types (not
// just prompt.queued) so a prompt.dequeued record is self-describing without
// cross-referencing the matching prompt.queued one earlier in the log.
// Reason is empty on prompt.queued and one of "delivered"/"injected"/
// "cleared" on prompt.dequeued (see DequeuePrompt/dequeueAllLocked).
type promptRecord struct {
	ID     int64  `json:"id,omitempty"`
	Text   string `json:"text,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Seq is the caller-issued idempotency sequence carried on a
	// prompt.queued record written by EnqueuePromptDurable (see queue.go);
	// 0/omitted on plain EnqueuePrompt records and on every
	// prompt.dequeued. LoadSession folds it into the session's enqueueSeq
	// high-water mark and dedupes same-seq records last-writer-wins — see
	// the recPromptQueued replay case for why that heals torn fsync
	// failures.
	Seq int64 `json:"seq,omitempty"`
	// MessageID is the resolved ID (see ResolveMessageID) the queued
	// prompt's eventual user message will carry — set on prompt.queued,
	// carried through to QueuedPrompt.MessageID by promptQueueFold.queued
	// on replay. Omitted (empty) on a record written before this field
	// existed; PromptWithOrigin's own mint site resolves that case exactly
	// like any other unset id, at dispatch time — a backward-compatible
	// fallback, not a replay error.
	MessageID string `json:"message_id,omitempty"`
	// Blobs are the queued prompt's attachments (see QueuedPrompt.Blobs),
	// written on prompt.queued ONLY — never on prompt.dequeued, unlike Text
	// above. A dequeued record exists to name which queue entry left the
	// queue, and it is matched by ID (promptQueueFold.dequeued), so copying
	// an image's bytes into it would double every attachment in the journal
	// for no reader.
	//
	// Omitted (nil) on every text-only prompt and on every record written
	// before prompt attachments existed, which folds back to a QueuedPrompt
	// with no attachments — the pre-feature behavior exactly, not a replay
	// error.
	Blobs []*message.Blob `json:"blobs,omitempty"`
}

// taskSpawnRecord is a recTaskSpawned record's payload — see that
// constant's own doc comment. Pure audit trail: which child, spawned as
// which agent type.
type taskSpawnRecord struct {
	ChildID string `json:"child_id,omitempty"`
	Agent   string `json:"agent,omitempty"`
}

// taskNotifyRecord is a recTaskNotifyQueued/recTaskNotifyDelivered
// record's payload — see those constants' own doc comment. Mirrors
// taskNotification (engine/taskdelivery.go) field-for-field: the SAME
// content a live queued notification carries, so LoadSession's fold can
// reconstruct an outstanding one exactly as it originally arrived (see
// the recTaskNotifyQueued replay case), and a recTaskNotifyDelivered
// record is fully self-describing on its own (mirroring promptRecord's
// identical "Text carried on both record types" reasoning) without
// cross-referencing the matching queued record earlier in the log.
type taskNotifyRecord struct {
	ChildID    string        `json:"child_id,omitempty"`
	Agent      string        `json:"agent,omitempty"`
	Status     SessionStatus `json:"status,omitempty"`
	Result     string        `json:"result,omitempty"`
	FailReason string        `json:"fail_reason,omitempty"`
	// FailKind mirrors taskNotification.FailKind (see its own doc
	// comment, taskdelivery.go): the structured classification a parent
	// branches on. Carried durably so a reloaded parent — or a re-adopted
	// child restored through restoreKnownStatusLocked — reports the same
	// kind a live one would. A legacy record with no fail_kind restores
	// "", exactly the ordinary-failure value.
	FailKind string `json:"fail_kind,omitempty"`
	// FailHint mirrors taskNotification.RecoverHint: the provider's own
	// recover-at statement, carried durably because the guidance line a
	// parent reads is the only place that time is stated (see
	// exhaustionReason). A legacy record with no fail_hint restores "",
	// and the guidance simply names no time — the same rendering a wall
	// with no parseable hint already produces.
	FailHint string         `json:"fail_hint,omitempty"`
	Usage    provider.Usage `json:"usage,omitzero"`
	// Canceled mirrors taskNotification.Canceled (see its own doc
	// comment, taskdelivery.go) — carried on every record type this
	// struct backs, though only recTaskOutcomeCommitted's own fold below
	// actually reads it back: that is the one record type
	// restoreKnownStatusLocked/recoverInterruptedTurnLocked restore a
	// node's status from.
	Canceled bool `json:"canceled,omitempty"`
}

// SessionInfo summarizes one persisted session for listings.
type SessionInfo struct {
	ID        string
	CreatedAt time.Time
	Messages  int
	// Usage is cumulative token usage summed from every message record's
	// optional Usage (see record.Usage, persistMessage). It comes from the
	// session's metadata index (index.go), like every other field here —
	// no full LoadSession/message.Message replay required (issue #62 layer
	// 2: GET /session/status needs this without paying for a full session
	// load per entry).
	Usage provider.Usage
	// LastInputTokens is the input-token count of the most recent message
	// record carrying Usage (0 if none do).
	LastInputTokens int
}

// addUsage accumulates one record's usage into a listing summary.
func (info *SessionInfo) addUsage(u provider.Usage) {
	info.Usage.InputTokens += u.InputTokens
	info.Usage.OutputTokens += u.OutputTokens
	info.Usage.CacheReadTokens += u.CacheReadTokens
	info.Usage.CacheWriteTokens += u.CacheWriteTokens
}

// finalRecordComplete reports whether a journal's LAST line was completely
// written, by the only definition that matters: it decodes as a record —
// the type the writer marshals, and so the definition of the format.
//
// Every reader must ask this ONE question about a final line, because a
// crash can leave it half-written and each reader would otherwise invent
// its own tolerance from whatever subset of fields it happens to decode.
// The index's fold reads a narrower shape (indexRecord) that ignores most
// of a record's fields, so a final line with a malformed tool_result,
// mcp_tools, or task_tool_names value passes that shape while LoadSession
// drops it — and the index would then count a message the session itself
// does not have, which is the whole class of disagreement the message-page
// work exists to prevent.
//
// It is deliberately NOT used for a non-final line. There, a line that
// fails this check is corruption mid-file: LoadSession refuses the session
// outright, and a reader that folds anyway is offering a degraded view of
// an unloadable journal rather than miscounting a loadable one.
func finalRecordComplete(raw []byte) bool {
	var rec record
	return json.Unmarshal(bytes.TrimSpace(raw), &rec) == nil
}

func sessionPath(dir, id string) string {
	return filepath.Join(dir, id+".jsonl")
}

// PersistErr returns the most recent persistence failure, or nil. Write
// errors never crash the agent loop; callers decide what to do with them.
func (s *Session) PersistErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPersistErr
}

// Persist forces the session log to exist on disk now (header plus a model
// record), rather than waiting for the first message append. NewSession creates
// the log lazily, so a session that is created but never prompted has no
// on-disk backing; callers that must be able to reload such a session — the
// serve API, which may evict an idle session from memory — call Persist to give
// it durable state. It is a no-op when SessionDir is empty or the log already
// exists, and is safe to call repeatedly.
func (s *Session) Persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.SessionDir == "" {
		return nil
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return err
	}
	return nil
}

// persistMessage appends a message record to the session log, carrying
// usage (nil for every message except the assistant message ending a model
// turn — see appendWithUsage). Caller holds s.mu.
func (s *Session) persistMessage(m *message.Message, usage *provider.Usage) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recMessage, Message: m, Usage: usage}); err != nil {
		s.lastPersistErr = err
	}
}

// persistModel appends a model record to the session log. It is a no-op
// until the log exists (lazy creation: nothing is written before the first
// message append). Caller holds s.mu.
func (s *Session) persistModel(ref message.ModelRef) {
	if s.cfg.SessionDir == "" || !s.logStarted {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recModel, Model: ref}); err != nil {
		s.lastPersistErr = err
	}
}

// persistMCPToolsSelected appends an mcp.tools_selected record naming the
// tools that just entered the selected set. It mirrors persistModel and
// persistEffort exactly: a no-op until the log exists (lazy creation),
// caller holds s.mu.
//
// Empty input writes nothing, which is what keeps a repeat select -- every
// name already in the set -- from appending a record that would restore
// nothing new.
func (s *Session) persistMCPToolsSelected(names []string) {
	if len(names) == 0 {
		return
	}
	if s.cfg.SessionDir == "" || !s.logStarted {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recMCPToolsSelected, MCPTools: names}); err != nil {
		s.lastPersistErr = err
	}
}

// persistEffort appends an effort record to the session log. It mirrors
// persistModel exactly: a no-op until the log exists (lazy creation), caller
// holds s.mu.
func (s *Session) persistEffort(e message.Effort) {
	if s.cfg.SessionDir == "" || !s.logStarted {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recEffort, Effort: e}); err != nil {
		s.lastPersistErr = err
	}
}

// persistServiceTier appends a service_tier record to the session log. It
// mirrors persistEffort exactly: a no-op until the log exists (lazy
// creation), caller holds s.mu.
func (s *Session) persistServiceTier(tier string) {
	if s.cfg.SessionDir == "" || !s.logStarted {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recServiceTier, ServiceTier: tier}); err != nil {
		s.lastPersistErr = err
	}
}

// persistClaudeCodeSessionID appends a claude_code.session_id record to the
// session log. It mirrors persistModel/persistEffort exactly: a no-op
// until the log exists (lazy creation), caller holds s.mu.
func (s *Session) persistClaudeCodeSessionID(id string) {
	if s.cfg.SessionDir == "" || !s.logStarted {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recClaudeCodeSessionID, ClaudeCodeSessionID: id}); err != nil {
		s.lastPersistErr = err
	}
}

// persistClaudeCodeHistoryWatermark appends a
// claude_code.history_watermark record to the session log. It mirrors
// persistClaudeCodeSessionID exactly: a no-op until the log exists (lazy
// creation), caller holds s.mu.
func (s *Session) persistClaudeCodeHistoryWatermark(n int) {
	if s.cfg.SessionDir == "" || !s.logStarted {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recClaudeCodeHistoryWatermark, ClaudeCodeHistoryWatermark: n}); err != nil {
		s.lastPersistErr = err
	}
}

// persistClaudeCodeUsage appends a claude_code.usage record to the session
// log, carrying both the turn's token usage and its own costUSD (see
// record.ClaudeCodeCostUSD's own doc comment). It mirrors persistModel/
// persistEffort exactly: a no-op until the log exists (lazy creation),
// caller holds s.mu.
func (s *Session) persistClaudeCodeUsage(usage provider.Usage, costUSD float64) {
	if s.cfg.SessionDir == "" || !s.logStarted {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recClaudeCodeUsage, Usage: &usage, ClaudeCodeCostUSD: &costUSD}); err != nil {
		s.lastPersistErr = err
	}
}

// persistGoalLocked appends a goal.* record to the session log. It forces the
// log to exist (a goal.set may be the first thing written to a fresh session).
// Caller holds s.mu.
func (s *Session) persistGoalLocked(recType string, g goalRecord) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recType, Goal: &g}); err != nil {
		s.lastPersistErr = err
	}
}

// persistPromptQueueLocked appends a prompt.queued or prompt.dequeued record
// to the session log (see queue.go's EnqueuePrompt/DequeuePrompt). It forces
// the log to exist — a prompt.queued may be the first thing ever written to
// a fresh session — mirroring persistGoalLocked exactly. Caller holds s.mu.
//
// Drains s.deferredQueueRecords FIRST, so a record parked by
// queueRecordDeferredLocked always reaches disk before any prompt-queue
// record written after the memory mutation it belongs to — see that
// method's own doc comment for the resurrection defect this ordering
// prevents.
func (s *Session) persistPromptQueueLocked(recType string, p promptRecord) {
	s.flushQueueRecordsLocked()
	s.writePromptQueueRecordLocked(recType, p)
}

// writePromptQueueRecordLocked is persistPromptQueueLocked's write half,
// with NO deferred-record drain — the one path both
// persistPromptQueueLocked and flushQueueRecordsLocked write through, so a
// drain can never recurse into another drain. Caller holds s.mu.
func (s *Session) writePromptQueueRecordLocked(recType string, p promptRecord) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recType, Prompt: &p}); err != nil {
		s.lastPersistErr = err
	}
}

// persistTaskSpawnLocked appends a task.spawned record to the session
// log (see SessionManager.Spawn's own call site) — mirrors
// persistPromptQueueLocked exactly, on the PARENT's log. Caller holds
// s.mu.
func (s *Session) persistTaskSpawnLocked(childID, agent string) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recTaskSpawned, TaskSpawn: &taskSpawnRecord{ChildID: childID, Agent: agent}}); err != nil {
		s.lastPersistErr = err
	}
}

// persistTaskNotifyLocked appends a task.notify_queued or
// task.notify_delivered record to the session log (see
// enqueueTaskNotification/commitTaskNotifications in taskdelivery.go) —
// mirrors persistPromptQueueLocked exactly. Caller holds s.mu.
//
// UPDATE: the m.mu-contention finding this comment used to describe here
// is fixed. It no longer applies to this method's two tree-delivery call
// sites — finalizeTurn and recoverInterruptedTurnLocked — which used to
// call this synchronously (via persistQueuedTaskNotification/
// persistDeliveredTaskNotifications) while SessionManager.mu, the single
// lock guarding every session in the tree, was held. Both now go through
// SessionManager.deferPersist/unlockAndFlushPersist instead (see that
// mechanism's own doc comment, session_manager.go): the in-memory queue
// mutation still happens under m.mu, exactly as before (preserving the
// queued-minus-delivered durability guarantee's atomicity with the
// caller's OTHER in-memory bookkeeping under that same lock), but the
// actual disk write is queued as a thunk and only runs once m.mu has
// already been released — closing the "a slow or contended disk on one
// session's notification stalls Info/Reap/Spawn/finalize for every OTHER
// session" exposure without reopening a durability window, exactly the
// "getting both properties" goal this comment used to call out as
// deliberately deferred future work.
//
// ReportTurnStart's own migration loop was never actually part of this
// problem in the first place, despite an earlier version of this comment
// listing it alongside the two callers above: it goes through
// enqueueTaskNotificationMigrated, which is memory-only and never calls
// this method at all — the notification's original recTaskNotifyQueued
// record, from whichever earlier enqueue actually persisted it, already
// backs it durably.
//
// The one remaining caller that DOES still run this synchronously,
// commitTaskNotifications, was likewise never part of the m.mu problem:
// it is invoked from a live turn's own per-session code path (streamTurn/
// runAgenticLoop, engine.go), holding only s.mu — a per-session lock,
// never SessionManager's tree-wide m.mu — so it was never in a position
// to stall any OTHER session's Info/Reap/Spawn/finalize call to begin
// with.
func (s *Session) persistTaskNotifyLocked(recType string, n taskNotification) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	rec := taskNotifyRecord{ChildID: n.ChildID, Agent: n.Agent, Status: n.Status, Result: n.Result, FailReason: n.FailReason, FailKind: n.FailKind, FailHint: n.RecoverHint, Usage: n.Usage, Canceled: n.Canceled}
	if err := s.writeRecord(record{Type: recType, TaskNotify: &rec}); err != nil {
		s.lastPersistErr = err
	}
}

// persistToolResultRetainedLocked appends a toolresult.retained pointer
// record to the session log (see toolresult.go's writeRetainedToolResult).
// It forces the log to exist, mirroring persistGoalLocked: a retention can
// land before any message has been persisted in a session created
// mid-turn. Best-effort like every other persist path here — a write
// failure lands in lastPersistErr and never fails the tool call, since the
// sidecar file is already written and the only cost of a lost record is a
// handle that a FUTURE process cannot resolve. Caller holds s.mu.
func (s *Session) persistToolResultRetainedLocked(m toolResultMeta) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	rec := record{
		Type: recToolResultRetained,
		ToolResult: &toolResultRecord{
			Handle: m.Handle,
			Tool:   m.Tool,
			Bytes:  m.Bytes,
			Lines:  m.Lines,
			Head:   m.Head,
		},
	}
	if err := s.writeRecord(rec); err != nil {
		s.lastPersistErr = err
	}
}

// persistCompactLocked appends a compact record to the session log: one
// json.Marshal, one Write call, exactly like every other record (see
// docs/design/context-compaction.md §3 "Crash discipline" — a torn write
// degrades to "compaction never happened", never a partially-spliced or
// ambiguous history). Caller holds s.mu and has already spliced s.history.
func (s *Session) persistCompactLocked(firstID, lastID string, turnsFolded int, summary message.Message, usage provider.Usage) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	rec := record{
		Type:      recCompact,
		CreatedAt: summary.CreatedAt,
		Usage:     &usage,
		Compact: &compactRecord{
			FirstID:     firstID,
			LastID:      lastID,
			TurnsFolded: turnsFolded,
			Summary:     summary,
		},
	}
	if err := s.writeRecord(rec); err != nil {
		s.lastPersistErr = err
	}
}

// storePhase reports elapsed as a completed op/phase to Config.OnStorePhase,
// nil-guarded. Caller holds s.mu (see OnStorePhase's doc comment).
func (s *Session) storePhase(op, phase string, elapsed time.Duration) {
	if s.cfg.OnStorePhase != nil {
		s.cfg.OnStorePhase(op, phase, elapsed)
	}
}

// storePhaseStart reports op/phase beginning to Config.OnStorePhaseStart,
// nil-guarded. Caller holds s.mu (see OnStorePhaseStart's doc comment).
// Prefer timedStorePhase over calling this directly — see its doc comment
// for why a hand-paired storePhaseStart/storePhase at each call site is the
// wrong shape.
func (s *Session) storePhaseStart(op, phase string) {
	if s.cfg.OnStorePhaseStart != nil {
		s.cfg.OnStorePhaseStart(op, phase)
	}
}

// timedStorePhase runs fn as one instrumented op/phase: storePhaseStart
// immediately before fn runs, storePhase immediately after fn returns —
// success OR error — with the elapsed time fn actually took either way.
// This is the ONLY call shape used below (ensureLog, EnqueuePromptDurable):
// a hand-paired storePhaseStart-then-later-storePhase around each op used
// to let an early `return err` on the operation's own failure skip the
// matching storePhase call, leaving the watchdog's in-flight table (see
// cmd/harness/main.go) with an entry that nothing would ever clear —
// exactly the failure mode an I/O error (EIO, ENOSPC) hits on a wedged
// volume, producing a permanent false "still stuck" warning for a phase
// that in fact failed and returned promptly. Routing every phase through
// this one helper makes "exactly one completion per start, on every path"
// structural rather than a rule each call site has to remember. Caller
// holds s.mu (same rule as storePhase/storePhaseStart).
func (s *Session) timedStorePhase(op, phase string, fn func() error) error {
	s.storePhaseStart(op, phase)
	t0 := time.Now()
	err := fn()
	s.storePhase(op, phase, time.Since(t0))
	return err
}

// volumeSync reports whether Config.SessionSync selects "volume" mode (see
// its doc comment): ensureLog's directory fsync and EnqueuePromptDurable's
// file fsync are both skipped entirely in this mode — no syscall, no phase
// event — rather than merely fast-pathed, since on some FUSE/9p transports
// the fsync call itself (particularly fsync(dirfd)) is what deadlocks the
// whole mount permanently. Any value other than SessionSyncVolume
// (including the zero value) is fsync mode; config.Config.SessionSync is
// the single validation point for the string, so an unrecognized value
// reaching here defaults to the safe (fsync) behavior rather than erroring.
func (s *Session) volumeSync() bool {
	return s.cfg.SessionSync == SessionSyncVolume
}

// ensureLog opens the session log, creating the directory and file — and
// writing the header — on first use. Caller holds s.mu. The fast path (log
// already open) reports no phases.
func (s *Session) ensureLog() error {
	if s.logFile != nil {
		return nil
	}
	const op = "ensure_log"
	if err := s.timedStorePhase(op, "mkdir", func() error {
		return os.MkdirAll(s.cfg.SessionDir, 0o755)
	}); err != nil {
		return err
	}
	// O_RDWR, not O_WRONLY: the torn-tail repair below needs to ReadAt the
	// file's own last byte. O_APPEND still governs every Write (here and in
	// writeRecord) regardless of the file's read/write position, so this
	// adds read capability without changing append semantics at all.
	var f *os.File
	if err := s.timedStorePhase(op, "open", func() error {
		var err error
		f, err = os.OpenFile(sessionPath(s.cfg.SessionDir, s.ID), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
		return err
	}); err != nil {
		return err
	}
	var fi os.FileInfo
	if err := s.timedStorePhase(op, "stat", func() error {
		var err error
		fi, err = f.Stat()
		return err
	}); err != nil {
		f.Close()
		return err
	}
	s.logFile = f
	size := fi.Size()
	// A prior process can have crashed mid-write, leaving the file not
	// ending in '\n'. Resuming WRITES onto that file is hazardous in a way
	// resuming READS is not: appending a new record directly after it, with
	// no separating newline, concatenates the two into ONE line ("{{"...),
	// which is itself unparseable — silently dropping the new record for as
	// long as it stays the last line (despite EnqueuePromptDurable having
	// returned a nil, durability-attesting error for it — an attestation
	// hole this closes), and then becoming a HARD load error the moment any
	// later record makes it no longer last (scanLog below only tolerates a
	// corrupt FINAL line; a corrupt non-final one is an error, never a
	// silent drop), poisoning the whole session. This was reachable even
	// when the missing-newline tail was the very first (and only) bytes
	// ever written — e.g. a crash after 1 byte of this function's own
	// header+model write below, before this repair existed.
	//
	// A missing trailing '\n' has TWO different honest causes, and they
	// need opposite repairs:
	//
	//  1. The write was torn mid-record (crash before the content itself
	//     finished landing) — the tail is not valid JSON. scanLog's
	//     documented tolerance already decided such a tail "never happened"
	//     (a corrupt/incomplete FINAL line is silently dropped), so the
	//     correct repair is to TRUNCATE back to just after the last '\n'
	//     (0 if there is none at all), making the file ON DISK agree
	//     byte-for-byte with what a load already treats it as meaning.
	//  2. The record content itself completed and is valid JSON, but the
	//     single trailing '\n' that terminates it never landed (e.g. a
	//     crash between the content write and the newline, or — as the
	//     rapid model test's TornCrashReload found — a byte-exact
	//     truncation that happens to land exactly on that newline). This
	//     tail is NOT what scanLog's tolerance is for: scanLog decides
	//     "torn" purely by whether the line parses, so it loads this record
	//     just fine despite the missing newline. Truncating it away here
	//     would silently destroy an already-durable, already-loadable
	//     record — a worse violation than the one being fixed. The correct
	//     repair is to APPEND the missing '\n', preserving the record and
	//     terminating it so the next write cannot concatenate onto it.
	//
	// Distinguishing the two means replicating scanLog's own rule — attempt
	// to parse the tail — rather than a cheaper newline-only heuristic.
	if size > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], size-1); err != nil {
			f.Close()
			s.logFile = nil
			return err
		}
		if last[0] != '\n' {
			if err := s.timedStorePhase(op, "tail_repair", func() error {
				data, err := os.ReadFile(sessionPath(s.cfg.SessionDir, s.ID))
				if err != nil {
					return err
				}
				tailStart := bytes.LastIndexByte(data, '\n') + 1 // 0 if no newline at all
				tail := bytes.TrimSpace(data[tailStart:])
				var rec record
				if len(tail) > 0 && json.Unmarshal(tail, &rec) == nil {
					// Case 2: complete, valid record — just terminate it.
					if _, err := f.Write([]byte("\n")); err != nil {
						return err
					}
					size++
				} else {
					// Case 1: genuinely torn (or trailing whitespace with no
					// record at all) — truncate the incomplete tail away.
					if err := f.Truncate(int64(tailStart)); err != nil {
						return err
					}
					size = int64(tailStart)
				}
				return nil
			}); err != nil {
				f.Close()
				s.logFile = nil
				return err
			}
		}
	}
	if size == 0 {
		// Header plus a model record for the session's current model, so
		// every persisted session names its model explicitly — a SetModel
		// before the first append would otherwise be silently lost and
		// LoadSession would wrongly fall back to Config.Model.
		//
		// Both records go out in ONE Write call: written separately, a
		// transient failure after the header would leave a non-empty file
		// that retries (gated on size == 0) never complete, permanently
		// dropping the model record. With a single write the worst case
		// under a mid-write crash is a truncated final line, which
		// LoadSession already tolerates.
		var buf bytes.Buffer
		headerRecs := []record{
			{Type: recSession, ID: s.ID, CreatedAt: s.createdAt, WorkDir: s.cfg.WorkDir, ParentSession: s.cfg.ParentSession, TaskParentID: s.cfg.TaskParentID, TaskAgentType: s.cfg.TaskAgentType, TaskToolNames: taskToolNamesPtr(s.cfg.TaskToolNames), TaskDepth: s.cfg.TaskDepth, Effort: s.effort, ServiceTier: s.serviceTier},
			{Type: recModel, Model: s.model},
		}
		// A selection made before the log existed has no other durable
		// carrier: persistMCPToolsSelected no-ops until logStarted, and
		// unlike model/effort the header record has no selected-tools
		// slot. Capture the live set here, exactly as the header captures
		// the live model and effort, so a pre-log select survives reload.
		if len(s.mcpSelected) > 0 {
			names := make([]string, 0, len(s.mcpSelected))
			for name := range s.mcpSelected {
				names = append(names, name)
			}
			sort.Strings(names)
			headerRecs = append(headerRecs, record{Type: recMCPToolsSelected, MCPTools: names})
		}
		for _, rec := range headerRecs {
			b, err := json.Marshal(rec)
			if err != nil {
				f.Close()
				s.logFile = nil
				return err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		if err := s.timedStorePhase(op, "header_write", func() error {
			_, err := f.Write(buf.Bytes())
			return err
		}); err != nil {
			f.Close()
			s.logFile = nil
			return err
		}
		// The header records bypass writeRecord (they go out in ONE Write,
		// see above), so fold them here — the metadata index must see
		// every record the journal holds, starting with the header that
		// names the session at all.
		size += int64(buf.Len())
		for _, rec := range headerRecs {
			s.index.applyIndexRecordBestEffort(indexRecordOf(rec), false)
		}
		// Same reason the fold is applied here: these records bypass
		// writeRecord, so the snapshot anchor must count them here or every
		// seq this session ever takes is short by the header's length.
		s.recordsWritten += int64(len(headerRecs))
		// A file fsync (as EnqueuePromptDurable does before attesting
		// durability — see queue.go) commits the file's *contents* but not
		// its directory entry: POSIX leaves the entry itself up to the
		// containing directory's own fsync. On a fresh log file, that entry
		// only just got created above, so without this the durable-enqueue
		// attestation is a lie on the first record after creation — the
		// enqueue's file fsync can return clean, the response go out, and a
		// crash before the directory entry is committed can lose both the
		// message and the watermark on some filesystems (e.g. ext4). Doing
		// it here, once per file creation rather than once per record, is
		// enough: later records reuse this already-linked file.
		//
		// This syncs the log file's entry within SessionDir, not SessionDir's
		// own entry in its parent — SessionDir is assumed to be a preexisting
		// mount (e.g. a volume) at boot, so that entry predates the process
		// and isn't this code's concern. See syncDir for why this is a
		// build-tagged no-op off unix.
		//
		// Skipped entirely in volume mode (see volumeSync): a continuously-
		// synced network volume's own commit layer is the documented
		// durability boundary there, so this fsync would add nothing except
		// the risk of joining it on a transport where fsync(dirfd) deadlocks
		// the mount permanently — see Config.SessionSync's doc comment.
		// Skipping the call is not enough on its own if it never returns; the
		// point is not issuing the syscall at all. No phase event fires
		// either, so the watchdog never carries a misleading sync_dir entry
		// for a phase that, in this mode, does not exist.
		if !s.volumeSync() {
			if err := s.timedStorePhase(op, "sync_dir", func() error {
				return syncDir(s.cfg.SessionDir)
			}); err != nil {
				f.Close()
				s.logFile = nil
				return err
			}
		}
	}
	s.logStarted = true
	// A fold marked broken by a failed record write is RE-SEEDED here, from
	// the journal as the repair above left it. Without this, one transient
	// write failure disabled the index for the rest of the session object's
	// life: every later read of that session refolded the whole journal,
	// which is the cost the index exists to remove. A review caught it.
	//
	// Re-seed, never merely clear the flag. That distinction is the whole
	// correctness argument, and a maintainer who "simplifies" this to
	// `s.index.broken = false` reintroduces a silent wrong-index bug. A
	// failed Write can land the record's bytes and not its trailing
	// newline. The tail repair above then takes its case-2 branch: the tail
	// parses, so it terminates the record and KEEPS it. The fold never saw
	// that record. Clearing the flag would resume flushing a sidecar that
	// is short by one message while claiming, through logSize, to cover the
	// whole file — a stale index that reads as current. Folding the file
	// again is what makes the fold agree with the bytes on disk, whichever
	// branch the repair took.
	//
	// This runs on a reopen, which a failed write forces (see writeRecord),
	// so it costs one slim fold per failure rather than per record. A fold
	// that fails again leaves broken set, exactly as before.
	if s.index.broken {
		if data, rerr := os.ReadFile(sessionPath(s.cfg.SessionDir, s.ID)); rerr == nil {
			if reseeded, ferr := foldJournalBytes(data); ferr == nil {
				s.index = reseeded
			}
		}
	}
	// The sidecar index handle is opened beside the log, once, and rewritten
	// in place from then on (see writeIndexTo). A failure to open it is
	// never a session failure: the index is a cache, and a reader that
	// cannot find one refolds the journal.
	if s.indexFile == nil {
		if idxf, err := os.OpenFile(sessionIndexPath(s.cfg.SessionDir, s.ID), os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			s.indexFile = idxf
		} else {
			s.lastIndexErr = err
		}
	}
	// size is the journal length after any tail repair above, which is
	// exactly the bytes the index fold covers: a repair that TRUNCATED a
	// torn tail dropped a record scanLog never folded either, and a repair
	// that only terminated a complete record added one byte to a record
	// the fold already holds.
	s.logSize = size
	// Flush now, not only after the first record: a session that is
	// created and persisted but never prompted (Session.Persist, which the
	// serve API calls so an evicted session can be reloaded) must still
	// answer GET /session/{id} from its index.
	s.flushIndexLocked()
	return nil
}

// ReleaseFiles closes the session's log and sidecar-index handles and drops
// them. The session stays fully usable: the next persist call re-enters
// ensureLog, which reopens both, repairs a torn tail if one is there, and
// continues appending. Nothing in memory changes, so a caller can release a
// session it may still use.
//
// It exists because a Session holds two descriptors for its whole life, and
// a server keeps one Session per session it has touched. A long-lived box
// with many subagent sessions accumulates them. The server calls this when
// it evicts a session from residency (evictResidentLocked), which is the
// point it has already decided the session is idle and can be reloaded from
// disk.
//
// Errors are dropped on purpose: a close failure on a handle being
// discarded tells a caller nothing it can act on, and the next ensureLog
// reopens from the path regardless.
func (s *Session) ReleaseFiles() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Eviction is the on-idle trigger's other half (docs/design/journal-
	// snapshotting.md §4.6): the caller has already decided this session
	// is idle and will be reloaded from disk, so checkpointing now is
	// exactly what makes that reload cheap. Background, coalesced, and a
	// no-op when nothing has been written since the last snapshot — see
	// snapshot.go.
	s.snapshotIdleLocked()
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile = nil
	}
	if s.indexFile != nil {
		s.indexFile.Close()
		s.indexFile = nil
	}
}

// writeRecord marshals one record and appends it as a line. Caller holds
// s.mu and has called ensureLog.
//
// It is also the session's single record choke point, so it is where the
// metadata index folds (see index.go): every durable record, from every
// persist path, passes here exactly once. A failed write folds nothing and
// advances no byte counter — it marks the fold broken instead, so this
// session stops writing a sidecar that could claim to summarize a record it
// never saw. The next reader refolds the journal from byte 0.
func (s *Session) writeRecord(rec record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	n, err := s.logFile.Write(append(b, '\n'))
	if err != nil {
		// The file may have grown by a partial line. Two things follow.
		//
		// The fold and logSize both stay put, and the fold is marked
		// broken, so this session never again writes a sidecar that could
		// claim to summarize a record it did not see. Readers refold.
		//
		// The handle is closed, so the next persist call re-enters
		// ensureLog instead of returning at its fast path. That is what
		// runs the torn-tail repair over the partial line. Without it the
		// next append concatenates onto that line with no separator, and
		// the pair becomes a hard load error as soon as any later record
		// makes it non-final — a retry of a failed EnqueuePromptDurable
		// could poison the whole session log.
		s.index.broken = true
		s.logFile.Close()
		s.logFile = nil
		return err
	}
	s.logSize += int64(n)
	// The journal head advanced by exactly one record, so the snapshot
	// anchor does too (see Session.recordsWritten). Only a record that
	// actually landed counts: the failed-write branch above returns before
	// this line, so a torn line can never be named by a snapshot's seq.
	s.recordsWritten++
	s.index.applyIndexRecordBestEffort(indexRecordOf(rec), false)
	s.flushIndexLocked()
	// Deliberately NO snapshot trigger here, though this is the single
	// record choke point and so the obvious place for one. A snapshot
	// captures MEMORY and anchors it to a JOURNAL POSITION, and inside
	// this function the two do not yet agree: several callers persist
	// their record BEFORE they apply their own in-memory mutation
	// (EnqueuePromptDurable, deliberately — see queue.go), so a capture
	// taken here would anchor past a record whose effect memory has not
	// applied, and the reload would skip that record and lose the effect
	// permanently. The trigger lives at the append boundary instead (see
	// Session.maybeSnapshotLocked call sites), where the caller has
	// completed both halves.
	return nil
}

// flushIndexLocked writes the session's sidecar metadata index for the
// journal as it now stands (see index.go). Best effort by design: the index
// is a memoized fold, and a reader that finds it missing, torn, or stale
// refolds the journal instead. Caller holds s.mu.
func (s *Session) flushIndexLocked() {
	if s.indexFile == nil || s.logFile == nil {
		return
	}
	// The journal's modification time is half the staleness key (see
	// SessionIndex.LogModTime), and it must be read AFTER the record write
	// this flush follows. One fstat on a handle already open.
	fi, err := s.logFile.Stat()
	if err != nil {
		s.lastIndexErr = err
		return
	}
	ix, ok := s.index.snapshot(s.logSize, fi.ModTime())
	if !ok {
		return
	}
	if err := writeIndexTo(s.indexFile, ix); err != nil {
		s.lastIndexErr = err
	}
}

// taskToolNamesPtr converts a Config.TaskToolNames value into the pointer
// record.TaskToolNames marshals — see that field's own doc comment for
// why a pointer, not a plain slice: nil stays nil (omitted on write,
// exactly like every other unset restriction field), and a non-nil slice
// — including an empty one, the whole point of this indirection — becomes
// a pointer to a local copy, so a later mutation of names (there isn't
// one today, but nothing here should rely on that) can't retroactively
// change what was already marshaled.
func taskToolNamesPtr(names []string) *[]string {
	if names == nil {
		return nil
	}
	// make(…, len(names)), not append([]string(nil), names...): append
	// with zero elements to append returns its nil starting slice
	// UNCHANGED (a genuine Go append quirk) — for a non-nil, LEN-ZERO
	// names (exactly the case this whole indirection exists to preserve
	// — see this field's own doc comment), that silently collapsed cp
	// back to nil, marshaling as JSON null and reproducing the identical
	// bug one layer down. make(...) always returns a non-nil slice, even
	// at length zero.
	cp := make([]string, len(names))
	copy(cp, names)
	return &cp
}

// ErrInvalidSessionID is returned (wrapped) by LoadSession when id fails
// ValidSessionID's two-format rule. It is checked before sessionPath ever
// builds a filesystem path, so a path-traversal-shaped id (e.g.
// "../../etc/passwd") is rejected without touching disk — defense in depth
// alongside the HTTP boundary's own ValidSessionID check (server/handlers.go),
// since not every caller (e.g. the CLI's -r/-c resume flags) goes through
// that boundary.
var ErrInvalidSessionID = errors.New("engine: invalid session id")

// LoadSession rebuilds a session from its log file: history and current
// model (the last model record wins; Config.Model otherwise), preserving the
// session ID. Subsequent appends continue the same file.
//
// A corrupt or incomplete final line (crash mid-write) is ignored; a corrupt
// line anywhere else is an error.
func LoadSession(cfg Config, id string) (*Session, error) {
	if cfg.SessionDir == "" {
		return nil, errors.New("engine: LoadSession requires Config.SessionDir")
	}
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	data, err := os.ReadFile(sessionPath(cfg.SessionDir, id))
	if err != nil {
		return nil, err
	}

	s := newSession(cfg)
	s.ID = id
	s.logStarted = true

	// The journal head, in LINES. It is the domain the snapshot anchor
	// lives in (see Session.recordsWritten) and it costs a byte scan, no
	// decoding. It can exceed the number of records the scan below
	// actually applies by one, when a crash mid-write left a torn final
	// line; the loop corrects s.recordsWritten to the last line genuinely
	// applied, so a session resumed over a torn tail keeps taking anchors
	// that agree with the line numbers a later load will see.
	head := countJournalRecords(data)

	// Snapshot-aware recovery (snapshot.go and docs/design/journal-
	// snapshotting.md §4.3). startAfter is the last journal line a valid
	// snapshot covers — 0 when there is no usable one, which is a full
	// replay and exactly the behavior this function had before snapshots
	// existed. snapshotStartAfter has already applied the session header
	// and restored the snapshot's state by the time it returns non-zero.
	startAfter := s.snapshotStartAfter(cfg.SessionDir, id, data, head)
	if startAfter > 0 {
		s.snapshotSeq = startAfter
		// The metadata index (index.go) is a fold of EVERY record, and
		// this load is deliberately not going to see most of them. Mark it
		// broken rather than build a partial fold that would flush a
		// sidecar claiming to summarize the whole journal while describing
		// its tail. The index is a cache with no repair path: a reader
		// that finds none refolds, and this session's next write re-seeds
		// the fold from the journal in ensureLog. Snapshotting the index
		// fold itself is a possible follow-up; nothing here may guess at
		// it.
		s.index.broken = true
	}

	// The prompt queue folds through promptQueueFold (queue.go), seeded
	// from this fresh session's own counters — or from the snapshot's
	// restored queue, so the tail's prompt.queued/prompt.dequeued records
	// fold onto the state the snapshot already holds — and written back
	// after the scan. It is the same fold the metadata index uses, so the
	// two can never drift on the torn-write and ID-burn rules it holds.
	qf := promptQueueFold{queue: s.promptQueue, nextID: s.promptQueueNextID, seq: s.enqueueSeq}

	// apply is the switch every fold below writes into a Session field
	// through. A snapshot-anchored load (snapshotStartAfter above) SKIPS
	// this switch for every record at or before the anchor, so any Session
	// field only ever set here (never restored elsewhere) must also be
	// captured/restored by snapshot.go's captureSnapshotLocked/
	// restoreSnapshot — see sessionSnapshot's own doc comment and
	// TestEverySessionFieldIsClassifiedForSnapshotting
	// (engine/snapshot_field_coverage_test.go), the fail-closed guard that
	// requires every Session field to be classified as snapshotted or
	// deliberately excluded, so a new case added here cannot silently ship
	// without that decision being made.
	apply := func(rec record, line int, isLast bool) error {
		// Seed the session's metadata-index fold from the same records
		// (index.go). A resumed session keeps writing that index through
		// on every later record, so it must start from the state this
		// journal already holds — a fold that began empty here would
		// flush a summary claiming to cover the whole journal while
		// describing one record of it.
		s.index.applyIndexRecordBestEffort(indexRecordOf(rec), isLast)
		switch rec.Type {
		case recSession:
			s.applySessionHeader(rec)
		case recMessage:
			if rec.Message == nil {
				if isLast {
					return nil
				}
				return fmt.Errorf("message record without message at line %d", line)
			}
			// Normalize before appending: the same ingest-time repair
			// Session.append runs live (see message.Message.Normalize's
			// doc comment) also applies on replay, so a persisted
			// ToolResult with empty Content (an older, unpatched write, or
			// a plugin/adapter that bypassed Normalize) is already fixed
			// by the time anything downstream — a transcoder, GET
			// /session/{id}/message — sees it. Session logs are
			// append-only and never rewritten, so this repair is
			// re-derived fresh on every load rather than persisted.
			msg := *rec.Message
			msg.Normalize()
			s.history = append(s.history, msg)
			// Replay this record's per-turn Usage (if any — see
			// persistMessage) into cumulative usage/lastUsage, exactly as
			// appendWithUsage does live: this is what makes Session.Usage()
			// and LastUsage() survive a process restart (issue #62 layer 2).
			if rec.Usage != nil {
				s.usage.InputTokens += rec.Usage.InputTokens
				s.usage.OutputTokens += rec.Usage.OutputTokens
				s.usage.CacheReadTokens += rec.Usage.CacheReadTokens
				s.usage.CacheWriteTokens += rec.Usage.CacheWriteTokens
				s.lastUsage = *rec.Usage
				s.haveLastUsage = true
			}
			// Every message append means a turn has started (or is still
			// in progress) without yet being finalized — see
			// Session.turnUnsettled's own doc comment. Mirrors
			// appendWithUsage's own identical live-path write.
			s.turnUnsettled = true
			// s.committedOutcome invalidation — mirrors appendWithUsage's
			// OWN identical clear, with one deliberate exception: a
			// message recognizable as ONE OF recoverInterruptedTurnLocked's
			// own synthetic closers (isRecoverySyntheticCloser — there are
			// two now, lostToRestartText and canceledInterruptedText, see
			// the latter's own doc comment) is NOT a new turn starting —
			// it is that SAME recovery attempt annotating the turn it is
			// still in the middle of settling, appended via
			// appendMemoryOnly (which, live, never clears committedOutcome
			// either — see that method's own doc comment). Clearing here
			// regardless would durably erase the very commit record a
			// LATER recovery-of-recovery pass needs to replay verbatim —
			// reopening the exact "false DONE" divergent-duplicate bug a
			// live review found: with the commit erased, that later pass
			// falls back to settledSuccessResult(), which then sees THIS
			// closing message itself (RoleAssistant, plain text, no
			// ToolCall) as a spurious natural completion.
			if !isRecoverySyntheticCloser(msg) {
				s.committedOutcome = nil
			}
		case recChildTurnSettled:
			s.turnUnsettled = false
			// committedOutcome deliberately NOT cleared here — see its own
			// doc comment (engine.go): once settled, it becomes the last
			// known terminal outcome a LATER adoption of this node
			// restores n.status/n.result/n.failReason from
			// (SessionManager.restoreKnownStatusLocked) — cleared only
			// when a genuinely NEW turn later starts (the recMessage
			// case's own fold, mirroring appendWithUsage's live-path
			// clear), not merely because THIS turn settled.
		case recTaskOutcomeCommitted:
			// See recTaskOutcomeCommitted's own doc comment for the full
			// mechanism. Last-writer-wins is fine on the rare chance more
			// than one lands for the same still-unsettled turn (a
			// recovery-of-recovery re-commit) — every commit for the SAME
			// turn is, by construction, computed from the SAME unchanged
			// s.history and so carries identical content.
			if rec.TaskNotify != nil {
				tn := rec.TaskNotify
				oc := taskNotification{
					ChildID: tn.ChildID, Agent: tn.Agent, Status: tn.Status,
					Result: tn.Result, FailReason: tn.FailReason, FailKind: tn.FailKind,
					RecoverHint: tn.FailHint, Usage: tn.Usage, Canceled: tn.Canceled,
				}
				s.committedOutcome = &oc
			}
		case recModel:
			s.model = rec.Model
		case recEffort:
			s.effort = rec.Effort
		case recServiceTier:
			s.serviceTier = rec.ServiceTier
		case recClaudeCodeSessionID:
			s.claudeCodeCLISessionID = rec.ClaudeCodeSessionID
		case recClaudeCodeHistoryWatermark:
			s.claudeCodeHistoryWatermark = rec.ClaudeCodeHistoryWatermark
		case recClaudeCodeUsage:
			// See Session.applyClaudeCodeUsage's own doc comment for why
			// this folds into BOTH cumulative usage and lastUsage, unlike
			// recCompact's cumulative-only replay.
			if rec.Usage != nil {
				s.usage.InputTokens += rec.Usage.InputTokens
				s.usage.OutputTokens += rec.Usage.OutputTokens
				s.usage.CacheReadTokens += rec.Usage.CacheReadTokens
				s.usage.CacheWriteTokens += rec.Usage.CacheWriteTokens
				s.lastUsage = *rec.Usage
				s.haveLastUsage = true
			}
			// See record.ClaudeCodeCostUSD's own doc comment: nil means a
			// record written before cost tracking existed, not a
			// zero-cost turn, so this deliberately leaves
			// haveClaudeCodeCost false in that case rather than folding a
			// phantom zero into the running total.
			if rec.ClaudeCodeCostUSD != nil {
				s.claudeCodeSessionCostUSD += *rec.ClaudeCodeCostUSD
				s.haveClaudeCodeCost = true
			}
		case recMCPToolsSelected:
			// Union every record, in log order, into the restored selected
			// set (see mcp_lazy.go). Replay is defensive, like
			// recPromptQueued's: a name that is not mcp__<server>__<tool>
			// shaped is SKIPPED rather than folded, the same shape select
			// itself refuses to record, so one rule holds at both ends of
			// the record's life. Duplicates collapse into the set.
			//
			// Nothing here checks whether the named tool still EXISTS. It
			// cannot: no server has connected at load time. A restored name
			// whose server is absent or parked is kept, so it arms itself
			// on reconnect; one whose server connects WITHOUT it is reaped
			// on that session's first plan (reapMCPSelections).
			for _, name := range rec.MCPTools {
				if _, _, ok := splitMCPToolName(name); !ok {
					continue
				}
				if s.mcpSelected == nil {
					s.mcpSelected = map[string]bool{}
				}
				s.mcpSelected[name] = true
			}
		case recGoalSet, recGoalUpdated, recGoalAchieved, recGoalCleared:
			// applyGoalRecord holds the rule (an active goal is one set
			// without a later achieved/cleared; the run counters reset) —
			// shared with the metadata index's own fold (index.go).
			s.goalActive, s.goalCondition = applyGoalRecord(s.goalActive, s.goalCondition, rec.Type, rec.Goal)
		case recGoalEval, recGoalStalled, recGoalEvalFailed, recGoalParked:
			// Per-turn evaluation/stall/eval-failure/park trace; no resume
			// state (counters reset). None of these ever change goalActive
			// by itself — either a later record of the same kind follows,
			// or a later goal.cleared/goal.eval/goal.achieved settles it,
			// all handled above. In particular, a resumed session that
			// last saw goal.parked with nothing after it restores exactly
			// like an ordinary active goal (recGoalSet's case above already
			// set s.goalActive=true and the condition) — a park never
			// clears the goal, live or on replay.
		case recPromptQueued:
			// Both prompt-queue cases fold through promptQueueFold (queue.go),
			// which owns the torn-write last-writer-wins rule, the malformed-
			// record guards, and the ID-burn counter advance. See its own doc
			// comments; the rules moved there verbatim so the metadata index
			// can share them.
			if rec.Prompt != nil {
				qf.queued(*rec.Prompt)
			}
		case recPromptDequeued:
			if rec.Prompt != nil {
				qf.dequeued(*rec.Prompt)
			}
		case recTaskSpawned:
			// Folded into s.spawnedChildIDs — see recTaskSpawned's own doc
			// comment (the "proactive-enough" crash-recovery finding) and
			// Session.spawnedChildIDs' own doc comment (engine.go) for the
			// full mechanism. Written exactly once per child (Spawn's own
			// single persistTaskSpawnLocked call), so no dedup/removal
			// logic is needed here, unlike recPromptQueued/
			// recTaskNotifyQueued's own queued/dequeued matched-pair folds.
			if rec.TaskSpawn != nil && rec.TaskSpawn.ChildID != "" {
				s.spawnedChildIDs = append(s.spawnedChildIDs, rec.TaskSpawn.ChildID)
			}
		case recTaskNotifyQueued:
			// Fold back into s.taskNotifications exactly as if this
			// process had never stopped — see recTaskNotifyQueued's own
			// doc comment (the "notification persistence" follow-up).
			// Keyed by ChildID, not a synthetic sequence number.
			//
			// CORRECTION (a live review caught the original version of
			// this comment overclaiming): a child does NOT always notify
			// its parent only once — finalizeTurn's own doc comment
			// states plainly that it "can run more than once for the
			// same child" (session.send legitimately restarts an
			// already-done/failed child for a follow-up turn, and its
			// own completion runs finalizeTurn again), so more than one
			// recTaskNotifyQueued record for the SAME ChildID genuinely
			// is reachable on the live write path — not just a
			// theoretical replay artifact the way promptRecord's ID/Seq
			// machinery above defends against a torn-fsync retry. This
			// is still NOT a correctness bug for the fold below: it
			// removes the FIRST matching ChildID entry per delivered
			// record, in the same interleaved order finalizeTurn wrote
			// them, so balanced queued/delivered pairs still converge to
			// exactly the undelivered set regardless of how many times
			// the same child appears. It would only become a real bug if
			// some FUTURE change relied on "at most one queued record
			// per ChildID" as an invariant — it is not one.
			if rec.TaskNotify != nil {
				tn := rec.TaskNotify
				s.taskNotifications = append(s.taskNotifications, taskNotification{
					ChildID: tn.ChildID, Agent: tn.Agent, Status: tn.Status,
					Result: tn.Result, FailReason: tn.FailReason, FailKind: tn.FailKind,
					RecoverHint: tn.FailHint, Usage: tn.Usage, Canceled: tn.Canceled,
				})
			}
		case recTaskNotifyDelivered:
			// Remove the matching queued entry by ChildID — mirrors
			// recPromptDequeued's identical "remove by key, not position"
			// reasoning, so the folded set ends up exactly the
			// undelivered notifications regardless of how queued and
			// delivered records interleave in the log.
			if rec.TaskNotify != nil {
				for i, n := range s.taskNotifications {
					if n.ChildID == rec.TaskNotify.ChildID {
						s.taskNotifications = append(s.taskNotifications[:i], s.taskNotifications[i+1:]...)
						break
					}
				}
			}
		case recToolResultRetained:
			// Fold a retained tool result's pointer record (see
			// toolresult.go and docs/plans/2026-08-19-tool-result-
			// handles.md §5) back into three pieces of session state:
			//
			//   1. toolResultNextID advances past every handle number
			//      seen — folded or skipped — so a resumed session can
			//      never mint a handle that already names a sidecar file.
			//      This is the counter-survives-resume requirement, and it
			//      advances past SKIPPED records too for exactly the
			//      reason recPromptQueued advances past burned IDs: a
			//      number that reached the log may have a file behind it
			//      whatever the record's other fields say.
			//   2. toolResults regains the metadata, so read_tool_result
			//      serves a handle minted by a previous process.
			//   3. toolResultBytes regains the running total, so
			//      Config.ToolResultRetainedBytes is a session-lifetime
			//      ceiling rather than a per-process one.
			//
			// A malformed handle (not trh_<positive int>) or a duplicate
			// handle is SKIPPED, never folded — the same defensive replay
			// posture recPromptQueued takes. The live path can write
			// neither: handles are minted from a monotonic counter and
			// burned on failure, so either shape in a log is corruption,
			// and folding a duplicate would silently overwrite one
			// result's metadata with another's while the retained-bytes
			// total double-counted.
			if rec.ToolResult != nil {
				n, valid := parseToolResultHandle(rec.ToolResult.Handle)
				if valid {
					if _, dup := s.toolResults[rec.ToolResult.Handle]; dup {
						valid = false
					}
				}
				if valid {
					s.toolResults[rec.ToolResult.Handle] = toolResultMeta{
						Handle: rec.ToolResult.Handle,
						Tool:   rec.ToolResult.Tool,
						Bytes:  rec.ToolResult.Bytes,
						Lines:  rec.ToolResult.Lines,
						Head:   rec.ToolResult.Head,
					}
					s.toolResultBytes += rec.ToolResult.Bytes
				}
				if n >= s.toolResultNextID {
					s.toolResultNextID = n + 1
				}
			}
		case recCompact:
			// See docs/design/context-compaction.md §2 "LoadSession
			// replay": find FirstID/LastID within s.history accumulated so
			// far (guaranteed present, in order, since a compact record can
			// only be written chronologically after those messages were
			// themselves durably appended) and splice — the identical
			// function the live path uses (spliceCompact, compact.go), so
			// the two can never drift apart. Not found is corruption, an
			// explicit error, never a silent best-effort guess.
			if rec.Compact == nil {
				return fmt.Errorf("compact record without payload at line %d", line)
			}
			// Normalize before splicing, same as every recMessage above:
			// LoadSession calls Normalize on every message it replays,
			// including a compact record's inline summary.
			rec.Compact.Summary.Normalize()
			// Heal path (NEP-5292 candidate fix 3): a record journaled by an
			// unpatched build can name a message.ResolveOrphanToolCalls
			// synthetic ID as LastID — that message is minted fresh by the
			// repair below, AFTER this scan loop finishes, and was never
			// itself persisted, so it can never be found in s.history here.
			// Re-derive the fold end from FirstID's position plus this
			// record's own TurnsFolded count instead of trusting a LastID
			// that is genuinely absent. A FOUND LastID keeps today's exact
			// behavior unchanged; the heal never runs for it. If FirstID
			// itself is missing, that is still corruption — spliceCompact
			// below fails exactly as it always has, heal or not.
			//
			// Deliberately NOT gated on message.IsSyntheticOrphanID: the heal
			// fires for ANY absent LastID, not only a synthetic one. A
			// genuinely corrupt non-synthetic LastID now heals where it used
			// to fail the whole load. healCompactFoldEnd catches an
			// out-of-range TurnsFolded or a FirstID that is not a turn
			// boundary, but not every mismatch: it re-derives the fold end as
			// starts[startPos+turnsFolded], which assumes replayed history has
			// the SAME count of RoleUser turn boundaries the writing binary
			// saw live. dropUnansweredDirective (goal.go) removes a RoleUser
			// message from live history without retracting an already-
			// journaled compact record, so live and replayed turn counts can
			// disagree — the heal then lands on the wrong message with no
			// error. Not a regression: main hard-fails this load every time,
			// and a session that loads with a slightly-wrong fold beats a
			// session that never loads again.
			// applyCompactRecord (compact.go) runs the heal and then
			// spliceCompact. It is shared with the metadata index's own
			// fold (index.go), so both agree on how many messages a
			// compact record removes. A failed heal falls through
			// unchanged: spliceCompact looks for the original (unhealed)
			// LastID, fails to find it exactly as before, and returns its
			// usual loud, explicit error — never a silent best-effort
			// guess.
			spliced, err := applyCompactRecord(s.history, rec.Compact.FirstID, rec.Compact.LastID, rec.Compact.TurnsFolded, rec.Compact.Summary)
			if err != nil {
				return fmt.Errorf("%w at line %d", err, line)
			}
			s.history = spliced
			s.compactCount++
			s.lastCompactedAt = rec.CreatedAt
			// Cumulative usage ONLY (see record.Usage's doc comment above
			// and the "Usage accounting" section of the design doc):
			// lastUsage/haveLastUsage must never be touched by a compact
			// record's usage, or a reload would report the small
			// summarization call as the session's "last request size" and
			// defeat the automatic trigger's re-fire check.
			if rec.Usage != nil {
				s.usage.InputTokens += rec.Usage.InputTokens
				s.usage.OutputTokens += rec.Usage.OutputTokens
				s.usage.CacheReadTokens += rec.Usage.CacheReadTokens
				s.usage.CacheWriteTokens += rec.Usage.CacheWriteTokens
			}
		}
		return nil
	}

	// The tail scan. scanLogRaw, not scanLog, so a record the snapshot
	// already covers is never DECODED — skipping the decode is where the
	// saving is, since decoding a message record builds its whole part
	// tree. The decode below reproduces scanLog's corruption discipline
	// verbatim (a corrupt or truncated FINAL line ends the scan silently;
	// corruption anywhere else is an error, with the same message text) so
	// the two paths cannot drift.
	//
	// A corrupt record at or before the anchor is not detected on the
	// snapshot path. That is the accepted consequence of not reading it:
	// the snapshot was DERIVED from those exact records by the process
	// that wrote them, so its state already reflects them.
	err = scanLogRaw(data, func(raw []byte, line int, isLast bool) error {
		if int64(line) <= startAfter {
			s.recordsWritten = int64(line)
			return nil // covered by the snapshot; the header is already applied
		}
		var rec record
		if err := json.Unmarshal(raw, &rec); err != nil {
			if isLast {
				return errTruncatedFinalRecord // crash mid-write, ignore
			}
			return fmt.Errorf("corrupt record at line %d: %v", line, err)
		}
		s.replayedRecords++
		// Only a line that DECODED counts toward the head: a torn final
		// line returns above, and ensureLog's own tail repair removes it
		// from the file before this session appends again.
		s.recordsWritten = int64(line)
		return apply(rec, line, isLast)
	})
	if err != nil {
		return nil, fmt.Errorf("engine: session %s: %w", id, err)
	}
	if startAfter > 0 {
		// The header record this load applied without folding it into the
		// tail scan (see snapshotStartAfter) is still a record this load
		// decoded, and the bounded-replay guarantee is stated over records
		// decoded, not over records folded in one particular place.
		s.replayedRecords++
	}
	s.promptQueue, s.promptQueueNextID, s.enqueueSeq = qf.queue, qf.nextID, qf.seq
	// A log from an older binary or an external writer can carry an
	// assistant tool_call whose turn died before a result was recorded.
	// Repair at ingest so every downstream consumer sees a protocol-valid
	// history, not just the transcoders' wire-time backstop. This is NOT
	// the load-path counterpart of a Normalize repair: Normalize's three
	// repairs (ProviderData, ToolCall.Arguments, empty ToolResult.Content —
	// applied per message in the scanLog loop above) never touch an
	// orphaned tool_use at all. The live-path counterpart of THIS repair is
	// appendUnexecutedToolCallResults/interruptedToolResults in
	// engine/engine.go, which synthesizes results for an abnormally-ended
	// turn's unexecuted tool calls before they ever reach history.
	// ResolveOrphanToolCalls is the load-time backstop for a history that
	// bypassed that path entirely (an older binary, a plugin, or an
	// external writer). The repair is re-derived deterministically on every
	// load; the log itself stays append-only and unmodified.
	s.history = message.ResolveOrphanToolCalls(s.history)
	// newSession already resolved s.cfg.ContextWindowTokens/contextWindowSource
	// once, against cfg.Model — but a recModel record above may have moved
	// s.model to whatever this session was last switched to, in an earlier
	// process, before it ever got to write another log line. Re-derive
	// against the FINAL replayed model so a resumed session's compaction
	// window matches its actual active model, not the caller's default
	// (loadSessionFn passes defModel, not the session's own last model — see
	// cmd/harness/main.go). A no-op when nothing moved it (the common case:
	// no recModel record, or explicit config), so this never double-logs the
	// sanity-floor warning for the unchanged case.
	if !s.contextWindowExplicit && s.model != cfg.Model {
		var miss error
		s.cfg.ContextWindowTokens, s.contextWindowSource, miss = resolveContextWindow(0, s.model)
		// A resume must not be FATAL for an unrecognized model: a session
		// that cannot load cannot be listed, read, or exported either, and
		// the operator would lose the transcript along with the ability to
		// fix the config. Record the refusal instead — every Prompt against
		// this session returns it, so it still cannot silently run.
		s.contextWindowErr = requiredContextWindowErr(s.cfg, s.model, miss, "session_resume")
	}
	logContextWindowArmed(s.ID, s.model, s.cfg.ContextWindowTokens, s.contextWindowSource, "start")
	// Review finding (round 5): advance toolResultNextID past every trh_N
	// handle that appears ANYWHERE in the final replayed history text, not
	// just the ones the toolresult.retained pointer-record fold above saw.
	// That fold is best-effort — persistToolResultRetainedLocked can lose a
	// crash race, landing in lastPersistErr while writeRetainedToolResult
	// still returns the handle successfully — but the ToolResult message
	// carrying that SAME handle in its preview text is durable the instant
	// Session.append succeeds, which is a STRONGER guarantee than the
	// pointer record gets. Without this second pass, a crash between the
	// sidecar write and the pointer-record append leaves a resumed
	// session's counter pointing at a number the crashed process already
	// handed to the model in a preview it trusts; the next retention then
	// silently reuses that handle, overwriting the sidecar file the old
	// preview still names. This one text scan also covers the compaction
	// retained-results index (its lines are embedded straight into the
	// summary message's text) and read_tool_result's own echoed output
	// (its header line names the handle it read) for free — every surface
	// a handle can appear on is just "text in history" by the time this
	// runs.
	advanceToolResultNextIDFromHistory(s)
	// read_tool_result registration (review finding F12): newSession decided
	// whether to register it BEFORE this fold ran, against an empty
	// s.toolResults — the only state it could see at that point. A session
	// resumed after its config set tool_result_inline_bytes:0 (retention
	// disabled going forward) can still have replayed handles from BEFORE
	// that change, from history written while it was still enabled. Those
	// handles are real, their sidecar files are real, and read_tool_result
	// can still serve them — s.toolResultInlineLimit gates only whether a
	// NEW handle can be MINTED, never whether an existing one can be READ
	// (see runReadToolResult, which never calls toolResultInlineLimit at
	// all). Without this, a resumed session's history is full of handles
	// the model has every reason to try reading, and every attempt fails
	// with "unknown tool" — not even the tool's own clean "unknown handle"
	// error, because the tool was never registered to receive the call.
	if _, ok := s.tools[readToolResultToolName]; !ok && len(s.toolResults) > 0 {
		s.tools[readToolResultToolName] = readToolResultTool()
	}
	return s, nil
}

// applySessionHeader restores the state a session's header record carries
// into s: its creation time and the Config fields the header is the durable
// truth for. Factored out of LoadSession's own recSession fold case because
// snapshot recovery (snapshot.go) applies the header WITHOUT replaying any
// other record — the header is line 1 of every journal, so replaying it
// unconditionally is cheaper than reproducing these restore rules, each of
// which turns on the difference between "this header omitted the field" and
// "the loading Config already has a value", in a second place.
func (s *Session) applySessionHeader(rec record) {
	s.createdAt = rec.CreatedAt
	// A restored WorkDir wins over the loading Config.WorkDir: the
	// header is the durable truth for a resumed session. A legacy
	// header (written before this field existed) omits it, so an
	// empty value here means "nothing to restore" — the loading
	// Config.WorkDir is kept unchanged.
	if rec.WorkDir != "" {
		s.cfg.WorkDir = rec.WorkDir
	}
	// Same restore rule as WorkDir above: the header is the durable
	// truth for a resumed session, and an empty value here means
	// nothing to restore (legacy header, or no lineage recorded),
	// never "clear the loading Config's ParentSession".
	if rec.ParentSession != "" {
		s.cfg.ParentSession = rec.ParentSession
	}
	// Same restore rule, but see Config.TaskParentID's doc comment
	// for why this is a different field entirely from
	// ParentSession above.
	if rec.TaskParentID != "" {
		s.cfg.TaskParentID = rec.TaskParentID
	}
	// Same restore rule again — see Config.TaskAgentType/
	// TaskToolNames's own doc comment.
	if rec.TaskAgentType != "" {
		s.cfg.TaskAgentType = rec.TaskAgentType
	}
	if rec.TaskToolNames != nil {
		s.cfg.TaskToolNames = *rec.TaskToolNames
	}
	// Same restore rule again — see Config.TaskDepth's own doc
	// comment. 0 means "this header genuinely predates the field"
	// (a real depth is always >= 1) — but unlike ParentSession/
	// TaskAgentType above, the loading Config's OWN TaskDepth is
	// NOT always safe to leave untouched on that branch: it is not
	// guaranteed unpopulated the way this restore rule assumes
	// elsewhere. SessionManager's crash-recovery sweep
	// (recoverCrashedChildrenLocked, session_manager.go) calls
	// LoadSession with a Config built from configSnapshot() of the
	// PARENT node currently being adopted — which, since
	// configSnapshot copies Config by value, carries THAT PARENT's
	// own live TaskDepth. A legacy child (this header predates the
	// field) loaded under that Config would otherwise silently
	// inherit its parent's depth instead of correctly falling back
	// to adoptReloadedLocked's own m.maxDepth refusal sentinel.
	// Reset to 0 unconditionally whenever s.cfg.TaskParentID is
	// non-empty (this IS a task-tool child, restored above either
	// from this record or the loading Config) but this specific
	// header recorded no depth, so the sentinel fallback always
	// applies for a genuinely legacy child regardless of what the
	// loading Config happened to carry in for an unrelated reason.
	// A genuine root (TaskParentID empty either way) is unaffected
	// either branch — TaskDepth is never read for one.
	if rec.TaskDepth > 0 {
		s.cfg.TaskDepth = rec.TaskDepth
	} else if s.cfg.TaskParentID != "" {
		s.cfg.TaskDepth = 0
	}
	// The effort at create time. Omitted (EffortUnset) on a legacy
	// header, which restores as the provider default — unchanged.
	s.effort = rec.Effort
	// The service tier at create time. Omitted (empty) on a header
	// predating this field, which restores as the provider default —
	// unchanged. Mirrors the effort restore immediately above.
	s.serviceTier = rec.ServiceTier
}

// toolResultHandleInTextPattern matches a canonical trh_N handle token
// (digits only, no leading zero, no sign — the exact grammar
// parseToolResultHandle enforces) anywhere inside a larger string, with NO
// word-boundary anchor. That's deliberate: over-matching (a coincidental
// "trh_123"-shaped substring in unrelated text) only advances
// toolResultNextID a little further than strictly necessary, which is
// harmless — handle numbers are cheap and never reused anyway. Under-
// matching a genuine handle is the dangerous direction (see
// advanceToolResultNextIDFromHistory), so this pattern is deliberately
// permissive rather than precise.
var toolResultHandleInTextPattern = regexp.MustCompile(`trh_[1-9][0-9]*`)

// advanceToolResultNextIDFromHistory scans every *message.Text part in the
// final replayed s.history for trh_N handle tokens and advances
// s.toolResultNextID past the highest one found. See the call site in
// LoadSession for why this exists (review finding, round 5): the
// toolresult.retained pointer-record fold is best-effort and can be lost to
// a crash, while a handle's PREVIEW TEXT reaching history is a strictly
// stronger durability guarantee (Session.append itself). This single text
// scan also happens to cover the compaction retained-results index (its
// lines are embedded directly in the summary message's text) and
// read_tool_result's own echoed output (its header names the handle it
// read) — every surface a handle can appear on is just message text by the
// time this runs, so one scan closes all of them at once.
func advanceToolResultNextIDFromHistory(s *Session) {
	var maxSeen int64
	for _, m := range s.history {
		for _, p := range m.Parts {
			t, ok := p.(*message.Text)
			if !ok {
				continue
			}
			for _, h := range toolResultHandleInTextPattern.FindAllString(t.Text, -1) {
				if n, ok := parseToolResultHandle(h); ok && n > maxSeen {
					maxSeen = n
				}
			}
		}
	}
	if maxSeen+1 > s.toolResultNextID {
		s.toolResultNextID = maxSeen + 1
	}
}

// scanLog iterates the JSONL records of a session log, decoding each line
// into T and calling fn with the 1-based line number. It owns the log's
// corruption discipline — shared by every reader so the rules cannot drift:
// a corrupt or truncated final line (crash mid-write) ends iteration
// silently; corruption anywhere else is an error.
func scanLog[T any](data []byte, fn func(rec T, line int, isLast bool) error) error {
	return scanLogRaw(data, func(raw []byte, line int, isLast bool) error {
		var rec T
		if err := json.Unmarshal(raw, &rec); err != nil {
			if isLast {
				return errTruncatedFinalRecord // crash mid-write, ignore
			}
			return fmt.Errorf("corrupt record at line %d: %v", line, err)
		}
		return fn(rec, line, isLast)
	})
}

// errTruncatedFinalRecord ends a scan at a corrupt FINAL line, which is
// scanLog's documented tolerance for a crash mid-write. scanLogRaw absorbs
// it, so a caller sees the same clean end scanLog has always returned.
// Every OTHER error propagates.
//
// scanLogRaw compares it by IDENTITY, never with errors.Is. A callback that
// wrapped this sentinel into a genuine failure — "cannot update index: %w"
// — would otherwise have that failure read as a torn final record and
// reported as a clean scan. Identity keeps the signal to the one decoder
// that raises it.
var errTruncatedFinalRecord = errors.New("engine: truncated final record")

// scanLogRaw is scanLog without the decode: it hands fn each non-empty line
// as raw bytes, aliasing data rather than copying it, and owns the same
// corruption discipline (a corrupt or truncated FINAL line ends iteration
// silently; corruption anywhere else is the caller's error to report).
//
// It exists for a reader that must decide, per line, HOW MUCH of it to
// decode. foldedPage (messagepage.go) folds every line through a slim shape
// and then fully decodes only the handful of records a page actually
// carries. Routed through scanLog instead, that reader would decode every
// message body in the journal — the cost the paginated read exists to
// avoid.
func scanLogRaw(data []byte, fn func(raw []byte, line int, isLast bool) error) error {
	lines := bytes.Split(data, []byte("\n"))
	last := len(lines) - 1
	for last >= 0 && len(bytes.TrimSpace(lines[last])) == 0 {
		last--
	}
	for i := range lines[:last+1] {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if err := fn(line, i+1, i == last); err != nil {
			if err == errTruncatedFinalRecord { //nolint:errorlint // identity on purpose; see the sentinel's doc comment
				return nil
			}
			return err
		}
	}
	return nil
}

// ListSessions lists persisted sessions in dir, sorted by creation time. A
// missing directory yields an empty list, not an error.
//
// The session JOURNALS are what exist. The metadata index (index.go) is an
// acceleration over them, never the source of truth about existence: a
// session whose sidecar is missing, stale, or unusable — or whose fold
// breaks on a damaged compact record — is still a session, and a listing
// that dropped it would lie to every caller that asks "what is here". So
// each journal is answered by its index when the index can answer, and by
// a direct scan of the journal when it cannot. Only a file that is not a
// session log at all is skipped.
//
// The index path never writes. Listing a directory must not rewrite the
// sidecar of a session another writer holds; the write path repairs it.
//
// One semantic follows from the two answers. Messages counts messages
// after compaction folds on the index path, which is what a full load
// reports. The fallback scan cannot fold — a broken fold is why it ran —
// so for that session it counts message records instead, the number the
// previous header-only scan always reported.
func ListSessions(dir string) ([]SessionInfo, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var infos []SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		// The file NAME, not a validated id: a listing has always reported
		// any *.jsonl carrying a session header, whatever it is called.
		info, err := sessionInfoAt(dir, strings.TrimSuffix(e.Name(), ".jsonl"))
		if err != nil {
			continue // unreadable, or not a session log: not listable
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].CreatedAt.Before(infos[j].CreatedAt) })
	return infos, nil
}

// ReadSessionInfo returns one session's listing summary: its index when
// that index can answer, and a direct scan of its journal when it cannot.
//
// It is the single-session form of ListSessions, for a caller that already
// knows which sessions it wants — GET /session/status walks ids it resolved
// itself. Sharing this one path is what keeps that endpoint and a listing
// from disagreeing about which sessions exist: a session whose fold breaks
// must appear in both, or in neither.
//
// Like a listing, it never writes: a refold here answers this call and is
// dropped. The write path repairs a sidecar.
func ReadSessionInfo(dir, id string) (SessionInfo, error) {
	if dir == "" {
		return SessionInfo{}, errors.New("engine: ReadSessionInfo requires a session dir")
	}
	if !ValidSessionID(id) {
		return SessionInfo{}, fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	return sessionInfoAt(dir, id)
}

// sessionInfoAt is ReadSessionInfo without the id validation, for
// ListSessions, whose ids are directory entries rather than caller input.
func sessionInfoAt(dir, id string) (SessionInfo, error) {
	if ix, err := readSessionIndexAt(dir, id, false); err == nil {
		return SessionInfo{
			ID:              ix.ID,
			CreatedAt:       ix.CreatedAt,
			Messages:        ix.Messages,
			Usage:           ix.Usage,
			LastInputTokens: ix.LastInputTokens,
		}, nil
	}
	// No usable index. Read the journal itself rather than report nothing.
	info, err := readSessionInfo(sessionPath(dir, id))
	if err != nil {
		return SessionInfo{}, err
	}
	// The FILENAME names the session, on both paths. LoadSession pins the
	// same way, and so does the index (see readSessionIndexAt), so a
	// journal copied to a new name reports the new name however it was
	// read. Without this, a listing could report one id from its index and
	// another from its fallback for the same file.
	info.ID = id
	return info, nil
}

// readSessionInfo scans one journal for the fields a listing needs. It is
// ListSessions' fallback for a journal the index cannot answer for, and it
// decodes only record heads — never message bodies — so it stays cheap on a
// large session.
//
// It does not fold compaction: a compact record's own payload is skipped
// like any other unknown field, so Messages counts message RECORDS. That is
// the number this scan has always reported, and it runs only where the fold
// that would have corrected it is the thing that failed.
//
// It DOES count a compact record's usage, which the pre-index version of
// this scan did not. A compact record carries the summarization call's own
// spend, LoadSession adds it to cumulative usage, and so does the index. A
// fallback should report the number its fast path would have reported, so
// this one follows the index rather than its own history. LastInputTokens
// still moves for message records only — the same rule LoadSession applies,
// so a reload never reports the small summarization call as the session's
// last request size.
func readSessionInfo(path string) (SessionInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionInfo{}, err
	}
	type headRecord struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		CreatedAt time.Time       `json:"created_at"`
		Usage     *provider.Usage `json:"usage,omitempty"`
	}
	var info SessionInfo
	first := true
	err = scanLog(data, func(rec headRecord, line int, isLast bool) error {
		if first {
			if rec.Type != recSession {
				return fmt.Errorf("engine: %s: missing session header", path)
			}
			info.ID = rec.ID
			info.CreatedAt = rec.CreatedAt
			first = false
			return nil
		}
		// Two record types carry usage a reader counts, and only two: a
		// message record and a compact record. LoadSession reads exactly
		// those, so a stray usage field on any other record — a goal
		// record written by a future build, say — must not inflate a
		// listing that the authoritative load would not.
		switch rec.Type {
		case recMessage:
			info.Messages++
			if rec.Usage != nil {
				info.addUsage(*rec.Usage)
				info.LastInputTokens = rec.Usage.InputTokens
			}
		case recCompact:
			if rec.Usage != nil {
				// Cumulative only. LastInputTokens must not move for a
				// summarization call — see record.Usage's doc comment.
				info.addUsage(*rec.Usage)
			}
		}
		return nil
	})
	if err != nil {
		return SessionInfo{}, err
	}
	if first {
		return SessionInfo{}, fmt.Errorf("engine: %s: empty session file", path)
	}
	return info, nil
}
