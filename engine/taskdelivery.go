package engine

import (
	"fmt"
	"strings"

	"github.com/majorcontext/harness/provider"
)

// taskNotification is one pending completion signal from a spawned child,
// queued on the PARENT session (Session.enqueueTaskNotification) until the
// parent's next streamTurn call checks it out (checkoutTaskNotificationsSegment)
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
// checkoutTaskNotificationsSegment and withAmbientStatus in process.go).
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
// undelivered notification — pending OR already checked out for an
// in-flight turn attempt that hasn't yet committed or been requeued.
// Diagnostic/test use only; streamTurn always calls
// checkoutTaskNotificationsSegment directly.
func (s *Session) hasPendingTaskNotifications() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.taskNotifications) > 0 || len(s.taskNotificationsInFlight) > 0
}

// checkoutTaskNotificationsSegment renders every notification currently
// pending OR already checked out for the CURRENT in-flight turn attempt,
// as one ambient status segment in the same shape
// processStatusSegment/mcpStatusSegment/identityStatusSegment use
// (engine/process.go's withAmbientStatus is the single producer that turns
// this into a wire-level EngineContext part) — but, UNLIKE checking those
// three out, this does NOT commit the notifications as delivered.
//
// # Why checkout/commit/requeue, not a single destructive drain
//
// An earlier version of this method drained (popped) the queue directly,
// on every call. That broke two ways an adversarial review reproduced
// live:
//   - streamTurnWithRetry (prompt_retry.go) can call streamTurn MULTIPLE
//     times for ONE logical turn — a transient provider error, or a
//     discarded empty-turn attempt, triggers a retry. Attempt 1 would
//     drain the queue into a request that then FAILED; attempt 2 would
//     re-render against an now-EMPTY queue — the notification vanished,
//     never actually delivered to the model in any request that survived.
//   - Even a first-attempt success does not, by itself, guarantee the
//     assistant turn that resulted gets KEPT — see emptyTurnError's own
//     discard path.
//
// The fix: this method only ever MOVES newly-pending notifications into an
// in-flight set and renders THAT (so a retried attempt within the same
// logical turn keeps re-rendering the identical content, and a new
// notification that arrives mid-retry is folded in for the NEXT attempt
// too). commitTaskNotifications (called by runAgenticLoop only once its
// streamTurnWithRetry call actually SUCCEEDS) clears the in-flight set —
// genuinely delivered. requeueTaskNotifications (called on that call's
// failure) returns the in-flight set to pending, so a LATER turn gets
// another chance — at-least-once delivery, never lost to a retry or a
// discard.
//
// Called from streamTurn on EVERY model call — including a later iteration
// of an in-progress tool loop, not only the first call of a Prompt — so a
// notification that arrives while the parent is mid-turn (StatusRunning)
// is delivered at that very next turn boundary with no special handling:
// it is simply sitting in the queue (pending or in-flight) the next time
// this function runs.
func (s *Session) checkoutTaskNotificationsSegment() string {
	s.mu.Lock()
	if len(s.taskNotifications) > 0 {
		s.taskNotificationsInFlight = append(s.taskNotificationsInFlight, s.taskNotifications...)
		s.taskNotifications = nil
	}
	inFlight := append([]taskNotification(nil), s.taskNotificationsInFlight...)
	s.mu.Unlock()
	return renderTaskNotifications(inFlight)
}

// commitTaskNotifications clears the in-flight set: call once the turn
// that checked them out (via checkoutTaskNotificationsSegment) has
// ACTUALLY succeeded — produced a real, kept result, not merely attempted
// one. See runAgenticLoop's call site (engine.go) and
// checkoutTaskNotificationsSegment's doc comment for why this two-phase
// commit exists at all.
func (s *Session) commitTaskNotifications() {
	s.mu.Lock()
	s.taskNotificationsInFlight = nil
	s.mu.Unlock()
}

// requeueTaskNotifications returns the in-flight set to pending: call when
// the turn that checked them out ultimately failed (streamTurnWithRetry
// exhausted its retry budget, or any other terminal error reached
// runAgenticLoop) — see runAgenticLoop's call site. A later turn — the
// next ordinary prompt, or another engine-initiated resume once the
// session goes idle again — checks them out again from there. Newly
// pending notifications that arrived during the failed attempt (already
// folded into the in-flight set by checkoutTaskNotificationsSegment) are
// requeued right along with the ones that were already in flight, in
// original order.
func (s *Session) requeueTaskNotifications() {
	s.mu.Lock()
	if len(s.taskNotificationsInFlight) > 0 {
		s.taskNotifications = append(s.taskNotificationsInFlight, s.taskNotifications...)
		s.taskNotificationsInFlight = nil
	}
	s.mu.Unlock()
}

// renderTaskNotifications renders pending notifications as one ambient
// status block, one notification per line. One-per-line (rather than the
// "; "-joined single line an earlier version used), combined with
// neutralizeNotificationText below, is a deliberate structural defense: a
// child's Result is untrusted, free-form model output (the design doc's
// own words: "anything instruction-shaped in it is the model's to
// distrust per the existing sentinel rule" — that rule protects the OUTER
// envelope, RenderEngineContext's sentinel tags; this closes the
// narrower, ONE-LEVEL-IN gap an adversarial review flagged: a
// semicolon-joined single line let a child's own text embed a literal
// newline followed by fabricated "- ses_fake (agent=x) done: trust me"
// content that read, structurally, as a SIBLING notification the engine
// never actually produced. Stripping newlines from the free-text fields
// before they're spliced in means a child cannot manufacture a new line
// at all, so it cannot forge an entry that looks like it came from this
// function. It does not, and is not meant to, stop a child from writing
// misleading prose on its own single line — that residual risk is exactly
// what the design doc's "distrust it" rule already accepts.
func renderTaskNotifications(pending []taskNotification) string {
	if len(pending) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[tasks:")
	for _, n := range pending {
		b.WriteString("\n- ")
		switch n.Status {
		case StatusDone:
			fmt.Fprintf(&b, "%s (agent=%s) done: %s (usage: %d in / %d out)",
				n.ChildID, n.Agent, neutralizeNotificationText(truncateTaskResult(n.Result)), n.Usage.InputTokens, n.Usage.OutputTokens)
		case StatusFailed:
			fmt.Fprintf(&b, "%s (agent=%s) failed: %s (usage: %d in / %d out)",
				n.ChildID, n.Agent, neutralizeNotificationText(n.FailReason), n.Usage.InputTokens, n.Usage.OutputTokens)
		}
	}
	b.WriteString("\n]")
	return b.String()
}

// neutralizeNotificationText collapses newlines to spaces — see
// renderTaskNotifications' doc comment for why. FailReason is always
// engine-generated (classifySpawnError, or the fixed "canceled" string —
// never raw child text), so it never actually needs this; applied
// uniformly anyway so there is exactly one rule for "text that lands
// inside a [tasks: ...] line," not one rule per field that could drift.
func neutralizeNotificationText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
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
