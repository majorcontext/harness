// Prompt queue: a durable per-session FIFO for prompts submitted while the
// session is busy (see docs/plans/2026-07-19-prompt-queue.md).
//
// A queued prompt is NOT a message. It lives entirely in Session.promptQueue
// and the prompt.queued/prompt.dequeued records (see store.go's
// promptRecord) until it is delivered — either as a normal Prompt call at
// idle drain, or prepended as a labeled operator interjection at a goal
// loop's turn boundary (both later tasks; see the plan). Until then it is
// absent from s.history and from every provider request: the plan's locked
// design decision is that a queued prompt must never leak into a running
// turn's context ahead of its actual delivery.
//
// EnqueuePrompt/DequeuePrompt follow goal.go's RegisterGoal/UpdateGoal shape
// exactly: persist the durable record and emit the engine event in the same
// critical section, under s.mu, so the event stream (and anything derived
// from it, e.g. a server's SSE journal) can never observe an event without
// the record that explains it already durable, or vice versa.
package engine

import (
	"errors"
	"strings"
)

// QueuedPrompt is one pending prompt in a session's durable FIFO queue (see
// EnqueuePrompt). ID is monotonic within the session (starting at 1),
// assigned at enqueue time and persisted (see promptRecord) so a resumed
// session's queue folds back in the exact same order — see LoadSession's
// recPromptQueued case.
type QueuedPrompt struct {
	ID   int64
	Text string
}

// EnqueuePrompt appends text to the session's durable FIFO prompt queue: it
// assigns the next monotonic ID, persists a prompt.queued record, and emits
// EventPromptQueued — all under s.mu (RegisterGoal's persist-and-emit-while-
// holding-mu shape, see goal.go), then returns the assigned ID. text is
// rejected (a no-op: no ID assigned, nothing persisted or emitted) if empty
// or whitespace-only, matching RegisterGoal's non-empty-condition rule. The
// stored/emitted text is trimmed, same as a goal condition.
//
// The enqueued prompt does not touch s.history and is not visible to any
// provider request started before it is actually delivered (see
// DequeuePrompt/dequeueAllLocked) — see the package doc comment.
func (s *Session) EnqueuePrompt(text string) (int64, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, errors.New("engine: EnqueuePrompt requires non-empty text")
	}
	s.mu.Lock()
	id := s.promptQueueNextID
	s.promptQueueNextID++
	s.promptQueue = append(s.promptQueue, QueuedPrompt{ID: id, Text: trimmed})
	s.persistPromptQueueLocked(recPromptQueued, promptRecord{ID: id, Text: trimmed})
	// Emit while still holding s.mu (see ClearGoal in goal.go): keeps event
	// order matching log order under a concurrent dequeue. OnEvent must not
	// call back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventPromptQueued, QueueID: id, QueueText: trimmed, QueueLen: len(s.promptQueue)})
	s.mu.Unlock()
	return id, nil
}

// DequeuePrompt pops the head of the FIFO queue (the lowest-ID pending
// prompt), persists a prompt.dequeued record carrying reason, and emits
// EventPromptDequeued — under s.mu, mirroring EnqueuePrompt's persist-and-
// emit shape. ok is false when the queue is empty: a clean no-op, nothing
// persisted or emitted.
//
// reason is one of "delivered" (idle dispatch, Task 3), "injected" (goal-
// turn-boundary interjection, Task 2), or "cleared" (DELETE
// /session/{id}/queue, Task 3) — this package does not validate the value,
// it is simply carried through to the record and event.
func (s *Session) DequeuePrompt(reason string) (p QueuedPrompt, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dequeueLocked(reason)
}

// dequeueLocked is DequeuePrompt's implementation with the lock already held
// by the caller — used directly by dequeueAllLocked below so a full-queue
// drain journals every record within one critical section, atomically with
// respect to a concurrent EnqueuePrompt.
func (s *Session) dequeueLocked(reason string) (QueuedPrompt, bool) {
	if len(s.promptQueue) == 0 {
		return QueuedPrompt{}, false
	}
	p := s.promptQueue[0]
	s.promptQueue = s.promptQueue[1:]
	s.persistPromptQueueLocked(recPromptDequeued, promptRecord{ID: p.ID, Text: p.Text, Reason: reason})
	// Emit while still holding s.mu (see EnqueuePrompt above): keeps event
	// order matching log order. OnEvent must not call back into this
	// Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventPromptDequeued, QueueID: p.ID, QueueText: p.Text, QueueReason: reason, QueueLen: len(s.promptQueue)})
	return p, true
}

// dequeueAllLocked drains the entire queue in FIFO order, journaling one
// prompt.dequeued record per item (all sharing reason) within a single s.mu
// critical section — for goal-turn-boundary injection (Task 2, which drains
// under the same lock it snapshots goal state with) and the DELETE
// /session/{id}/queue clear surface (Task 3). Caller already holds s.mu
// (unlike DequeuePrompt, which takes the lock itself) — the "Locked" suffix
// follows this package's existing convention for such methods (see
// persistGoalLocked). Returns the drained prompts in FIFO order, nil if the
// queue was already empty.
func (s *Session) dequeueAllLocked(reason string) []QueuedPrompt {
	var drained []QueuedPrompt
	for {
		p, ok := s.dequeueLocked(reason)
		if !ok {
			break
		}
		drained = append(drained, p)
	}
	return drained
}

// QueuedPrompts returns a copy of the session's pending prompt queue, in
// FIFO order (lowest ID first).
func (s *Session) QueuedPrompts() []QueuedPrompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]QueuedPrompt(nil), s.promptQueue...)
}
