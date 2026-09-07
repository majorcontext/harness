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
// the server begins draining, so the tail ships before Close takes the
// journal file away.
func (s *Server) runEventSink() {
	defer close(s.sinkDone)
	flush := s.opts.EventSinkFlush
	if flush <= 0 {
		flush = defaultEventSinkFlush
	}
	for {
		select {
		case <-s.closing:
			s.flushEventSink()
			return
		case <-s.sinkWake:
		}
		// Coalesce a burst into one request.
		t := time.NewTimer(flush)
		select {
		case <-s.closing:
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
			case <-s.closing:
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
	defer s.mu.Unlock()

	cursor := s.sinkCursor
	// The journal is append-only and seq is monotonic within it, so the
	// first record above the cursor is a binary search rather than a scan.
	i := sort.Search(len(s.journal), func(i int) bool { return s.journal[i].Seq > cursor })
	if i >= len(s.journal) {
		return EventBatch{}, false
	}

	batch := EventBatch{FromSeq: s.journal[i].Seq}
	var bytes int
	for ; i < len(s.journal); i++ {
		rec := s.journal[i]
		if len(batch.Records) >= maxRecords {
			break
		}
		// The size check runs only after the first record is in, so one
		// oversized record is delivered alone rather than dropped.
		if len(batch.Records) > 0 && bytes >= maxBytes {
			break
		}
		if b, err := json.Marshal(rec); err == nil {
			bytes += len(b)
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
