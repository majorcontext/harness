package server

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

// EventSink is the outbound transport for the durable journal. Deliver
// returns the seq the receiver has applied through, which becomes the
// pump's cursor: the RECEIVER owns that cursor, so harness keeps no durable
// outbound state and a re-bootstrap needs no separate channel — the
// receiver commands one by answering 0.
type EventSink interface {
	Deliver(ctx context.Context, batch EventBatch) (appliedThrough int64, err error)
}

// EventBatch is one contiguous run of durable records, oldest first.
type EventBatch struct {
	FromSeq int64
	ToSeq   int64
	Records []Event
}

const (
	defaultEventSinkFlush      = 250 * time.Millisecond
	defaultEventSinkMaxRecords = 256
	defaultEventSinkMaxBytes   = 4 << 20
	eventSinkRetryDelay        = 2 * time.Second
)

// stopEventSink retires the pump after a final flush. Idempotent, and safe
// when no pump was ever started.
func (s *Server) stopEventSink() {
	s.sinkStopOnce.Do(func() { close(s.sinkStop) })
}

// notifySinkLocked wakes the pump. It sends a SIGNAL, never a record: a
// dropped or coalesced wake cannot lose anything, because the pump re-reads
// the journal from its own cursor and finds whatever it missed. Sending the
// record instead would inherit fanoutLocked's drop-on-full policy, which is
// right for an SSE client that can reconnect and wrong for a replica that
// would keep a permanent hole. Caller holds s.mu.
func (s *Server) notifySinkLocked() {
	if s.sinkWake == nil {
		return
	}
	select {
	case s.sinkWake <- struct{}{}:
	default:
	}
}

// runEventSink is the pump goroutine. It exits after one final flush when
// stopEventSink is called, so the tail ships before Close takes the journal
// file away.
//
// It watches sinkStop rather than s.closing deliberately. Drain closes
// s.closing FIRST and only then waits for in-flight prompts, which journal
// their trailing records — a final assistant message, a session.aborted per
// cancelled prompt, the session.status(idle) transitions — during that wait.
// Exiting on s.closing would retire the pump before those records exist and
// lose every one of them.
func (s *Server) runEventSink() {
	defer close(s.sinkDone)
	flush := s.opts.EventSinkFlush
	if flush <= 0 {
		flush = defaultEventSinkFlush
	}
	// A restored journal is already in s.journal — loadJournal appends it
	// directly, never through emitDurableLocked — so nothing has woken this
	// pump for records this process did not itself emit. Without this first
	// flush, a process that restarts and then goes idle replicates nothing
	// until some unrelated record happens to arrive.
	s.flushEventSink()
	for {
		select {
		case <-s.sinkStop:
			s.flushEventSink()
			return
		case <-s.sinkWake:
		}
		// Coalesce a burst into one request.
		t := time.NewTimer(flush)
		select {
		case <-s.sinkStop:
			t.Stop()
			s.flushEventSink()
			return
		case <-t.C:
		}
		s.flushEventSink()
	}
}

// flushEventSink delivers everything above the cursor, in batches, until it
// runs out of records or a delivery fails. It never holds s.mu across
// Deliver.
func (s *Server) flushEventSink() {
	for {
		batch, ok := s.nextEventBatch()
		if !ok {
			return
		}
		applied, err := s.opts.EventSink.Deliver(context.Background(), batch)
		if err != nil {
			s.logWarn("event sink delivery failed", "from_seq", batch.FromSeq, "to_seq", batch.ToSeq, "error", err.Error())
			t := time.NewTimer(eventSinkRetryDelay)
			select {
			case <-s.sinkStop:
				t.Stop()
				return
			case <-t.C:
			}
			continue
		}
		if !s.advanceSinkCursor(applied) {
			// The receiver did not move past this batch's start, so sending
			// it again immediately would spin. Wait for the next wake.
			return
		}
	}
}

// nextEventBatch slices the journal above the cursor. ok is false when
// there is nothing to send.
func (s *Server) nextEventBatch() (EventBatch, bool) {
	maxRecords := s.opts.EventSinkMaxRecords
	if maxRecords <= 0 {
		maxRecords = defaultEventSinkMaxRecords
	}
	maxBytes := s.opts.EventSinkMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultEventSinkMaxBytes
	}

	s.mu.Lock()
	cursor := s.sinkCursor
	// The journal is append-only and seq is monotonic within it, so the
	// first record above the cursor is a binary search rather than a scan.
	i := sort.Search(len(s.journal), func(i int) bool { return s.journal[i].Seq > cursor })
	if i >= len(s.journal) {
		s.mu.Unlock()
		return EventBatch{}, false
	}
	end := len(s.journal)
	if end-i > maxRecords {
		end = i + maxRecords
	}
	// Event values are immutable once appended. Copy the candidate structs
	// while holding the journal lock, then release it before JSON sizing.
	candidates := append([]Event(nil), s.journal[i:end]...)
	s.mu.Unlock()

	batch := EventBatch{FromSeq: candidates[0].Seq}
	var bytes int
	for _, rec := range candidates {
		encoded, err := json.Marshal(rec)
		if err != nil {
			// Isolate a poison record. Records before it can advance; when it is
			// first, it still ships alone and the transport reports the failure.
			s.logWarn("event sink: record does not marshal", "seq", rec.Seq, "type", rec.Type, "error", err.Error())
			if len(batch.Records) > 0 {
				break
			}
		} else {
			// Check the candidate's size before adding it. The first record is
			// exempt so one oversized record ships alone rather than disappearing.
			if len(batch.Records) > 0 && bytes+len(encoded) > maxBytes {
				break
			}
			bytes += len(encoded)
		}
		batch.Records = append(batch.Records, rec)
		batch.ToSeq = rec.Seq
	}
	return batch, len(batch.Records) > 0
}

// advanceSinkCursor moves the cursor to what the receiver reported. It
// reports whether the cursor actually changed, which is what tells
// flushEventSink whether looping immediately would make progress.
//
// This is deliberately NOT "did applied reach batch.FromSeq": nextEventBatch
// built batch from the cursor as it stood BEFORE this delivery, so a
// mid-flight rewind (the receiver answering something below its own
// previous cursor, commanding a re-bootstrap) moves the cursor backward
// without ever reaching batch.FromSeq — yet the very next nextEventBatch
// call will see a different, larger window (the rewound gap plus this
// batch), so there is no spin risk and the pump must keep going. The only
// case that DOES spin is the cursor staying exactly where it was: the next
// nextEventBatch call would then hand back this identical batch forever.
func (s *Server) advanceSinkCursor(applied int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if applied < 0 {
		applied = 0
	}
	// A receiver cannot have applied a record this server has not assigned.
	if applied > s.seq {
		applied = s.seq
	}
	prev := s.sinkCursor
	s.sinkCursor = applied
	return applied != prev
}
