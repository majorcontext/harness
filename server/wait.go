package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/majorcontext/harness/engine"
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

	if state, goal, queued := s.waitSnapshot(id); waitConditionMet(until, state, goal, queued) {
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
			state, goal, _ := s.waitSnapshot(id)
			writeJSON(w, http.StatusOK, waitJSON{State: state, Goal: goal})
			return
		case <-timer.C:
			state, goal, _ := s.waitSnapshot(id)
			writeJSON(w, http.StatusOK, waitJSON{State: state, Goal: goal})
			return
		case <-wt.ch:
			state, goal, queued := s.waitSnapshot(id)
			met := waitConditionMet(until, state, goal, queued)
			if s.waitWakeCheckedRace != nil {
				// Test-only seam — see its own doc comment (server.go).
				// Fires AFTER the condition has been evaluated, carrying
				// the outcome (met or not) — so a test can deterministically
				// both confirm this specific wake has been fully processed,
				// AND assert what it decided, before letting whatever it
				// was racing proceed.
				s.waitWakeCheckedRace(met)
			}
			if met {
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

// waitSnapshot resolves the current composite state, goal summary, and
// prompt-queue depth for a session from the same source Session JSON uses
// (Server.goalState, this process's live tracker), so /wait's response
// agrees with GET /session/{id}. queued is read here too — not folded into
// state itself, which must stay exactly what GET /session/{id} already
// reports (an idle-with-queue session is documented and tested as State
// "idle" — see TestIdlePromptWithQueueGoesFIFO) — only waitConditionMet's
// own until=idle case (below) treats it specially.
//
// s.mu is released BEFORE the QueuedPrompts() call, deliberately: that
// call acquires sess's OWN lock (a DIFFERENT mutex, engine.Session.mu),
// and dequeueLocked (engine/queue.go) acquires the two in the OPPOSITE
// order — session.mu held, THEN server.mu (via its own emit -> OnEvent ->
// Publish -> emitDurable chain, journal.go). Calling QueuedPrompts() while
// still holding s.mu here would be a textbook lock-order-inversion
// deadlock the moment the two goroutines interleave — caught immediately
// (a real hang, not a guess) the first time this fix was tested under
// -race with any concurrent dispatch in flight. Reading running/sess/goal
// under one short s.mu hold, THEN releasing before the session-locked
// call, keeps the two mutexes strictly nested in only one direction
// system-wide.
func (s *Server) waitSnapshot(id string) (state string, goal *goalJSON, queued int) {
	s.mu.Lock()
	var running bool
	var sess *engine.Session
	if st := s.sessions[id]; st != nil {
		running = st.running
		sess = st.sess
	}
	goal = goalJSONFrom(s.goalState[id])
	s.mu.Unlock()
	if sess != nil {
		queued = len(sess.QueuedPrompts())
	}
	return compositeState(running, goal != nil && goal.Active, forcesIdlePause(goal)), goal, queued
}

// waitConditionMet reports whether the requested `until` condition holds
// given a freshly computed composite state, goal summary, and queue depth.
func waitConditionMet(until, state string, goal *goalJSON, queued int) bool {
	switch until {
	case "idle":
		// state == "idle" alone is not sufficient: a session reads
		// not-running with its prompt queue still non-empty during the
		// real, reproducible gap between freeRunSlotAndEmitIdle's own
		// idle transition and maybeDispatchQueued's own immediate
		// re-claim of the queue's next head (runPrompt's tail,
		// handlers.go — the two are not one atomic operation). A live
		// review finding, caught via a genuinely reproduced CI failure
		// (TestQueueLenExplicitOnEmptyingDequeue: a waiter woken by
		// that transient idle event observed it, and returned, BEFORE
		// the queue's own next item had even been dequeued yet — a
		// caller polling GET /session/{id} instead never had this
		// problem, since a snapshot read has no "woken early" moment to
		// race). until=idle means "nothing left to do, safe to treat
		// this session as settled" — a non-empty queue on an otherwise
		// idle session directly contradicts that: it is already,
		// unconditionally, about to resume on its own, whether or not
		// anyone is watching.
		return state == "idle" && queued == 0
	case "goal-done":
		return goal == nil || !goal.Active
	default:
		return false
	}
}
