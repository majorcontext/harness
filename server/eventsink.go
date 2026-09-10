package server

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// ErrEventSinkPermanent marks a delivery failure that retrying cannot fix.
// A transport wraps it when the receiver rejects the BATCH — a malformed
// body, a refused credential, a route that holds no receiver — rather than
// asking for the same batch later. The pump stops on it, because resending
// identical bytes every eventSinkRetryDelay only earns the same rejection
// for the life of the process.
var ErrEventSinkPermanent = errors.New("permanent receiver rejection")

// EventSink is the outbound transport for the durable journal. Deliver
// returns the seq the receiver has applied through, which becomes the
// pump's cursor: the RECEIVER owns that cursor, so harness keeps no durable
// outbound state and a re-bootstrap needs no separate channel — the
// receiver commands one by answering 0.
type EventSink interface {
	Deliver(ctx context.Context, batch EventBatch) (appliedThrough int64, err error)
}

// EventBatch is one scanned run of durable records, oldest first. FromSeq
// and ToSeq bound the range the pump SCANNED, not the range it carries:
// when Filtered is true a selector dropped some of that range, so Records
// is sparse and may be empty. An empty filtered batch is a checkpoint — it
// is how the receiver's cursor clears a long unselected run.
type EventBatch struct {
	FromSeq  int64
	ToSeq    int64
	Filtered bool
	Records  []Event
}

// eventSinkTypeSet builds the exact-match selector for Options.
// EventSinkIncludeTypes. It returns nil for an empty list, and a nil set is
// what tells the pump to stay unfiltered.
func eventSinkTypeSet(types []string) map[string]struct{} {
	if len(types) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(types))
	for _, t := range types {
		set[t] = struct{}{}
	}
	return set
}

const (
	defaultEventSinkFlush      = 250 * time.Millisecond
	defaultEventSinkMaxRecords = 256
	defaultEventSinkMaxBytes   = 4 << 20
	eventSinkRetryDelay        = 2 * time.Second
)

// eventSinkStoppedMsg is the one warning a permanent rejection logs. The
// pump exits after it, so an operator reads it once, not once per retry.
const eventSinkStoppedMsg = "event sink stopped: receiver rejected the batch"

// stopEventSink cancels an active delivery, then asks the pump to make one
// final catch-up pass under finalCtx. Idempotent and safe without a pump.
func (s *Server) stopEventSink(finalCtx context.Context) {
	s.sinkStopOnce.Do(func() {
		s.sinkFinalCtx = finalCtx
		if s.sinkCancel != nil {
			s.sinkCancel()
		}
		close(s.sinkStop)
	})
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
	if !s.flushEventSink(s.sinkCtx) {
		return
	}
	for {
		select {
		case <-s.sinkStop:
			s.flushEventSink(s.sinkFinalCtx)
			return
		case <-s.sinkWake:
		}
		// Coalesce a burst into one request.
		t := time.NewTimer(flush)
		select {
		case <-s.sinkStop:
			t.Stop()
			s.flushEventSink(s.sinkFinalCtx)
			return
		case <-t.C:
		}
		if !s.flushEventSink(s.sinkCtx) {
			return
		}
	}
}

// flushEventSink delivers everything above the cursor in batches. A failed
// delivery retries the same batch after eventSinkRetryDelay. It returns when
// no records remain, ctx is canceled, or a failed delivery observes sinkStop.
// A successful final pass can drain the backlog after sinkStop closes. It never
// holds s.mu across Deliver.
//
// It reports whether the pump may keep running. Only ErrEventSinkPermanent
// answers false: that batch cannot succeed on a retry, and neither can any
// later batch built the same way, so the caller retires the pump. Harness
// itself is unaffected — the journal, the sessions, and every other client
// surface keep working without a replica.
func (s *Server) flushEventSink(ctx context.Context) bool {
	for {
		select {
		case <-ctx.Done():
			return true
		default:
		}
		batch, ok := s.nextEventBatch()
		if !ok {
			return true
		}
		applied, err := s.opts.EventSink.Deliver(ctx, batch)
		if err != nil {
			if errors.Is(err, ErrEventSinkPermanent) {
				// The transport already bounded and sanitized this text.
				s.logWarn(eventSinkStoppedMsg, "from_seq", batch.FromSeq, "to_seq", batch.ToSeq, "error", err.Error())
				return false
			}
			s.logWarn("event sink delivery failed", "from_seq", batch.FromSeq, "to_seq", batch.ToSeq, "error", err.Error())
			t := time.NewTimer(eventSinkRetryDelay)
			select {
			case <-ctx.Done():
				t.Stop()
				return true
			case <-s.sinkStop:
				t.Stop()
				return true
			case <-t.C:
			}
			continue
		}
		if !s.advanceSinkCursor(applied) {
			// The receiver did not move past this batch's start, so sending
			// it again immediately would spin. Wait for the next wake.
			return true
		}
	}
}

// nextEventBatch scans a bounded contiguous window of the journal above the
// cursor. Without a selector the batch is that window verbatim. With one it
// carries only the matching records, while FromSeq and ToSeq still report
// the whole window, so a delivery clears the omitted records too. ok is
// false only when the window is empty.
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

	batch := EventBatch{FromSeq: candidates[0].Seq, Filtered: s.sinkTypes != nil}
	var bytes int
	for _, rec := range candidates {
		if batch.Filtered {
			if _, want := s.sinkTypes[rec.Type]; !want {
				// An omitted record spends a record-window slot but no
				// bytes: it is never encoded and never sent, so charging
				// the byte budget for it would stall the cursor behind a
				// long unselected run.
				batch.ToSeq = rec.Seq
				continue
			}
		}
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
		if err != nil {
			break
		}
	}
	if batch.Filtered {
		// A filtered batch ships even with no records. It is a checkpoint:
		// ToSeq is how far the pump scanned, which is what the receiver
		// answers with and what advances the cursor.
		return batch, batch.ToSeq >= batch.FromSeq
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
