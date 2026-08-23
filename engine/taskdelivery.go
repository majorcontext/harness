package engine

import (
	"fmt"
	"strings"

	"github.com/majorcontext/harness/provider"
)

// taskNotification is one pending completion signal from a spawned child,
// queued on the PARENT session (Session.enqueueTaskNotification) until the
// parent's next streamTurn call drains it (drainTaskNotificationsSegment)
// — the design doc's "queue-or-resume delivery": queued for a running
// parent (picked up at its next turn boundary, which streamTurn already
// is), engine-initiated for an idle one (see SessionManager.finalizeTurn
// and triggerResumeLocked).
type taskNotification struct {
	ChildID    string
	Agent      string
	Status     SessionStatus // StatusDone or StatusFailed; nothing else is ever queued
	Result     string        // the child's final text — set for StatusDone
	FailReason string        // classified (#82-rule) reason — set for StatusFailed
	Usage      provider.Usage
}

// taskNotificationResultCap bounds how much of a child's final text a
// notification carries verbatim — a long-running research child can return
// pages of text, and this block goes straight into the parent's next
// request as ambient context on every subsequent turn's history replay
// (like any other message, once appended... except EngineContext is NOT
// appended to durable history at all, see message.EngineContext's doc
// comment — but it IS resent as part of the live request each time
// withAmbientStatus runs against a rebuilt message set, so keeping it
// bounded still matters for the immediate request size).
const taskNotificationResultCap = 4000

// taskResumeTriggerText is the synthetic user-role message
// SessionManager.triggerResumeLocked appends when it initiates a resume
// turn on an idle parent — the design doc's "engine-initiated resume
// turn," a new engine capability. It mirrors the goal loop's own
// established pattern for driving a turn with no real end-user input
// (PursueGoal's worker directive, promptTurnWithRetry in goal.go): a
// short, honest, visible user-role message, real history a resumed
// conversation can refer back to — never an empty or synthetic-looking
// message that could confuse a transcript reader, and never a vehicle for
// the notification's actual content, which rides the EngineContext part
// this same turn's streamTurn call attaches to it (see
// drainTaskNotificationsSegment and withAmbientStatus in process.go).
const taskResumeTriggerText = "A background task you started has finished. See the engine context below for its result, and continue accordingly."

// enqueueTaskNotification appends n to s's pending queue. Safe to call
// from any goroutine (SessionManager.finalizeTurn calls it from whichever
// goroutine just finished driving the CHILD's turn, which is never s
// itself).
func (s *Session) enqueueTaskNotification(n taskNotification) {
	s.mu.Lock()
	s.taskNotifications = append(s.taskNotifications, n)
	s.mu.Unlock()
}

// hasPendingTaskNotifications reports whether s has at least one
// undelivered notification, without draining it — SessionManager uses
// this only for tests and diagnostics; streamTurn always calls
// drainTaskNotificationsSegment directly.
func (s *Session) hasPendingTaskNotifications() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.taskNotifications) > 0
}

// drainTaskNotificationsSegment pops every pending notification and
// renders them as one ambient status segment, in the same "[name: ...]"
// shape processStatusSegment/mcpStatusSegment/identityStatusSegment use
// (engine/process.go's withAmbientStatus is the single producer that turns
// this into a wire-level EngineContext part). Unlike those three, which
// are pure functions recomputed fresh on every call, this one MUTATES: it
// drains the queue, so each notification is delivered exactly once — a
// child's completion is a one-shot EVENT, not a live status a repeated
// read should keep reporting. This mirrors the prompt queue's own
// drain-once shape (DequeueAllPrompts, queue.go) for the identical
// exactly-once reason, not the other three ambient producers' idempotent
// one.
//
// Called from streamTurn on EVERY model call — including a later iteration
// of an in-progress tool loop, not only the first call of a Prompt — so a
// notification that arrives while the parent is mid-turn (StatusRunning)
// is delivered at that very next turn boundary with no special handling:
// it is simply sitting in the queue the next time this function runs.
func (s *Session) drainTaskNotificationsSegment() string {
	s.mu.Lock()
	pending := s.taskNotifications
	s.taskNotifications = nil
	s.mu.Unlock()
	if len(pending) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[tasks: ")
	for i, n := range pending {
		if i > 0 {
			b.WriteString("; ")
		}
		switch n.Status {
		case StatusDone:
			fmt.Fprintf(&b, "%s (agent=%s) done: %s (usage: %d in / %d out)",
				n.ChildID, n.Agent, truncateTaskResult(n.Result), n.Usage.InputTokens, n.Usage.OutputTokens)
		case StatusFailed:
			fmt.Fprintf(&b, "%s (agent=%s) failed: %s (usage: %d in / %d out)",
				n.ChildID, n.Agent, n.FailReason, n.Usage.InputTokens, n.Usage.OutputTokens)
		}
	}
	b.WriteString("]")
	return b.String()
}

// truncateTaskResult bounds a child's final text to
// taskNotificationResultCap runes, marking the cut so the parent model
// knows more is available (via session_info / session.info, Stage 4) —
// never a silent truncation.
func truncateTaskResult(s string) string {
	r := []rune(s)
	if len(r) <= taskNotificationResultCap {
		return s
	}
	return string(r[:taskNotificationResultCap]) + "… [truncated]"
}
