package server

import (
	"net/http"
	"strconv"
	"time"
)

// waiter is one in-flight GET /session/{id}/wait long-poll, registered in
// Server.waiters for the request's duration. ch is woken (non-blocking,
// buffered 1 so wakes coalesce) by notifyWaitersLocked after every durable
// event for session; the waiter never trusts the event's payload, it always
// re-derives state fresh via waitSnapshot.
type waiter struct {
	ch      chan struct{}
	session string
}

const (
	defaultWaitTimeout = 30 * time.Second
	maxWaitTimeout     = 300 * time.Second
)

// waitJSON is the GET /session/{id}/wait response: the same composite state
// and goal summary shapes as Session JSON.
type waitJSON struct {
	State string    `json:"state"`
	Goal  *goalJSON `json:"goal,omitempty"`
}

// handleWait long-polls a session's composite state: it returns immediately
// if the requested condition already holds, otherwise it blocks — parked on a
// channel woken by the existing durable-event fanout (see
// notifyWaitersLocked), never by server-side polling — until the condition
// holds, timeout_s elapses (default 30s, capped at 300s), or the server
// begins draining/shutdown (s.closing), whichever comes first; a drain-driven
// return, like a timeout, carries the current best-effort snapshot and may
// not satisfy the requested condition — the caller distinguishes it the same
// way, by checking the returned state/goal.
//
// until=idle waits for the composite state to read idle (not busy, and no
// active goal). until=goal-done waits for the goal to become inactive —
// achieved or cleared, distinguished in the response's goal.achieved field,
// exactly as Session JSON does — or, if no goal was ever set for this
// session, is trivially already true (there is nothing to wait for).
//
// The waiter is registered BEFORE the immediate condition check (not after),
// so an event racing the check can never be missed: it either lands before
// registration (the immediate check already reflects it) or after (the
// waiter is already in Server.waiters to receive the wake). It is
// unregistered via defer on every return path, including a client disconnect
// (r.Context().Done()) — so a dropped connection cannot leak a waiter.
func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	until := r.URL.Query().Get("until")
	if until != "idle" && until != "goal-done" {
		writeErr(w, http.StatusBadRequest, "until must be idle or goal-done")
		return
	}
	timeout, err := parseWaitTimeout(r.URL.Query().Get("timeout_s"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	_, resident := s.sessions[id]
	s.mu.Unlock()
	if !resident && !s.sessionOnDisk(id) {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}

	wt := &waiter{ch: make(chan struct{}, 1), session: id}
	s.mu.Lock()
	s.waiters[wt] = struct{}{}
	s.mu.Unlock()
	if s.waitRegisteredRace != nil {
		// Test-only seam — see its own doc comment (server.go). Fires
		// right after registration, before the immediate condition
		// check below — lets a test confirm this waiter is actually in
		// Server.waiters (so a later notifyWaitersLocked wake will
		// reach it) before triggering whatever wake it means to race.
		s.waitRegisteredRace()
	}
	defer func() {
		s.mu.Lock()
		delete(s.waiters, wt)
		s.mu.Unlock()
	}()

	if state, goal := s.waitSnapshot(id); waitConditionMet(until, state, goal) {
		writeJSON(w, http.StatusOK, waitJSON{State: state, Goal: goal})
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-r.Context().Done():
			// Client disconnected (or the server's own request context ended);
			// nothing to write, the deferred unregister above prevents a leak.
			return
		case <-s.closing:
			// Drain has begun: respond with the current best-effort snapshot
			// rather than hold the connection open past shutdown.
			state, goal := s.waitSnapshot(id)
			writeJSON(w, http.StatusOK, waitJSON{State: state, Goal: goal})
			return
		case <-timer.C:
			state, goal := s.waitSnapshot(id)
			writeJSON(w, http.StatusOK, waitJSON{State: state, Goal: goal})
			return
		case <-wt.ch:
			state, goal := s.waitSnapshot(id)
			if waitConditionMet(until, state, goal) {
				writeJSON(w, http.StatusOK, waitJSON{State: state, Goal: goal})
				return
			}
			// Not yet: a durable event fired but didn't satisfy `until` (e.g. a
			// goal.eval that left the goal active) — loop and keep waiting.
		}
	}
}

// parseWaitTimeout resolves timeout_s: empty defaults to 30s; any positive
// integer is accepted and silently capped at 300s (never rejected for being
// too large — a generous client asking for the moon still gets a bounded
// wait); anything else (non-integer, zero, negative) is a 400.
func parseWaitTimeout(raw string) (time.Duration, error) {
	if raw == "" {
		return defaultWaitTimeout, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errInvalidTimeout
	}
	// Cap the integer seconds BEFORE converting to a Duration: n * time.Second
	// overflows int64 for n >~ 9.2e9 (e.g. timeout_s=10000000000), wrapping to
	// a negative Duration that would slip past a post-multiply "> maxWaitTimeout"
	// check and make time.NewTimer fire immediately — the opposite of the
	// documented bounded-wait contract for oversized requests.
	if maxSecs := int(maxWaitTimeout / time.Second); n > maxSecs {
		n = maxSecs
	}
	return time.Duration(n) * time.Second, nil
}

var errInvalidTimeout = waitTimeoutError{}

// waitTimeoutError is a fixed sentinel so parseWaitTimeout needs no fmt
// import for a message that never varies.
type waitTimeoutError struct{}

func (waitTimeoutError) Error() string { return "timeout_s must be a positive integer" }

// waitSnapshot resolves the current composite state and goal summary for a
// session from the same source Session JSON uses (Server.goalState, this
// process's live tracker), so /wait's response agrees with GET
// /session/{id} — with one deliberate difference: it also folds in
// Server.queueDrainPending, treating it exactly like st.running for this
// method's own purposes only.
//
// Why: freeRunSlotAndEmitIdle (handlers.go) always durably emits "idle" at
// the end of a turn, queue or no queue — collectUntilIdle (server_test.go)
// and every test built on it depend on that ordering, so the event is never
// suppressed. That means a GET /session/{id}/wait?until=idle waiter woken by
// it can genuinely observe running=false with the queue still non-empty, in
// the brief window before maybeDispatchQueued (called right after, same
// tail) redispatches the head — a live, reproduced CI failure
// (TestQueueLenExplicitOnEmptyingDequeue) caught exactly this. queueDrainPending
// is true for precisely that window (set by freeRunSlotAndEmitIdle, cleared
// by maybeDispatchQueued's own clearQueueDrainPending once it resolves), so
// folding it in here closes the gap without changing what GET /session/{id}
// itself reports (its State field is deliberately untouched — idle-with-
// queue stays "idle" there, matching TestIdlePromptWithQueueGoesFIFO).
//
// This also avoids two earlier, rejected approaches: reading queue depth via
// engine.Session.QueuedPrompts() directly here needs sess's OWN separate
// lock, which — acquired while s.mu was released to sidestep the resulting
// lock-order-inversion deadlock (dequeueLocked takes the two locks in the
// OPPOSITE order) — made the running/queued reads non-atomic, a narrower
// version of the same false-idle. And gating naively on queue depth alone
// (empty or not) is simply wrong: a session resumed after a restart with a
// non-empty queue and nothing running is genuinely idle right now (AGENTS.md:
// "Boot never auto-dispatches a resumed queue... it sits there until the
// next natural drain trigger") — loadJournal never sets queueDrainPending,
// so that case is unaffected here and still returns idle immediately.
//
// running also falls back to SessionManager's live status, but ONLY when
// id is not resident at all — never merely because queueDrainPending and
// the resident running flag both read false. A Spawn-driven child is
// never a key in s.sessions (Spawn drives its turn directly, never through
// claimForPrompt — see liveSession's doc comment for the identical
// reasoning on the journaling path). Without the fallback, a mid-turn
// child reads running=false here and until=idle returns a false "idle"
// immediately.
//
// liveSession.status() applies that residency gate, once, for every
// caller: a resident session answers from its own running flag and the
// manager half is read only for an id residency does not know at all.
// Gating on residency is load-bearing, not a belt-and-suspenders extra
// check — a live review finding caught a real regression from an earlier
// revision that fell back whenever !running, resident or not.
// freeRunSlotAndEmitIdle (handlers.go) sets st.running = false and wakes
// waiters BEFORE ReportTurnEnd flips the SessionManager node off
// StatusRunning (the two calls are deliberately ordered that way — see
// runPrompt's own doc comment). A waiter woken in that gap used to read
// st.running == false correctly, then see this fallback (ungated) still
// find sessMgr's node StatusRunning and report busy — a genuinely wrong
// answer for an ordinary resident session's prompt-then-wait flow, not
// just a promptness gap, and the waiter then hangs to timeout_s with
// nothing left to wake it (the idle event already fired once). A resident
// session's own st.running is always the authoritative, race-free answer
// for itself; the fallback exists ONLY to cover the one case residency
// has no answer for at all. sessMgr.Info is consulted OUTSIDE s.mu:
// server.mu stays a leaf lock (see syncMessages' lock-ordering note), and
// no established order exists between server.mu and SessionManager.mu to
// rely on.
//
// Known residual, accepted for this fix's scope (a live review finding):
// a GET /session/{childID}/wait?until=idle waiter can still block until
// timeout rather than returning promptly the instant a Spawn-driven
// child's turn actually settles. The child's last EventMessage wakes the
// waiter (via notifyWaitersLocked) BEFORE SessionManager.finalizeTurn
// (called from Spawn's own goroutine, after child.Prompt returns) flips
// its node's status away from StatusRunning — finalizeTurn emits no
// server-level durable event on the child's own id (recordTurnEnd is
// server-side glue only runPrompt/runGoal/handleCompact call, all
// root-only paths; a Spawn-driven child's completion is instead delivered
// to its PARENT as a task notification). A waiter that loses that race
// re-parks on wt.ch with nothing left to wake it on the child's own id,
// and returns only once timeout_s elapses — a promptness regression, not
// a wrong answer (waitSnapshot itself is correct at any instant it runs).
// Closing this needs a way for SessionManager to notify server-level
// waiters when a node it drives settles, independent of the server's own
// event journal — a genuinely new cross-package notification path, not
// this fix's residency-blindness bug class. Left as a follow-up.
func (s *Server) waitSnapshot(id string) (string, *goalJSON) {
	// The residency half, queueDrainPending, and goalState are read in ONE
	// s.mu hold: they are the three inputs to one composite answer, and a
	// second hold would let a turn start or a goal arm between them. The
	// manager half is completed after the unlock — see liveSession's own
	// doc comment on why the two halves cannot share one hold.
	s.mu.Lock()
	lv := s.liveResidentLocked(id)
	drainPending := s.queueDrainPending[id]
	goal := goalJSONFrom(s.goalState[id])
	s.mu.Unlock()
	lv = lv.withManager(s.sessMgr)
	running := drainPending || lv.status() == "busy"
	return compositeState(running, goal != nil && goal.Active, forcesIdlePause(goal)), goal
}

// waitConditionMet reports whether the requested `until` condition holds
// given a freshly computed composite state and goal summary.
func waitConditionMet(until, state string, goal *goalJSON) bool {
	switch until {
	case "idle":
		return state == "idle"
	case "goal-done":
		return goal == nil || !goal.Active
	default:
		return false
	}
}
