package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
)

// fakeSink records every batch it is handed and answers a scripted cursor.
type fakeSink struct {
	mu       sync.Mutex
	batches  []EventBatch
	gotBatch chan struct{}

	// failNext, when > 0, makes Deliver return an error and decrements.
	failNext int
	// appliedOverride is returned instead of batch.ToSeq when overrideSet.
	// A separate bool because 0 is a MEANINGFUL override — it is how a
	// receiver commands a full re-bootstrap — so it cannot double as
	// "unset".
	appliedOverride int64
	overrideSet     bool
}

func newFakeSink() *fakeSink {
	return &fakeSink{gotBatch: make(chan struct{}, 64)}
}

func (f *fakeSink) Deliver(_ context.Context, b EventBatch) (int64, error) {
	f.mu.Lock()
	if f.failNext > 0 {
		f.failNext--
		f.mu.Unlock()
		return 0, errors.New("sink unavailable")
	}
	f.batches = append(f.batches, b)
	applied := b.ToSeq
	if f.overrideSet {
		applied = f.appliedOverride
		f.overrideSet = false
	}
	f.mu.Unlock()
	select {
	case f.gotBatch <- struct{}{}:
	default:
	}
	return applied, nil
}

func (f *fakeSink) delivered() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Event
	for _, b := range f.batches {
		out = append(out, b.Records...)
	}
	return out
}

// waitForSeq blocks until the sink has been handed a record with seq >= want.
// It blocks on the sink's own notification channel rather than polling.
func (f *fakeSink) waitForSeq(t *testing.T, want int64) {
	t.Helper()
	for {
		f.mu.Lock()
		var max int64
		for _, b := range f.batches {
			if b.ToSeq > max {
				max = b.ToSeq
			}
		}
		f.mu.Unlock()
		if max >= want {
			return
		}
		<-f.gotBatch
	}
}

// waitForRecord blocks until a record with this seq has been delivered.
// The rewind test needs this rather than waitForSeq: after a rewind the
// pump sends the new record and THEN re-ships from the reset cursor in the
// same flush cycle, so a wait keyed on ToSeq is satisfied by the first of
// those two batches and races the second.
func (f *fakeSink) waitForRecord(t *testing.T, seq int64) {
	t.Helper()
	for {
		for _, e := range f.delivered() {
			if e.Seq == seq {
				return
			}
		}
		<-f.gotBatch
	}
}

func sinkServer(t *testing.T, f *fakeSink) *Server {
	t.Helper()
	return newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 4, func(o *Options) {
		o.EventSink = f
		o.EventSinkFlush = time.Millisecond
	})
}

func TestEventSinkForwardsEveryDurableRecord(t *testing.T) {
	f := newFakeSink()
	s := sinkServer(t, f)

	var want []int64
	for i := 0; i < 5; i++ {
		want = append(want, s.emitDurable(Event{
			Type: evtSessionStatus, SessionID: "ses_x", Status: "busy",
		}))
	}
	f.waitForSeq(t, want[len(want)-1])

	got := f.delivered()
	if len(got) < len(want) {
		t.Fatalf("delivered %d records, want at least %d", len(got), len(want))
	}
	// Seqs must arrive in order with no gap between consecutive records.
	for i := 1; i < len(got); i++ {
		if got[i].Seq != got[i-1].Seq+1 {
			t.Fatalf("record %d seq %d, want %d with no gap", i, got[i].Seq, got[i-1].Seq+1)
		}
	}
}

func TestNextEventBatchHonorsMaxBytes(t *testing.T) {
	first := Event{Type: evtSessionStatus, SessionID: "ses_batch", Seq: 1, Text: strings.Repeat("a", 128)}
	second := Event{Type: evtSessionStatus, SessionID: "ses_batch", Seq: 2, Text: strings.Repeat("b", 128)}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first record: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second record: %v", err)
	}

	s := &Server{
		opts: Options{
			EventSinkMaxRecords: 10,
			EventSinkMaxBytes:   len(firstJSON) + len(secondJSON) - 1,
		},
		journal: []Event{first, second},
		seq:     2,
	}
	batch, ok := s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch returned no records")
	}
	if len(batch.Records) != 1 || batch.FromSeq != 1 || batch.ToSeq != 1 {
		t.Fatalf("batch = %+v, want only seq 1 within the byte limit", batch)
	}

	s.opts.EventSinkMaxBytes = 1
	batch, ok = s.nextEventBatch()
	if !ok || len(batch.Records) != 1 || batch.Records[0].Seq != 1 {
		t.Fatalf("oversized first-record batch = %+v, ok=%t; want seq 1 alone", batch, ok)
	}
}

func TestNextEventBatchIsolatesMarshalFailure(t *testing.T) {
	poison := Event{
		Type: evtSessionStatus, SessionID: "ses_poison", Seq: 1,
		Output: message.Parts{nil},
	}
	valid := Event{Type: evtSessionStatus, SessionID: "ses_poison", Seq: 2, Status: "idle"}
	s := &Server{
		opts:    Options{EventSinkMaxRecords: 10, EventSinkMaxBytes: 1 << 20},
		journal: []Event{poison, valid},
		seq:     2,
	}

	batch, ok := s.nextEventBatch()
	if !ok || len(batch.Records) != 1 || batch.FromSeq != 1 || batch.ToSeq != 1 {
		t.Fatalf("poison batch = %+v, ok=%t; want only seq 1", batch, ok)
	}

	s.sinkCursor = 1
	batch, ok = s.nextEventBatch()
	if !ok || len(batch.Records) != 1 || batch.FromSeq != 2 || batch.ToSeq != 2 {
		t.Fatalf("post-poison batch = %+v, ok=%t; want valid seq 2", batch, ok)
	}
}

func TestEventSinkRetriesAfterFailureWithoutLosingARecord(t *testing.T) {
	f := newFakeSink()
	f.failNext = 2
	s := sinkServer(t, f)

	seq := s.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_y", Status: "busy"})
	f.waitForSeq(t, seq)

	var found bool
	for _, e := range f.delivered() {
		if e.Seq == seq {
			found = true
		}
	}
	if !found {
		t.Fatalf("record seq %d never arrived after the sink recovered", seq)
	}
}

func TestEventSinkReshipsWhenReceiverResetsCursor(t *testing.T) {
	f := newFakeSink()
	s := sinkServer(t, f)

	first := s.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_z", Status: "busy"})
	f.waitForSeq(t, first)

	// The receiver commands a re-bootstrap by answering 0: it holds nothing.
	f.mu.Lock()
	f.appliedOverride = 0
	f.overrideSet = true
	f.batches = nil
	f.mu.Unlock()

	second := s.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_z", Status: "idle"})
	_ = second // the emit is what wakes the pump; the assertion below is the re-ship

	// The re-ship is the assertion: with the cursor reset to 0, the record
	// already delivered before the rewind must arrive again.
	f.waitForRecord(t, first)
}

func TestEventSinkNeverHoldsServerMutexAcrossDeliver(t *testing.T) {
	release := make(chan struct{})
	blocking := &blockingSink{release: release, entered: make(chan struct{}, 1)}
	s := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 4, func(o *Options) {
		o.EventSink = blocking
		o.EventSinkFlush = time.Millisecond
	})

	s.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_b", Status: "busy"})
	<-blocking.entered // the pump is now inside Deliver

	// If the pump held s.mu across Deliver, this would block until release.
	done := make(chan int64, 1)
	go func() { done <- s.currentSeq() }()
	<-done
	close(release)
}

type blockingSink struct {
	release chan struct{}
	entered chan struct{}
}

func (b *blockingSink) Deliver(_ context.Context, batch EventBatch) (int64, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return batch.ToSeq, nil
}

type cancelAwareSink struct {
	mu        sync.Mutex
	calls     int
	firstCtx  context.Context
	entered   chan struct{}
	release   chan struct{}
	delivered chan EventBatch
}

func (s *cancelAwareSink) Deliver(ctx context.Context, batch EventBatch) (int64, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	if call == 1 {
		s.firstCtx = ctx
	}
	s.mu.Unlock()
	if call == 1 {
		close(s.entered)
		select {
		case <-ctx.Done():
		case <-s.release:
		}
		return 0, errors.New("first delivery interrupted")
	}
	s.delivered <- batch
	return batch.ToSeq, nil
}

func (s *cancelAwareSink) firstContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstCtx
}

func TestDrainCancelsBlockedDeliveryBeforeFinalFlush(t *testing.T) {
	sink := &cancelAwareSink{
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		delivered: make(chan EventBatch, 1),
	}
	s := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 4, func(o *Options) {
		o.EventSink = sink
		o.EventSinkFlush = time.Millisecond
	})
	seq := s.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_cancel", Status: "idle"})
	<-sink.entered

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drained := make(chan struct{})
	go func() {
		s.Drain(ctx)
		close(drained)
	}()

	<-s.sinkStop
	if deliveryCtx := sink.firstContext(); deliveryCtx == nil || deliveryCtx.Err() == nil {
		t.Error("stopEventSink closed sinkStop without canceling the blocked delivery context")
	}
	// Unblock the old implementation after recording the failure, so this
	// regression never waits on a guessed deadline.
	close(sink.release)
	<-drained

	select {
	case batch := <-sink.delivered:
		if batch.FromSeq != seq || batch.ToSeq != seq {
			t.Fatalf("final batch = %+v, want seq %d", batch, seq)
		}
	default:
		t.Fatal("Drain canceled the blocked delivery but did not make a final delivery attempt")
	}
}

func TestEventSinkDisabledHasNoWakeChannel(t *testing.T) {
	s := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 4)
	if s.sinkWake != nil {
		t.Fatal("sinkWake is non-nil without an event sink; durable records pay for an unconsumed wake")
	}
	select {
	case <-s.sinkDone:
	default:
		t.Fatal("sinkDone is open without an event sink")
	}
}

func TestEventSinkDoesNotRunWithoutASessionDir(t *testing.T) {
	f := newFakeSink()
	s := newServer(t, "", &scriptedProvider{name: "test"}, 4, func(o *Options) {
		o.EventSink = f
		o.EventSinkFlush = time.Millisecond
	})

	s.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_n", Status: "busy"})

	if s.sinkWake != nil {
		t.Fatal("sinkWake is non-nil without a durable session directory; durable records pay for an unconsumed wake")
	}
	// sinkDone is closed at construction when no pump starts, so this is a
	// state assertion rather than a race with a goroutine that may not exist.
	select {
	case <-s.sinkDone:
	default:
		t.Fatal("sinkDone is open: a pump started despite an empty SessionDir")
	}
	if got := f.delivered(); len(got) != 0 {
		t.Fatalf("delivered %d records with persistence disabled, want 0", len(got))
	}
}

// Drain closes s.closing FIRST and only then waits for in-flight prompts,
// which journal their trailing records during that wait. A pump that exited
// on s.closing retired before those records existed and lost every one of
// them, while Drain's own sinkDone wait returned instantly having guarded
// nothing.
func TestEventSinkShipsRecordsJournaledDuringDrain(t *testing.T) {
	f := newFakeSink()
	s := sinkServer(t, f)

	s.mu.Lock()
	s.closeOnce.Do(func() { close(s.closing) })
	s.mu.Unlock()

	// Stands in for the trailing records a cancelled prompt journals after
	// s.closing closes but before Drain retires the pump. Its delivery below
	// is the deterministic proof that the pump stayed alive for this window.
	late := s.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_drain", Status: "idle"})

	s.Drain(t.Context())

	for _, e := range f.delivered() {
		if e.Seq == late {
			return
		}
	}
	t.Fatalf("record seq %d was journaled during the drain window and never shipped", late)
}

// loadJournal appends a restored journal straight to s.journal, never through
// emitDurableLocked, so nothing wakes the pump for records this process did
// not itself emit. Without a flush before the wait loop, a process that
// restarts and then goes idle replicates nothing at all.
func TestEventSinkShipsARestoredJournalWithNoNewRecord(t *testing.T) {
	dir := t.TempDir()

	// First server writes a journal, then goes away.
	first := newServer(t, dir, &scriptedProvider{name: "test"}, 4)
	want := first.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_restart", Status: "busy"})
	if err := first.Close(); err != nil {
		t.Fatalf("close first server: %v", err)
	}

	// Second server over the same dir, with a sink and NO new record.
	f := newFakeSink()
	newServer(t, dir, &scriptedProvider{name: "test"}, 4, func(o *Options) {
		o.EventSink = f
		o.EventSinkFlush = time.Millisecond
	})

	f.waitForRecord(t, want)
}

// batchSnapshot copies the batches delivered so far, including the empty
// filtered checkpoints that delivered() cannot show.
func (f *fakeSink) batchSnapshot() []EventBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]EventBatch(nil), f.batches...)
}

// seqsOf lists a batch's record sequence numbers, oldest first.
func seqsOf(b EventBatch) []int64 {
	var out []int64
	for _, r := range b.Records {
		out = append(out, r.Seq)
	}
	return out
}

// filteredSinkServer builds a pump whose selector is the exact type list a
// user writes in the event-sink configuration.
func filteredSinkServer(t *testing.T, dir string, f *fakeSink, types ...string) *Server {
	t.Helper()
	return newServer(t, dir, &scriptedProvider{name: "test"}, 4, func(o *Options) {
		o.EventSink = f
		o.EventSinkFlush = time.Millisecond
		o.EventSinkIncludeTypes = types
	})
}

// The type strings below are the caller-facing selector values, quoted the
// way a configuration file writes them, so these tests never restate a
// server-local constant back to itself.
const (
	sinkTypeMessage     = "message"
	sinkTypePromptQueue = "prompt.queued"
	sinkTypeTurnEnd     = "turn.end"
)

// Journal: 1 message, 2 prompt.queued, 3 message, 4 turn.end. The selector
// takes prompt.queued and turn.end. A dense pump ships all four records; the
// filtered pump must ship seq 2 and 4 only, and must still report the whole
// range it scanned (1..4) so the receiver's cursor clears the omitted
// records.
func TestEventSinkFilterShipsSelectedRecordsWithTheScannedRange(t *testing.T) {
	s := &Server{
		opts:      Options{EventSinkMaxRecords: 10, EventSinkMaxBytes: 1 << 20},
		sinkTypes: eventSinkTypeSet([]string{sinkTypePromptQueue, sinkTypeTurnEnd}),
		journal: []Event{
			{Type: sinkTypeMessage, SessionID: "ses_f", Seq: 1},
			{Type: sinkTypePromptQueue, SessionID: "ses_f", Seq: 2},
			{Type: sinkTypeMessage, SessionID: "ses_f", Seq: 3},
			{Type: sinkTypeTurnEnd, SessionID: "ses_f", Seq: 4},
		},
		seq: 4,
	}

	batch, ok := s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch returned no batch for a journal with two selected records")
	}
	if batch.FromSeq != 1 || batch.ToSeq != 4 || !batch.Filtered {
		t.Errorf("batch range = from %d to %d filtered %t, want from 1 to 4 filtered true", batch.FromSeq, batch.ToSeq, batch.Filtered)
	}
	if got := seqsOf(batch); len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Errorf("record seqs = %v, want [2 4]", got)
	}
}

// A selector that matches nothing must still ship a checkpoint. Without one
// the pump reports "no work" for a journal it has fully scanned, so the
// receiver's cursor stalls at 0 for the whole life of an unselected run.
func TestEventSinkFilterShipsAnEmptyCheckpointForTheScannedRange(t *testing.T) {
	s := &Server{
		opts:      Options{EventSinkMaxRecords: 10, EventSinkMaxBytes: 1 << 20},
		sinkTypes: eventSinkTypeSet([]string{sinkTypeTurnEnd}),
		journal: []Event{
			{Type: sinkTypeMessage, SessionID: "ses_e", Seq: 1},
			{Type: sinkTypeMessage, SessionID: "ses_e", Seq: 2},
			{Type: sinkTypeMessage, SessionID: "ses_e", Seq: 3},
		},
		seq: 3,
	}

	batch, ok := s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch reported no work for three scanned records; the cursor can never advance")
	}
	if batch.FromSeq != 1 || batch.ToSeq != 3 || !batch.Filtered {
		t.Errorf("checkpoint = from %d to %d filtered %t, want from 1 to 3 filtered true", batch.FromSeq, batch.ToSeq, batch.Filtered)
	}
	if len(batch.Records) != 0 {
		t.Errorf("checkpoint carries %d records, want 0", len(batch.Records))
	}
}

// EventSinkMaxRecords bounds the JOURNAL window, not the selected count. A
// pump that counted only selected records would scan past seq 2 looking for
// a second match and ship turn.end at seq 3 in the first batch.
func TestEventSinkFilterScanWindowCountsOmittedRecords(t *testing.T) {
	s := &Server{
		opts:      Options{EventSinkMaxRecords: 2, EventSinkMaxBytes: 1 << 20},
		sinkTypes: eventSinkTypeSet([]string{sinkTypeTurnEnd}),
		journal: []Event{
			{Type: sinkTypeMessage, SessionID: "ses_w", Seq: 1},
			{Type: sinkTypeMessage, SessionID: "ses_w", Seq: 2},
			{Type: sinkTypeTurnEnd, SessionID: "ses_w", Seq: 3},
		},
		seq: 3,
	}

	batch, ok := s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch reported no work for the first two scanned records")
	}
	if batch.FromSeq != 1 || batch.ToSeq != 2 || len(batch.Records) != 0 {
		t.Fatalf("first batch = from %d to %d seqs %v, want from 1 to 2 with no records", batch.FromSeq, batch.ToSeq, seqsOf(batch))
	}

	s.sinkCursor = batch.ToSeq
	batch, ok = s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch reported no work with turn.end still unsent")
	}
	if batch.FromSeq != 3 || batch.ToSeq != 3 || len(seqsOf(batch)) != 1 || seqsOf(batch)[0] != 3 {
		t.Fatalf("second batch = from %d to %d seqs %v, want from 3 to 3 seqs [3]", batch.FromSeq, batch.ToSeq, seqsOf(batch))
	}
}

// An omitted record is never sent, so it must not charge the byte budget.
// The limit here fits both selected records exactly; if the two large
// omitted message records were charged, the batch would stop at seq 2 and
// leave turn.end at seq 4 for a later pass.
func TestEventSinkFilterOmittedRecordsDoNotConsumeMaxBytes(t *testing.T) {
	bulk := strings.Repeat("m", 4096)
	journal := []Event{
		{Type: sinkTypeMessage, SessionID: "ses_c", Seq: 1, Text: bulk},
		{Type: sinkTypeTurnEnd, SessionID: "ses_c", Seq: 2, Outcome: "completed"},
		{Type: sinkTypeMessage, SessionID: "ses_c", Seq: 3, Text: bulk},
		{Type: sinkTypeTurnEnd, SessionID: "ses_c", Seq: 4, Outcome: "completed"},
	}
	var selected int
	for _, i := range []int{1, 3} {
		encoded, err := json.Marshal(journal[i])
		if err != nil {
			t.Fatalf("marshal selected record %d: %v", journal[i].Seq, err)
		}
		selected += len(encoded)
	}

	s := &Server{
		opts:      Options{EventSinkMaxRecords: 10, EventSinkMaxBytes: selected},
		sinkTypes: eventSinkTypeSet([]string{sinkTypeTurnEnd}),
		journal:   journal,
		seq:       4,
	}

	batch, ok := s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch returned no batch")
	}
	if batch.FromSeq != 1 || batch.ToSeq != 4 {
		t.Errorf("batch range = from %d to %d, want from 1 to 4", batch.FromSeq, batch.ToSeq)
	}
	if got := seqsOf(batch); len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Errorf("record seqs = %v, want [2 4]; an omitted record charged the byte budget", got)
	}
}

// The first-record byte exemption must survive an omitted prefix. With a
// one-byte budget the selected record at seq 2 still ships alone, and the
// next oversized selection ships in its own batch rather than disappearing.
func TestEventSinkFilterShipsAnOversizedSelectedRecordAlone(t *testing.T) {
	bulk := strings.Repeat("t", 4096)
	s := &Server{
		opts:      Options{EventSinkMaxRecords: 10, EventSinkMaxBytes: 1},
		sinkTypes: eventSinkTypeSet([]string{sinkTypeTurnEnd}),
		journal: []Event{
			{Type: sinkTypeMessage, SessionID: "ses_o", Seq: 1, Text: bulk},
			{Type: sinkTypeTurnEnd, SessionID: "ses_o", Seq: 2, Error: bulk},
			{Type: sinkTypeTurnEnd, SessionID: "ses_o", Seq: 3, Error: bulk},
		},
		seq: 3,
	}

	batch, ok := s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch dropped an oversized selected record")
	}
	if batch.FromSeq != 1 || batch.ToSeq != 2 || len(seqsOf(batch)) != 1 || seqsOf(batch)[0] != 2 {
		t.Fatalf("first batch = from %d to %d seqs %v, want from 1 to 2 seqs [2]", batch.FromSeq, batch.ToSeq, seqsOf(batch))
	}

	s.sinkCursor = batch.ToSeq
	batch, ok = s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch dropped the second oversized selected record")
	}
	if batch.FromSeq != 3 || batch.ToSeq != 3 || len(seqsOf(batch)) != 1 || seqsOf(batch)[0] != 3 {
		t.Fatalf("second batch = from %d to %d seqs %v, want from 3 to 3 seqs [3]", batch.FromSeq, batch.ToSeq, seqsOf(batch))
	}
}

// An empty selector leaves every batch dense and unmarked, so a receiver
// cannot tell an unfiltered batch from a filtered one that happened to
// select everything.
func TestEventSinkFilterDisabledKeepsDenseUnmarkedBatches(t *testing.T) {
	s := &Server{
		opts: Options{EventSinkMaxRecords: 10, EventSinkMaxBytes: 1 << 20},
		journal: []Event{
			{Type: sinkTypeMessage, SessionID: "ses_d", Seq: 1},
			{Type: sinkTypeTurnEnd, SessionID: "ses_d", Seq: 2},
		},
		seq: 2,
	}

	batch, ok := s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch returned no batch")
	}
	if batch.Filtered {
		t.Error("batch is marked filtered without a selector")
	}
	if got := seqsOf(batch); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("record seqs = %v, want [1 2]", got)
	}

	s.sinkCursor = 2
	if batch, ok := s.nextEventBatch(); ok {
		t.Errorf("nextEventBatch = %+v, ok=true past the journal end; an unfiltered pump must report no work", batch)
	}
}

// A receiver that answers 0 commands a re-bootstrap. The rescan must stay
// filtered: it re-ships the selected record and must not smuggle the
// omitted message record in behind it.
func TestEventSinkFilterRewindToZeroRescansFiltered(t *testing.T) {
	f := newFakeSink()
	s := filteredSinkServer(t, t.TempDir(), f, sinkTypeTurnEnd)

	first := s.emitDurable(Event{Type: sinkTypeTurnEnd, SessionID: "ses_r", Outcome: "completed"})
	f.waitForSeq(t, first)

	f.mu.Lock()
	f.appliedOverride = 0
	f.overrideSet = true
	f.batches = nil
	f.mu.Unlock()

	// The emit wakes the pump; the rescan below is the assertion.
	s.emitDurable(Event{Type: sinkTypeMessage, SessionID: "ses_r"})
	f.waitForRecord(t, first)

	for _, b := range f.batchSnapshot() {
		if !b.Filtered {
			t.Fatalf("batch %+v is not marked filtered after the rewind", b)
		}
		for _, r := range b.Records {
			if r.Type != sinkTypeTurnEnd {
				t.Fatalf("rescan shipped a %q record at seq %d; the selector was dropped on rewind", r.Type, r.Seq)
			}
		}
	}
}

// loadJournal restores records without waking the pump, so the first flush
// is the only chance to report them. When the selector matches none of them,
// that flush must still ship an empty checkpoint: otherwise a restarted
// process that goes idle never tells the receiver how far it has scanned.
func TestEventSinkFilterRestoredJournalShipsACheckpointWithoutNewWork(t *testing.T) {
	dir := t.TempDir()

	first := newServer(t, dir, &scriptedProvider{name: "test"}, 4)
	restored := first.emitDurable(Event{Type: sinkTypeMessage, SessionID: "ses_rs"})
	if err := first.Close(); err != nil {
		t.Fatalf("close first server: %v", err)
	}

	f := newFakeSink()
	filteredSinkServer(t, dir, f, sinkTypeTurnEnd)
	f.waitForSeq(t, restored)

	batches := f.batchSnapshot()
	if len(batches) == 0 {
		t.Fatal("no batch after restoring a journal with no selected record")
	}
	got := batches[0]
	if got.FromSeq != 1 || got.ToSeq != restored || !got.Filtered || len(got.Records) != 0 {
		t.Fatalf("first batch = from %d to %d filtered %t seqs %v, want from 1 to %d filtered true with no records",
			got.FromSeq, got.ToSeq, got.Filtered, seqsOf(got), restored)
	}
}

// Drain closes s.closing first and then waits for in-flight prompts, which
// journal trailing records during that wait. When the selector matches none
// of them, the final flush must still ship the scanned tail so the receiver
// learns the shutdown point instead of stalling one batch short of it.
func TestEventSinkFilterFinalDrainShipsTheScannedTail(t *testing.T) {
	f := newFakeSink()
	s := filteredSinkServer(t, t.TempDir(), f, sinkTypeTurnEnd)

	s.mu.Lock()
	s.closeOnce.Do(func() { close(s.closing) })
	s.mu.Unlock()

	late := s.emitDurable(Event{Type: sinkTypeMessage, SessionID: "ses_t"})
	s.Drain(t.Context())

	var reached bool
	for _, b := range f.batchSnapshot() {
		if b.ToSeq >= late {
			reached = true
		}
		for _, r := range b.Records {
			if r.Type != sinkTypeTurnEnd {
				t.Fatalf("drain shipped a %q record at seq %d; the selector was dropped on the final flush", r.Type, r.Seq)
			}
		}
	}
	if !reached {
		t.Fatalf("no batch scanned through seq %d; the unselected drain tail never checkpointed", late)
	}
}

// The byte limit stops before the selected record that would exceed it, and
// ToSeq must then name the last candidate SCANNED, not the last record sent.
// Journal: 1 turn.end (fits), 2 message (omitted), 3 turn.end (does not
// fit). A pump that reported ToSeq=1 would hand the already-scanned message
// record back to the next pass.
func TestEventSinkFilterByteLimitStopsAtTheLastScannedCandidate(t *testing.T) {
	small := Event{Type: sinkTypeTurnEnd, SessionID: "ses_s", Seq: 1, Outcome: "completed"}
	encoded, err := json.Marshal(small)
	if err != nil {
		t.Fatalf("marshal the selected record: %v", err)
	}
	s := &Server{
		opts:      Options{EventSinkMaxRecords: 10, EventSinkMaxBytes: len(encoded)},
		sinkTypes: eventSinkTypeSet([]string{sinkTypeTurnEnd}),
		journal: []Event{
			small,
			{Type: sinkTypeMessage, SessionID: "ses_s", Seq: 2},
			{Type: sinkTypeTurnEnd, SessionID: "ses_s", Seq: 3, Error: strings.Repeat("e", 4096)},
		},
		seq: 3,
	}

	batch, ok := s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch returned no batch")
	}
	if batch.FromSeq != 1 || batch.ToSeq != 2 || len(seqsOf(batch)) != 1 || seqsOf(batch)[0] != 1 {
		t.Fatalf("batch = from %d to %d seqs %v, want from 1 to 2 seqs [1]", batch.FromSeq, batch.ToSeq, seqsOf(batch))
	}

	s.sinkCursor = batch.ToSeq
	batch, ok = s.nextEventBatch()
	if !ok {
		t.Fatal("nextEventBatch dropped the oversized record the byte limit deferred")
	}
	if batch.FromSeq != 3 || batch.ToSeq != 3 || len(seqsOf(batch)) != 1 || seqsOf(batch)[0] != 3 {
		t.Fatalf("deferred batch = from %d to %d seqs %v, want from 3 to 3 seqs [3]", batch.FromSeq, batch.ToSeq, seqsOf(batch))
	}
}
