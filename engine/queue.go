// Queued prompts stay outside history and requests until delivery. Queue records and events share the session lock.
package engine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/majorcontext/harness/message"
)

// QueuedPrompt is one pending prompt in a session's durable FIFO queue (see
// EnqueuePrompt). ID is monotonic within the session (starting at 1),
// assigned at enqueue time and persisted (see promptRecord) so a resumed
// session's queue folds back in the exact same order — see LoadSession's
// recPromptQueued case.
type QueuedPrompt struct {
	ID   int64
	Text string
	// Seq is the caller-issued idempotency sequence for a prompt enqueued
	// via EnqueuePromptDurable (see store.go's promptRecord.Seq); 0 for a
	// plain EnqueuePrompt, which has no idempotency contract.
	Seq int64
	// MessageID is the ID the user message this prompt eventually becomes
	// will carry — already resolved (see ResolveMessageID) by EnqueuePrompt
	// at enqueue time, so it is stable and known before this prompt is ever
	// dispatched: a caller reporting a synchronous "queued" response can
	// promise the exact ID PromptWithOrigin will use later, at drain time,
	// with no risk of a second, different mint for the same prompt. Empty
	// on a record folded from an older session log written before this
	// field existed — PromptWithOrigin's own mint site resolves that case
	// exactly like any other unset id, at dispatch time.
	MessageID string
	// Blobs are the prompt's attachments — an uploaded image or PDF, the
	// set server/prompt_parts.go admits — kept beside Text rather than
	// folded into it because a Blob is binary content a provider
	// transcodes as its own wire block — see message.Blob. They are
	// persisted with the queued prompt (promptRecord.Blobs) and delivered
	// as Blob parts of the user message this prompt becomes, so a prompt
	// that waited behind a running turn, or behind a process restart,
	// still arrives with its files.
	//
	// Nil for every text-only prompt, which is still the overwhelming
	// majority: a queued prompt was text-only by contract until image
	// input landed (see docs/session-storage-and-queue.md).
	Blobs []*message.Blob
}

// promptQueueFold replays prompt.queued/prompt.dequeued records into the
// exact set of undelivered prompts, in the order live memory held them. It
// is the ONE implementation of that fold, shared by LoadSession (store.go)
// and the session metadata index (index.go), which needs the same depth
// without paying for a full replay.
//
// nextID and seq mirror Session.promptQueueNextID and Session.enqueueSeq: a
// caller seeds them with the session's current values and reads them back
// after the fold.
type promptQueueFold struct {
	queue  []QueuedPrompt
	nextID int64
	seq    int64
}

// queued folds one prompt.queued record.
//
// A record carrying Seq (durable enqueue) folds last-writer-wins against
// any already-folded entry with the SAME Seq: a failed fsync can leave a
// torn record on disk whose write reported failure, followed by its
// successful retry under a fresh ID — live memory only ever held the
// retry's entry, so replay must converge to that one too (a later
// prompt.dequeued references the retry's ID — this holds under
// EnqueuePromptDurable's caller contract that the same seq is retried
// before any higher seq is accepted). Seq also advances the enqueueSeq
// high-water mark, which is what makes duplicate detection survive a
// process restart.
//
// The fold REMOVES the old same-Seq entry from its slot and APPENDS the new
// one at the tail, rather than replacing it in place: a plain EnqueuePrompt
// can land BETWEEN the torn write and its retry (log order id1/seq5 torn,
// id2/seq0 plain, id3/seq5 retry). Live memory only ever appended id2 then
// id3, in that order — an in-place replacement at id1's old slot would
// instead fold to [id3, id2], reordering delivery relative to what actually
// happened live. Remove+append reconstructs live append order faithfully
// (the retry always carries the highest ID seen so far, so this can never
// misorder against a later, genuinely-newer plain entry); the common case
// with no interposed record degenerates to the exact same single-entry
// result as an in-place replacement.
//
// Malformed-record guards (found by FuzzLoadSessionReplay): the live path
// can never write a queued record with ID <= 0 (promptQueueNextID starts at
// 1) nor two records with the same ID (IDs are burned, never reused — see
// EnqueuePromptDurable), so either shape in a journal is corruption, not
// history. Folding them anyway would violate the queue's ID-uniqueness
// invariant (two ID-0 entries from two `{"prompt":{}}` lines) and a later
// dequeue-by-ID would remove an arbitrary one. Skip the record; same
// defensive posture as message.ResolveOrphanToolCalls at this layer.
//
// nextID advances past every ID this fold ACCEPTS, which is what keeps a
// resumed session's counter collision-free: IDs are burned on failed
// durable writes, so a counter must clear every ID that ever reached the
// log. A SKIPPED record does not advance it, and does not need to. A
// duplicate ID was already cleared by the record that folded first, and an
// ID at or below zero can never reach a counter that starts at 1. Do not
// "fix" this by hoisting the advance above the validity guard: that would
// let a malformed record's ID move the counter, which is exactly what the
// guard rejects it for.
func (f *promptQueueFold) queued(p promptRecord) {
	q := QueuedPrompt{ID: p.ID, Text: p.Text, Seq: p.Seq, MessageID: p.MessageID, Blobs: p.Blobs}
	valid := q.ID > 0
	for _, existing := range f.queue {
		if existing.ID == q.ID {
			valid = false
			break
		}
	}
	if !valid {
		return
	}
	if q.Seq > 0 {
		for i, existing := range f.queue {
			if existing.Seq == q.Seq {
				f.queue = append(f.queue[:i], f.queue[i+1:]...)
				break
			}
		}
		if q.Seq > f.seq {
			f.seq = q.Seq
		}
	}
	f.queue = append(f.queue, q)
	if p.ID >= f.nextID {
		f.nextID = p.ID + 1
	}
}

// dequeued folds one prompt.dequeued record: it removes the matching queued
// entry by ID, not by position (see promptRecord's doc comment), so the
// folded queue ends up exactly the undelivered set however many other
// records separate a queued record from its own dequeued record.
//
// This fold reads FORWARD only: a dequeued record for an ID not folded yet
// is a no-op, and a queued record arriving after it re-appends the item.
// Every writer therefore owes this fold one ordering guarantee — a queued
// record reaches disk before its own dequeued record. The two writers that
// defer a prompt-queue write out from under the tree-wide m.mu keep it by
// parking the record on the session, not in their own closure; see
// queueRecordDeferredLocked for the resurrection defect a closure-held
// record caused.
func (f *promptQueueFold) dequeued(p promptRecord) {
	for i, existing := range f.queue {
		if existing.ID == p.ID {
			f.queue = append(f.queue[:i], f.queue[i+1:]...)
			return
		}
	}
}

// ErrEmptyPromptText is returned for a prompt whose text is empty or
// whitespace-only. One shared sentinel, not a fresh errors.New per call
// site — a review finding: SessionManager.SendToDescendant validates the
// same rule for a running target (it enqueues through
// enqueueMemoryOnlyLocked, which assumes validated text), and a fresh
// value there could not be classified with errors.Is, so
// classifyTaskVerbError (task_tool.go) fell through to its default arm
// and leaked the internal "engine:" layer to the model.
var ErrEmptyPromptText = errors.New("engine: prompt text must not be empty or whitespace-only")

// EnqueuePrompt appends text to the session's durable FIFO prompt queue: it
// assigns the next monotonic ID, persists a prompt.queued record, and emits
// EventPromptQueued — all under s.mu (RegisterGoal's persist-and-emit-while-
// holding-mu shape, see goal.go), then returns the assigned ID. text is
// rejected (a no-op: no ID assigned, nothing persisted or emitted) if empty
// or whitespace-only, matching RegisterGoal's non-empty-condition rule. The
// stored/emitted text is trimmed, same as a goal condition.
//
// messageID is the caller's own (possibly empty, possibly client-supplied)
// message ID for the prompt; EnqueuePrompt resolves it exactly once, via
// ResolveMessageID, and both persists and returns that SAME resolved value
// — never resolved a second time at dispatch, which would risk minting a
// DIFFERENT fresh ID than whatever this call's own return value already
// promised a caller reporting a synchronous "queued" response. Pass "" for
// a caller with no client ID of its own (a server-minted ID is what
// PromptWithOrigin would have chosen anyway).
//
// The enqueued prompt does not touch s.history and is not visible to any
// provider request started before it is actually delivered (see
// DequeuePrompt/dequeueAllLocked) — see the package doc comment.
// blobs are the prompt's attachments, carried through the queue with it (see
// QueuedPrompt.Blobs). They are variadic so every existing text-only caller
// and its call sites stay untouched — one entry point still owns the enqueue
// rule, rather than a second near-identical method growing beside it. A
// prompt carrying at least one blob is valid with EMPTY text: an uploaded
// screenshot with nothing typed beside it is a real prompt, not an empty one.
// usablePromptBlobs drops the blobs a queued prompt cannot actually deliver:
// a nil entry, and one carrying neither Data nor URL (every provider
// transcoder errors on that shape regardless of media type — see
// message/wire_normalize.go's intersection comment).
//
// It runs BEFORE the empty-prompt check, because the raw count is not a
// count of attachments. A caller passing a single nil would otherwise
// satisfy "empty text is fine when a blob came with it", persist a prompt
// with an unusable Blobs slice, and then have operatorMessagesBlock
// announce "[1 attachment(s) attached below]" to the model while
// promptParts silently skipped the nil — the marker promising a file that
// never arrives, which is the same defect the claude-code drain had.
//
// Returns nil for an all-unusable input, so len() answers "how many
// attachments will really be delivered".
func usablePromptBlobs(blobs []*message.Blob) []*message.Blob {
	usable := make([]*message.Blob, 0, len(blobs))
	for _, b := range blobs {
		if b == nil || (len(b.Data) == 0 && b.URL == "") {
			continue
		}
		usable = append(usable, b)
	}
	if len(usable) == 0 {
		return nil
	}
	return usable
}

func (s *Session) EnqueuePrompt(text string, messageID string, blobs ...*message.Blob) (id int64, resolvedMessageID string, err error) {
	trimmed := strings.TrimSpace(text)
	usable := usablePromptBlobs(blobs)
	if trimmed == "" && len(usable) == 0 {
		return 0, "", ErrEmptyPromptText
	}
	resolved := ResolveMessageID(messageID)
	s.mu.Lock()
	p := s.enqueueMemoryOnlyLocked(trimmed, resolved, usable...)
	s.persistPromptQueueLocked(recPromptQueued, promptRecord{ID: p.ID, Text: p.Text, MessageID: p.MessageID, Blobs: p.Blobs})
	// Emit while still holding s.mu (see ClearGoal in goal.go): keeps event
	// order matching log order under a concurrent dequeue. OnEvent must not
	// call back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventPromptQueued, QueueID: p.ID, QueueText: p.Text, QueueLen: len(s.promptQueue)})
	s.mu.Unlock()
	return p.ID, p.MessageID, nil
}

// enqueueMemoryOnlyLocked is EnqueuePrompt's memory-only half: assigns
// the next monotonic queue ID and appends to s.promptQueue — WITHOUT
// EnqueuePrompt's own persistPromptQueueLocked call (a synchronous disk
// write: ensureLog + writeRecord) or its emit. Exists for a caller that
// must mutate s's queue from inside ANOTHER lock's own critical section
// without paying for that disk write under it —
// SessionManager.SendToDescendant's running-target branch, which holds
// the tree-wide m.mu across this call (see its own doc comment) and
// defers the matching persistPromptQueueLocked call via
// SessionManager.deferPersist/unlockAndFlushPersist instead, exactly
// like the task-notification delivery path already does for its own
// durable writes (commitOutcomeLocked, finalizeTurn's notify-delivery
// block). A live review finding: an earlier version of this fix called
// the full EnqueuePrompt (persist inline) from inside SendToDescendant's
// own m.mu-held block, stalling every OTHER session's Info/Reap/Spawn/
// finalize call on this ONE session's fsync for as long as it took.
//
// Deliberately does NOT emit: unlike the notification path (which never
// emits an event on enqueue at all), a queued prompt DOES have an
// observable event (EventPromptQueued, for the queue-depth UI) — the
// caller decides when to emit it relative to the (possibly deferred)
// persist, exactly as EnqueuePrompt itself does above (persist, then
// emit, unchanged order for its own direct callers).
//
// text is assumed already validated non-empty and trimmed — the one
// other caller (SendToDescendant) applies the same validation
// EnqueuePrompt does above, on its own copy of the text. messageID is
// stored as given — already resolved by EnqueuePrompt's own caller, or ""
// for a caller (SendToDescendant) with no client message ID of its own,
// left for PromptWithOrigin to resolve at dispatch time. Caller holds
// s.mu.
func (s *Session) enqueueMemoryOnlyLocked(text string, messageID string, blobs ...*message.Blob) QueuedPrompt {
	id := s.promptQueueNextID
	s.promptQueueNextID++
	p := QueuedPrompt{ID: id, Text: text, MessageID: messageID, Blobs: blobs}
	s.promptQueue = append(s.promptQueue, p)
	return p
}

// deferredQueueRecord is one prompt-queue record (see promptRecord in
// store.go) whose memory mutation is already applied but whose disk write
// is deferred — see queueRecordDeferredLocked.
type deferredQueueRecord struct {
	recType string
	prompt  promptRecord

	// event is the queue event this record's memory mutation owes its
	// subscribers, emitted by flushQueueRecordsLocked immediately after
	// the record is written. Parked with the record, not emitted at
	// mutation time — a review finding: the two m.mu-held mutation sites
	// emitted inline, and a subscriber can do real work on the call
	// (server.Server's Publish journals a prompt.queued/prompt.dequeued
	// event to events.jsonl, a synchronous disk write under its own
	// server.mu), which put that write back inside the tree-wide m.mu
	// this whole park/flush mechanism exists to keep clear of slow work.
	// Emitting from the flush keeps event order equal to record order for
	// a session — whichever s.mu holder drains the park emits the parked
	// event before its own — while no emit runs under m.mu at all.
	event Event
}

// queueRecordDeferredLocked parks one prompt-queue record for a write that
// runs LATER, outside the tree-wide m.mu — the durable half of
// enqueueMemoryOnlyLocked/dequeueMemoryOnlyLocked. The caller must park the
// record under the SAME s.mu hold as the memory mutation it describes, then
// arrange the flush via SessionManager.deferPersist/unlockAndFlushPersist
// (SendToDescendant's running-target enqueue, finalizeTurn's queued-message
// re-drive).
//
// Parking on the SESSION, not in the caller's own closure, is what keeps
// the log's record order equal to the memory-mutation order. A review
// finding proved the closure form wrong: SendToDescendant appends to the
// queue in memory under m.mu and defers the prompt.queued write, while the
// child's own turn goroutine — which needs only s.mu, never m.mu — can
// drain that very item at a tool-call boundary and write its
// prompt.dequeued record synchronously. The dequeued record then reached
// disk FIRST, and LoadSession's fold (which removes a dequeued item by ID
// and no-ops when its queued record has not folded yet) let the later
// queued record re-append the item: a reload resurrected an
// already-delivered prompt and the child ran it twice, breaking the
// queue's no-double-delivery invariant. Because a parked record lives on
// s and every prompt-queue write drains the park first
// (persistPromptQueueLocked), the competing dequeuer now writes the parked
// queued record before its own — whichever goroutine gets there first
// writes both, in order.
//
// A duplicate write is impossible: the flush removes what it wrote, so a
// dequeuer that drains the park makes the later deferred flush a no-op.
// Caller holds s.mu.
func (s *Session) queueRecordDeferredLocked(recType string, p promptRecord, ev Event) {
	s.deferredQueueRecords = append(s.deferredQueueRecords, deferredQueueRecord{recType: recType, prompt: p, event: ev})
}

// flushQueueRecordsLocked writes every parked prompt-queue record, in FIFO
// order, and clears the park. Called by persistPromptQueueLocked before its
// own write, and by the deferPersist thunk the parking caller queued. Cheap
// no-op when nothing is parked, which is every ordinary session's steady
// state. Caller holds s.mu.
func (s *Session) flushQueueRecordsLocked() {
	if len(s.deferredQueueRecords) == 0 {
		return
	}
	pending := s.deferredQueueRecords
	s.deferredQueueRecords = nil
	for _, r := range pending {
		s.writePromptQueueRecordLocked(r.recType, r.prompt)
		// Emit right after the write, still under s.mu — the same
		// persist-then-emit order EnqueuePrompt/dequeueLocked use for
		// their own inline records, so event order matches log order for
		// this session either way. An empty Type means a caller parked a
		// record with no event to emit; nothing in this package does that
		// today, and a zero Event must never reach a subscriber.
		if r.event.Type != "" {
			s.emit(r.event)
		}
	}
}

// EnqueuePromptDurable is EnqueuePrompt with an honest durability and
// idempotency contract, for callers (an inbox poller, a coordinator relay)
// whose OWN upstream ack rides on this call's success — see
// docs/plans/2026-07-21-durable-enqueue.md:
//
//   - seq is a caller-issued, session-monotonic idempotency sequence. At or
//     below the current high-water mark (EnqueueSeq) the call is a clean
//     duplicate no-op — nothing persisted, emitted, or enqueued — so
//     upstream retries are always safe. The caller must issue seqs for one
//     session in nondecreasing order; a gap is fine (the mark jumps), an
//     out-of-order fresh seq is indistinguishable from a duplicate and is
//     dropped.
//   - The prompt.queued record (carrying seq) is written, and — in the
//     default fsync mode (Config.SessionSync) — ALSO fsynced, before any
//     queue/watermark mutation and before success returns: write-ahead,
//     unlike every other persist path in this package, which buffers to the
//     page cache and swallows errors into lastPersistErr. In "volume" mode
//     the fsync round-trip is skipped (see volumeSync in store.go and
//     Config.SessionSync's doc comment) — the write(2) landing IS the
//     attestation there, and durability is delegated to the volume's own
//     continuous-sync commit layer. Either way, an error return means "not
//     durably accepted; retry with the same seq"; only a nil error
//     authorizes the caller to ack upstream.
//   - The assigned queue ID is burned BEFORE the write is attempted, and so
//     stays burned on every failure path (ensureLog opening/creating the
//     log, the write itself, or the fsync when fsync mode is active): any of
//     those may still have left a torn trace on disk (a half-created file, a
//     partially written record), and reusing the ID for a later plain
//     enqueue would fold two different prompts under one ID on replay.
//     LoadSession converges a torn record and its successful same-seq retry
//     last-writer-wins — see the recPromptQueued replay case in store.go.
//     This healing is mode-independent: a volume can still lose an
//     unsynced tail on abrupt death exactly like a torn fsync can, and the
//     same fold repairs both.
//
// Delivery of the enqueued prompt is unchanged queue machinery: FIFO with
// plain-enqueued prompts, drained at idle dispatch or injected at
// tool-call/goal-turn boundaries.
//
// A nil error attests durable ACCEPTANCE into the session's queue per the
// configured Config.SessionSync mode: fsynced in the default mode, or
// delegated to the volume layer's continuous sync in "volume" mode — so a
// retry with the same seq is always a safe no-op from here on either way.
// It is not a delivery receipt: once the queue's existing machinery
// dequeues this prompt for a turn, it carries the exact same crash exposure
// as any in-flight prompt (see the server's maybeDispatchQueued,
// "No-double-delivery equivalence", invariant 7). A crash between that
// dequeue record and the turn's completion loses the delivery — it is
// never redelivered — while the watermark correctly continues to report
// the message as accepted: lose-once-on-crash, not deliver-twice.
//
// blobs are the prompt's attachments, carried through exactly like
// EnqueuePrompt's own blobs parameter (see its doc comment): variadic so
// every existing text-only caller and call site stays untouched, filtered
// through usablePromptBlobs BEFORE the emptiness check so a caller passing
// only unusable blobs (nil, or one with neither Data nor URL) cannot
// satisfy "empty text is fine when a blob came with it" and durably persist
// a prompt.queued record promising an attachment that can never be
// delivered. They ride on the SAME seq as the prompt's text — there is no
// separate idempotency key for an attachment, so a retry that resends the
// identical seq is required to resend the identical blobs too (the caller
// reconstructs the same request on retry; this method has no way to detect
// a seq reused with DIFFERENT content, same as it already has none for text).
func (s *Session) EnqueuePromptDurable(text string, seq int64, blobs ...*message.Blob) (id int64, duplicate bool, err error) {
	trimmed := strings.TrimSpace(text)
	usable := usablePromptBlobs(blobs)
	if trimmed == "" && len(usable) == 0 {
		return 0, false, ErrEmptyPromptText
	}
	if seq < 1 {
		return 0, false, errors.New("engine: EnqueuePromptDurable requires seq >= 1")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq <= s.enqueueSeq {
		return 0, true, nil
	}
	if s.cfg.SessionDir == "" {
		return 0, false, errors.New("engine: EnqueuePromptDurable requires Config.SessionDir")
	}
	// Burn the ID now, before any I/O is attempted, so every failure path
	// below advances the counter past it — see the doc comment above.
	id = s.promptQueueNextID
	s.promptQueueNextID++
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return 0, false, err
	}
	// Drain the park before this record, exactly as persistPromptQueueLocked
	// does for every other prompt-queue write — a review finding: this
	// method writes through writeRecord directly (it owns its own fsync and
	// error contract), so it was the one writer that could put its own
	// queued record ahead of a still-parked one. Memory order stayed [A, B]
	// while disk order became [queued(B), queued(A)], and LoadSession's fold
	// appends in record order and never sorts by ID, so a reload restored
	// [B, A] — a FIFO reorder across a restart. See
	// queueRecordDeferredLocked's own doc comment for the park itself.
	//
	// Ordering only: a parked record carries the ordinary best-effort
	// durability every non-durable enqueue has, so this drain neither
	// weakens nor strengthens this method's own write-ahead contract for
	// the record it is about to write.
	s.flushQueueRecordsLocked()
	const op = "enqueue_durable"
	rec := record{Type: recPromptQueued, Prompt: &promptRecord{ID: id, Text: trimmed, Seq: seq, Blobs: usable}}
	if err := s.timedStorePhase(op, "write_record", func() error {
		return s.writeRecord(rec)
	}); err != nil {
		s.lastPersistErr = err
		return 0, false, err
	}
	// Skipped entirely in volume mode (see store.go's volumeSync): the
	// write(2) above is unchanged in both modes, but this fsync round-trip —
	// and its phase event — only happen in fsync mode. See Config.
	// SessionSync's doc comment for why: on a continuously-synced network
	// volume, this fsync would add no durability the volume's own commit
	// layer doesn't already provide, and some FUSE/9p transports deadlock
	// permanently on it.
	if !s.volumeSync() {
		if err := s.timedStorePhase(op, "fsync", func() error {
			return s.logFile.Sync()
		}); err != nil {
			// The record may or may not have reached stable storage — torn
			// state. Nothing in memory moved, so a retry with the same seq is
			// clean here; replay's last-writer-wins fold heals the disk side.
			s.lastPersistErr = err
			return 0, false, err
		}
	}
	s.promptQueue = append(s.promptQueue, QueuedPrompt{ID: id, Text: trimmed, Seq: seq, Blobs: usable})
	s.enqueueSeq = seq
	// Emit while still holding s.mu (see EnqueuePrompt above): keeps event
	// order matching log order under a concurrent dequeue. OnEvent must not
	// call back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventPromptQueued, QueueID: id, QueueText: trimmed, QueueSeq: seq, QueueLen: len(s.promptQueue)})
	return id, false, nil
}

// DequeuePrompt pops the head of the FIFO queue (the lowest-ID pending
// prompt), persists a prompt.dequeued record carrying reason, and emits
// EventPromptDequeued — under s.mu, mirroring EnqueuePrompt's persist-and-
// emit shape. ok is false when the queue is empty: a clean no-op, nothing
// persisted or emitted.
//
// remaining is len(s.promptQueue) immediately after the dequeue above,
// computed under the SAME s.mu hold as the dequeue itself — the caller's
// one atomic answer to "how many are left," rather than a second,
// separately-locked QueuedPrompts() call. A live review finding: a
// caller that dequeued here and THEN called QueuedPrompts() as a
// follow-up reintroduced a narrower version of the exact
// dispatchQueueHead race that PR fixed (server/handlers.go) — a
// DIFFERENT dequeue (a concurrent DELETE /session/{id}/queue, another
// dispatch) can interleave in the gap between the two separately-locked
// calls, same as re-reading QueuedPrompts() after spawning runPrompt
// could observe a queue already drained further than this exact call
// left it. Returning it as part of this same locked operation removes
// that gap entirely.
//
// reason is one of "delivered" (idle dispatch, Task 3), "injected" (goal-
// turn-boundary interjection, Task 2), or "cleared" (DELETE
// /session/{id}/queue, Task 3) — this package does not validate the value,
// it is simply carried through to the record and event.
func (s *Session) DequeuePrompt(reason string) (p QueuedPrompt, remaining int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dequeueLocked(reason)
}

// dequeueLocked is DequeuePrompt's implementation with the lock already held
// by the caller — used directly by dequeueAllLocked below so a full-queue
// drain journals every record within one critical section, atomically with
// respect to a concurrent EnqueuePrompt.
func (s *Session) dequeueLocked(reason string) (QueuedPrompt, int, bool) {
	p, ok := s.dequeueMemoryOnlyLocked()
	if !ok {
		return QueuedPrompt{}, 0, false
	}
	s.persistPromptQueueLocked(recPromptDequeued, promptRecord{ID: p.ID, Text: p.Text, Reason: reason})
	remaining := len(s.promptQueue)
	// Emit while still holding s.mu (see EnqueuePrompt above): keeps event
	// order matching log order. OnEvent must not call back into this
	// Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventPromptDequeued, QueueID: p.ID, QueueText: p.Text, QueueReason: reason, QueueLen: remaining})
	return p, remaining, true
}

// dequeueMemoryOnlyLocked is dequeueLocked's memory-only half: pops the
// queue head, if any — WITHOUT the persist or emit dequeueLocked itself
// always does inline. See enqueueMemoryOnlyLocked's own doc comment for
// why this split exists and who needs it:
// SessionManager.finalizeTurn's own queued-message re-drive check, which
// — like SendToDescendant's enqueue — holds the tree-wide m.mu across
// this call and must not pay for persistPromptQueueLocked's synchronous
// disk write under it. Caller holds s.mu.
func (s *Session) dequeueMemoryOnlyLocked() (QueuedPrompt, bool) {
	if len(s.promptQueue) == 0 {
		return QueuedPrompt{}, false
	}
	p := s.promptQueue[0]
	s.promptQueue = s.promptQueue[1:]
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
		p, _, ok := s.dequeueLocked(reason)
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

// EnqueueSeq returns the durable-enqueue high-water mark: the largest seq
// accepted by EnqueuePromptDurable, live or restored by LoadSession's
// replay. A caller recovering after ITS OWN crash reads this to learn which
// messages are already inside the durability domain and must not be re-sent
// as fresh (they would be deduplicated anyway — this is the read that lets
// it skip the round-trip).
func (s *Session) EnqueueSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enqueueSeq
}

// QueueState returns the durable-enqueue watermark and a copy of the
// pending prompt queue in one critical section, so the pair is internally
// consistent — a reconciliation reader (GET /session/{id}/queue) must never
// observe a queued entry whose Seq exceeds the watermark it was returned
// with, which the separate EnqueueSeq/QueuedPrompts calls cannot guarantee
// under a concurrent EnqueuePromptDurable.
func (s *Session) QueueState() (watermark int64, prompts []QueuedPrompt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enqueueSeq, append([]QueuedPrompt(nil), s.promptQueue...)
}

// DequeueAllPrompts is dequeueAllLocked's exported, self-locking wrapper —
// the whole-queue counterpart to DequeuePrompt, for callers that need to
// drain everything atomically in one critical section rather than one item
// at a time. Used by goal-turn-boundary injection (Task 2, goal.go's
// PursueGoal, reason "injected") and the DELETE /session/{id}/queue clear
// surface (Task 3, reason "cleared").
func (s *Session) DequeueAllPrompts(reason string) []QueuedPrompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dequeueAllLocked(reason)
}

// operatorContext selects operatorMessagesBlock's trailing clause so the
// header never tells a plain turn to "continue the goal" when there isn't
// one.
type operatorContext string

const (
	// operatorContextTask is engine.go's Prompt loop's mid-turn tool-call-
	// boundary drain: a plain turn (or a goal loop's worker turn, which
	// runs through this exact same loop — see PursueGoal/
	// promptTurnWithRetry) has no goal-shaped directive to hand back to,
	// only "the task" it was already doing.
	operatorContextTask operatorContext = "task"
	// operatorContextGoal is goal.go's PursueGoal turn-boundary drain,
	// where the block is prepended to an actual goal condition/guidance
	// directive.
	operatorContextGoal operatorContext = "goal"
)

// operatorMessagesBlock renders a batch of prompts drained by
// DequeueAllPrompts as a labeled, numbered block meant to be prepended
// ahead of — never substituted for — whatever text the drain site is about
// to deliver. The label makes the operator origin explicit to the worker
// model (these are direct human/API input arriving mid-loop, distinct from
// a goal condition, evaluator feedback, or the model's own turn), and the
// loop is fully independent of ordering: prompts is already FIFO-ordered by
// DequeueAllPrompts/dequeueAllLocked, so numbering here just mirrors that
// order rather than establishing it.
//
// Two call sites share this exact template, parameterized only by ctx's
// trailing clause, so a drained batch renders identically apart from that
// one word no matter which boundary delivered it: goal.go's PursueGoal
// prepends it (operatorContextGoal) to a turn's directive/guidance at the
// goal's own turn boundary; engine.go's Prompt loop appends it
// (operatorContextTask) as a standalone user message at a mid-turn
// tool-call boundary (see the "Design amendment: tool-call-boundary
// injection" note in docs/plans/2026-07-19-prompt-queue.md). The task
// wording applies even when that mid-turn drain happens to fire inside a
// goal loop's worker turn — the drain has no way to know (and does not
// need to) that its enclosing Prompt call is being driven by PursueGoal;
// only goal.go's OWN turn-boundary drain, which is actually building a
// goal directive, uses the goal wording.
// A prompt carrying attachments renders its own count marker
// ("[N attachment(s) attached below]"): this block is TEXT, so the bytes
// themselves ride as separate Blob parts of whatever message the drain site
// builds around it (see queuedBlobs and drainQueuedPromptsIntoHistory). The
// marker is what ties the numbered entry to the files that follow it, so the
// model can tell which operator message an attachment belongs to instead of
// finding an unexplained blob at the end.
//
// The marker names no MEDIA TYPE, deliberately. message/wire_normalize.go
// spells the same situation two ways -- "[N image attachment(s) attached
// below]" on a path that only ever carries images, and a generic
// "[N attachment(s) omitted: ...]" where the type varies -- and this drain
// is the second kind: a queued prompt carries whatever
// server/prompt_parts.go admits, images and PDFs alike. Saying "image"
// here would tell the model a queued PDF is a picture.
func operatorMessagesBlock(prompts []QueuedPrompt, ctx operatorContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OPERATOR MESSAGES (address these, then continue the %s):\n", ctx)
	for i, p := range prompts {
		fmt.Fprintf(&b, "%d. %s\n", i+1, p.Text)
		if len(p.Blobs) > 0 {
			fmt.Fprintf(&b, "   [%d attachment(s) attached below]\n", len(p.Blobs))
		}
	}
	b.WriteString("\n")
	return b.String()
}

// queuedBlobs collects every drained prompt's attachments, in FIFO prompt
// order, for a drain site that renders the batch's TEXT through
// operatorMessagesBlock above and must deliver the bytes alongside it. Nil
// when no drained prompt carried an attachment — the common case, and the
// one that keeps an injected operator message byte-identical to what it was
// before attachments existed.
func queuedBlobs(prompts []QueuedPrompt) []*message.Blob {
	var blobs []*message.Blob
	for _, p := range prompts {
		blobs = append(blobs, p.Blobs...)
	}
	return blobs
}
