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
	d := time.Duration(n) * time.Second
	if d > maxWaitTimeout {
		d = maxWaitTimeout
	}
	return d, nil
}

var errInvalidTimeout = waitTimeoutError{}

// waitTimeoutError is a fixed sentinel so parseWaitTimeout needs no fmt
// import for a message that never varies.
type waitTimeoutError struct{}

func (waitTimeoutError) Error() string { return "timeout_s must be a positive integer" }

// waitSnapshot resolves the current composite state and goal summary for a
// session from the same source Session JSON uses (Server.goalState, this
// process's live tracker), so /wait's response agrees with GET
// /session/{id}.
func (s *Server) waitSnapshot(id string) (string, *goalJSON) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var running bool
	if st := s.sessions[id]; st != nil {
		running = st.running
	}
	var goal *goalJSON
	if g := s.goalState[id]; g != nil {
		goal = &goalJSON{Condition: g.condition, Active: g.active, Achieved: g.achieved, Turns: g.turns, LastReason: g.lastReason, Attempt: g.attempt}
	}
	return compositeState(running, goal != nil && goal.Active), goal
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
