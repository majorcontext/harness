package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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
		select {
		case <-f.gotBatch:
		case <-time.After(10 * time.Second):
			t.Fatalf("sink never received seq %d", want)
		}
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
		select {
		case <-f.gotBatch:
		case <-time.After(10 * time.Second):
			t.Fatalf("record seq %d was never delivered", seq)
		}
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
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("currentSeq blocked while the sink was mid-Deliver: the pump holds s.mu across it")
	}
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

	// The pump must still be alive. Drain has not yet waited for in-flight
	// prompts, so their trailing records are not journaled yet; a pump that
	// retired on s.closing would never see them. This is a negative
	// assertion — that something does NOT happen — so it needs a bounded
	// window rather than a channel to block on. Under the old design the
	// pump exits promptly here and this fires.
	select {
	case <-s.sinkDone:
		t.Fatal("pump retired when s.closing closed; every record journaled during the drain window would be lost")
	case <-time.After(500 * time.Millisecond):
	}

	// Stands in for the trailing records a cancelled prompt journals while
	// Drain is still waiting on it.
	late := s.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_drain", Status: "idle"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.Drain(ctx)

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
