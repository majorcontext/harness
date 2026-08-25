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
	ChildID string
	Agent   string
	Status  SessionStatus // StatusDone or StatusFailed; nothing else is ever queued
	Result  string        // the child's final text — set for StatusDone
	// FailReason is set for StatusFailed: a fixed classified prefix, then
	// the masked, capped provider cause — see classifySpawnError
	// (session_manager.go) for both halves and the #82 leak rule they
	// satisfy together.
	FailReason string
	Usage      provider.Usage
	// Canceled distinguishes a child that was explicitly Cancel()ed from
	// an ordinarily failed one — deliberately NOT folded into Status
	// (which stays exactly Done/Failed, unchanged, for every existing
	// parent-facing consumer: the queued/delivered wire shape, the
	// [tasks:] rendering, etc.) and NOT inferable from FailReason=="canceled"
	// either: classifySpawnError (below) also produces that exact string
	// for a genuinely FAILED turn whose own context was canceled for some
	// OTHER reason (e.g. an ancestor's context propagating), which is a
	// real StatusFailed outcome, not a StatusCanceled one — string-matching
	// FailReason would conflate the two. Set true ONLY by finalizeTurn's
	// alreadyCanceled branch (n.status was already StatusCanceled,
	// directly, via cancelSubtreeLocked, before finalizeTurn ever ran).
	// Read ONLY by SessionManager.restoreKnownStatusLocked and
	// recoverInterruptedTurnLocked, to restore/set n.status accurately
	// for a re-adopted or recovered node — a live review finding: without
	// this, re-adopting an already-canceled child silently rewrote its
	// history to StatusFailed, indistinguishable from a genuine failure,
	// the moment restoreKnownStatusLocked's committed.Status (Done/Failed
	// only) was all that survived to restore from.
	Canceled bool
}

// nodeStatusForOutcome maps n (a taskNotification carrying a completed
// turn's outcome — a committedOutcome being restored, or a freshly-built
// notify being applied to the node it describes) to the sessionNode
// status that outcome actually represents — the ONE place this mapping
// happens, shared by SessionManager.restoreKnownStatusLocked and
// recoverInterruptedTurnLocked (both used to inline their own copy of
// the Done/Failed half of this switch, which is what let the Canceled
// case go unhandled in the first place — see Canceled's own doc comment).
func nodeStatusForOutcome(n taskNotification) SessionStatus {
	switch {
	case n.Canceled:
		return StatusCanceled
	case n.Status == StatusDone:
		return StatusDone
	default:
		return StatusFailed
	}
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

// enqueueTaskNotification appends n to s's pending queue AND durably
// persists it in one call. Safe to call from any goroutine NOT already
// holding SessionManager's own m.mu — see enqueueTaskNotificationMemoryOnly/
// persistQueuedTaskNotification's own doc comments for the split version
// every SessionManager call site (finalizeTurn, recoverInterruptedTurnLocked,
// Spawn) uses instead, and why. Kept as a single call for direct callers
// outside that lock (tests, primarily).
func (s *Session) enqueueTaskNotification(n taskNotification) {
	s.enqueueTaskNotificationMemoryOnly(n)
	s.persistQueuedTaskNotification(n)
}

// enqueueTaskNotificationMemoryOnly appends n to s's pending queue without
// persisting — the in-memory half of enqueueTaskNotification, split out
// for SessionManager callers that hold m.mu (the single lock guarding
// every session in the tree) and must not do disk I/O while holding it.
// persistQueuedTaskNotification is the paired durable half, meant to run
// AFTER m.mu is released — see SessionManager.deferPersist/
// unlockAndFlushPersist's own doc comment for the full mechanism and why
// it exists (a live review finding: this session's own disk write used
// to run WHILE m.mu was held, letting one session's slow disk stall
// Info/Reap/Spawn/finalize for every OTHER session in the process).
// Splitting is safe here specifically because nothing durable needs to
// have happened yet for the in-memory append to be immediately useful:
// checkoutTaskNotificationsSegment (an idle-resume turn started under
// the SAME m.mu-held call this append is part of, via triggerResumeLocked)
// reads s.taskNotifications directly, never waiting on the durable write.
func (s *Session) enqueueTaskNotificationMemoryOnly(n taskNotification) {
	s.mu.Lock()
	s.taskNotifications = append(s.taskNotifications, n)
	s.mu.Unlock()
}

// enqueueTaskNotificationMemoryOnlyDeduped is
// enqueueTaskNotificationMemoryOnly's sibling for
// recoverInterruptedTurnLocked's crash-window-safe delivery specifically:
// like enqueueTaskNotificationMigrated (see its own doc comment for the
// exact-value dedup rationale — taskNotification is a plain comparable
// struct), it skips the append when an identical notification is already
// present, but unlike that method it reports whether it actually added
// anything, so the caller knows whether persistQueuedTaskNotification
// needs to run at all for n. A live review finding: recovery used to
// durably mark its own turn settled (making a retry structurally
// impossible — hasUnfinalizedTurn() becomes false forever) BEFORE
// delivering its failure notification to the ancestor; a crash in
// between permanently lost the notification with no way to ever retry.
// Reordering so delivery happens first means a crash in THAT gap now
// causes a genuine retry (hasUnfinalizedTurn() stays true) instead of
// silent loss — but a retry recomputes and re-attempts the SAME
// delivery, which must not re-persist (or re-render to the parent
// model) a notification already durably queued from the earlier,
// partially-completed attempt. Returns true (added, caller should
// persist) for a genuinely first delivery OR anything that has
// genuinely changed since; false (skip) for an exact-value repeat.
func (s *Session) enqueueTaskNotificationMemoryOnlyDeduped(n taskNotification) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.taskNotifications {
		if existing == n {
			return false
		}
	}
	s.taskNotifications = append(s.taskNotifications, n)
	return true
}

// persistQueuedTaskNotification durably writes n's recTaskNotifyQueued
// record — the deferred counterpart to enqueueTaskNotificationMemoryOnly/
// enqueueTaskNotificationMemoryOnlyDeduped's in-memory-only append. See
// recTaskNotifyQueued's own doc comment (store.go) for the two
// follow-ups this closes: a structured, independently-queryable
// spawn/delivery journal, and (paired with commitTaskNotifications'
// matching recTaskNotifyDelivered write) surviving a parent-side
// crash/restart BEFORE this notification is ever checked out —
// LoadSession's queued-minus-delivered fold restores it into
// s.taskNotifications exactly as if this process had never stopped.
func (s *Session) persistQueuedTaskNotification(n taskNotification) {
	s.mu.Lock()
	s.persistTaskNotifyLocked(recTaskNotifyQueued, n)
	s.mu.Unlock()
}

// enqueueTaskNotificationMigrated is enqueueTaskNotification's sibling for
// ReportTurnStart's OLD-object-to-fresh-object migration specifically: it
// skips the append (and the durable write) entirely when an identical
// notification (taskNotification is a plain comparable struct — no
// slices, maps, or pointers — so == is a real deep-equality check here)
// is already sitting in s's pending queue.
//
// A live review finding: ReportTurnStart's migration of a stale, evicted
// object's queue onto a freshly cold-loaded one predates the durable
// queued-minus-delivered fold LoadSession now performs (see
// recTaskNotifyQueued's own doc comment in store.go). Once that fold
// existed, the two mechanisms overlapped for the exact same notification:
// a background child finishes and enqueues onto the evicted OLD object
// (durably writing recTaskNotifyQueued); the resume that follows cold-
// loads a FRESH session, whose own LoadSession call already folds that
// just-written record in as "copy 1"; ReportTurnStart then runs its
// migration, draining the SAME notification off the (still populated,
// never touched since) old object and re-enqueuing it as "copy 2" —
// checkoutTaskNotificationsSegment then rendered the same child
// completion to the parent model twice.
//
// The migration is still genuinely needed for a narrower race the fold
// alone cannot cover: LoadSession runs OUTSIDE m.mu (it may hit disk),
// so something can enqueue onto the OLD object in the gap between that
// load completing and ReportTurnStart reacquiring the lock to swap
// n.session — a notification the fresh object's own load could not
// possibly have seen. Deduping on the notification's own value, not
// removing the migration outright, is what keeps that race covered while
// closing the common-case double-delivery: anything the fold already
// restored is skipped as an exact match; anything genuinely new (the
// race window, or literally any other divergence) still migrates.
//
// NEVER persists — not even on the append (genuinely-new, race-window)
// branch. A live review finding, caught within minutes of this method's
// own first version landing: n can only ever reach this method by having
// first been drained off old (drainAllTaskNotifications — memory-only for
// every caller now, see its own doc comment), and old can only ever have
// HAD n in the first place because old's own earlier enqueueTaskNotification
// call already durably wrote n's recTaskNotifyQueued record — to THIS
// SAME shared log, since old and s are two in-memory objects for one
// durable session id. Writing a SECOND recTaskNotifyQueued for n here,
// even on the "new" branch, is therefore always a duplicate of a record
// that's already there: a future reload's queued-minus-delivered fold
// would net one phantom pending copy of n, double-delivering the same
// child completion after a restart. The in-memory append is the only
// work this method ever needs to do.
func (s *Session) enqueueTaskNotificationMigrated(n taskNotification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.taskNotifications {
		if existing == n {
			return
		}
	}
	s.taskNotifications = append(s.taskNotifications, n)
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

// drainAllTaskNotifications unconditionally empties BOTH the pending and
// in-flight sets and returns everything that was in them, in original
// order. Unlike checkoutTaskNotificationsSegment's checkout/commit/
// requeue two-phase handoff (which exists so a notification survives a
// RETRIED turn), this is for a session that is never going to run
// another turn of its own at all: SessionManager.finalizeTurn and
// recoverInterruptedTurnLocked both call it on a CHILD that just went
// terminal itself, to FORWARD any notifications ITS OWN children
// (grandchildren) queued on it onto a DIFFERENT session (the nearest
// live ancestor) — which would otherwise be stranded forever on a node
// that will never check out its queue again (see finalizeTurn's own doc
// comment); ReportTurnStart's OLD-object-to-fresh-object migration also
// calls it, for the SAME durable session id rather than a different one.
//
// Memory-only — does NOT persist a recTaskNotifyDelivered record for
// what it drains. Two live review findings, in order:
//
//  1. This method used to persist unconditionally. That was correct
//     for the forward-to-a-different-ancestor callers (finalizeTurn,
//     recoverInterruptedTurnLocked — see persistDeliveredTaskNotifications,
//     which those callers now call explicitly once the forwarded set has
//     actually been re-homed) but WRONG for the migration's same-session-id
//     case: old and sess there are two in-memory objects for ONE durable
//     session id sharing ONE log. Persisting a delivered record there
//     durably canceled the notification's ORIGINAL recTaskNotifyQueued
//     entry on that shared log, while enqueueTaskNotificationMigrated's
//     own dedup correctly declined to write a compensating fresh queued
//     record for something LoadSession's fold already restored — leaving
//     the log showing a balanced (queued+delivered) notification that was
//     still, in fact, only in-memory pending and undelivered. A crash or a
//     second eviction before it was ever checked out then had the next
//     LoadSession fold it as genuinely delivered and silently drop it.
//  2. Once (1)'s fix made this method memory-only for that one caller, a
//     second review finding: SessionManager's finalizeTurn/
//     recoverInterruptedTurnLocked callers hold m.mu (the single lock
//     guarding every session in the tree) for the whole call — persisting
//     here, even correctly, meant disk I/O ran WHILE that global lock was
//     held. Making this method memory-only for EVERY caller and moving the
//     persist step to persistDeliveredTaskNotifications, called explicitly
//     after m.mu is released (via SessionManager.deferPersist/
//     unlockAndFlushPersist), closes that too — one caller with one
//     obligation (persist after forwarding succeeds) instead of two
//     divergent behaviors hidden behind one method name.
func (s *Session) drainAllTaskNotifications() []taskNotification {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := append(s.taskNotificationsInFlight, s.taskNotifications...) //nolint:gocritic // deliberately combining, not appending in place — both are cleared immediately below
	s.taskNotifications = nil
	s.taskNotificationsInFlight = nil
	return all
}

// persistDeliveredTaskNotifications durably writes a recTaskNotifyDelivered
// record for each of ns, on s's own log — the deferred, explicit
// counterpart to drainAllTaskNotifications' now memory-only drain, for
// the two callers (finalizeTurn, recoverInterruptedTurnLocked) that
// re-home a terminal child's own pending notifications onto a DIFFERENT
// session (the nearest live ancestor). Call only once the notifications
// have actually been (re-)enqueued on that different session — this is
// what stops THIS session's own now-unmatched recTaskNotifyQueued records
// from resurrecting as phantom pending notifications on a future
// LoadSession of this exact child (e.g. a session.send re-run of a
// done/failed child), which would re-deliver to it results its ancestor
// already received. A no-op for an empty ns, so callers can call it
// unconditionally without checking len(ns) first.
func (s *Session) persistDeliveredTaskNotifications(ns []taskNotification) {
	if len(ns) == 0 {
		return
	}
	s.mu.Lock()
	for _, n := range ns {
		s.persistTaskNotifyLocked(recTaskNotifyDelivered, n)
	}
	s.mu.Unlock()
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
	// Durable proof of ACTUAL delivery, one record per notification —
	// see recTaskNotifyDelivered's own doc comment (store.go). Written
	// before clearing, matching every other persist-then-mutate ordering
	// in this package (a write failure here lands in s.lastPersistErr,
	// same as any other persist call, never blocks the in-memory commit).
	for _, n := range s.taskNotificationsInFlight {
		s.persistTaskNotifyLocked(recTaskNotifyDelivered, n)
	}
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
// never raw CHILD text), but it does now carry the provider's own error
// message as its cause half, and a provider error can be multi-line, so
// this pass is load-bearing for FailReason too, not only for a child's
// Result. Applied uniformly, so there is exactly one rule for "text that
// lands inside a [tasks: ...] line," not one rule per field that could
// drift.
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
