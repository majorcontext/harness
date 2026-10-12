package e2e

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// leased is an Owner whose one grant ends when the test closes lost.
type leased struct{ lost chan struct{} }

func (l leased) Acquire(context.Context, string) (harness.Ownership, error) { return l, nil }
func (l leased) Epoch() uint64                                              { return 1 }
func (l leased) Lost() <-chan struct{}                                      { return l.lost }
func (l leased) Release(context.Context) error                              { return nil }

func lifetimeRuntime(t *testing.T, owner harness.Owner) *harness.Runtime {
	t.Helper()
	return lifetimeRuntimeOn(t, store.NewMemStore(), owner)
}

func lifetimeRuntimeOn(t *testing.T, st harness.Store, owner harness.Owner) *harness.Runtime {
	t.Helper()
	r, err := harness.New(harness.Options{Getenv: os.Getenv, Store: st, Owner: owner, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: "http://127.0.0.1:1", APIKeyEnv: "HARNESS_E2E_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func lifetimeSession(t *testing.T, r *harness.Runtime) *harness.Session {
	t.Helper()
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func awaitDone(t *testing.T, s *harness.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	select {
	case <-s.Done():
	case <-ctx.Done():
		t.Fatal("Done did not close")
	}
}

// endOfEvents drains Events from seq 0 and returns the error that ends it.
func endOfEvents(t *testing.T, s *harness.Session) error {
	t.Helper()
	var last error
	for _, err := range s.Events(t.Context(), 0) {
		last = err
	}
	return last
}

func TestContractRuntimeLifetime(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"session_lifetime_is_open_while_it_runs", rowSessionLifetimeIsOpenWhileItRuns},
		{"release_stops_the_session_with_cause_released", rowReleaseStopsTheSessionWithCauseReleased},
		{"end_stops_the_session_with_cause_ended", rowEndStopsTheSessionWithCauseEnded},
		{"a_lost_grant_stops_the_session_with_cause_ownership_lost", rowALostGrantStopsTheSessionWithCauseOwnershipLost},
		{"events_end_with_the_stop_cause", rowEventsEndWithTheStopCause},
		{"open_after_a_stop_returns_a_new_handle", rowOpenAfterAStopReturnsANewHandle},
		{"open_returns_the_same_handle_until_done_closes", rowOpenReturnsTheSameHandleUntilDoneCloses},
		{"an_append_conflict_stops_the_session_with_cause_ownership_lost", rowAnAppendConflictStopsTheSessionWithCauseOwnershipLost},
		{"a_stale_sync_epoch_stops_the_session_with_cause_ownership_lost", rowAStaleSyncEpochStopsTheSessionWithCauseOwnershipLost},
		{"a_store_failure_stops_the_session_with_not_owned_and_the_failure", rowAStoreFailureStopsTheSessionWithNotOwnedAndTheFailure},
		{"a_sync_rejection_stops_the_session_with_not_owned_and_the_rejection", rowASyncRejectionStopsTheSessionWithNotOwnedAndTheRejection},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func rowSessionLifetimeIsOpenWhileItRuns(t *testing.T) {
	s := lifetimeSession(t, lifetimeRuntime(t, nil))
	if s.ID() != "s1" {
		t.Errorf("ID = %q, want s1", s.ID())
	}
	select {
	case <-s.Done():
		t.Fatal("Done is closed while the session runs")
	default:
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err = %v, want nil before Done", err)
	}
}

func checkCause(t *testing.T, s *harness.Session, want error) {
	t.Helper()
	awaitDone(t, s)
	err := s.Err()
	for _, w := range []error{want, harness.ErrSessionNotOwned} {
		if !errors.Is(err, w) {
			t.Errorf("Err = %v, want a match for %v", err, w)
		}
	}
	for _, other := range []error{harness.ErrSessionReleased, harness.ErrSessionEnded, harness.ErrOwnershipLost} {
		if other != want && errors.Is(err, other) {
			t.Errorf("Err = %v, matches %v too", err, other)
		}
	}
}

func rowReleaseStopsTheSessionWithCauseReleased(t *testing.T) {
	s := lifetimeSession(t, lifetimeRuntime(t, nil))
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkCause(t, s, harness.ErrSessionReleased)
}

func rowEndStopsTheSessionWithCauseEnded(t *testing.T) {
	r := lifetimeRuntime(t, nil)
	s := lifetimeSession(t, r)
	if err := r.End(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	checkCause(t, s, harness.ErrSessionEnded)
}

func rowALostGrantStopsTheSessionWithCauseOwnershipLost(t *testing.T) {
	lost := make(chan struct{})
	s := lifetimeSession(t, lifetimeRuntime(t, leased{lost}))
	close(lost)
	checkCause(t, s, harness.ErrOwnershipLost)
}

func rowEventsEndWithTheStopCause(t *testing.T) {
	s := lifetimeSession(t, lifetimeRuntime(t, nil))
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	err := endOfEvents(t, s)
	if !errors.Is(err, harness.ErrSessionReleased) || !errors.Is(err, harness.ErrSessionNotOwned) {
		t.Errorf("Events ended with %v, want ErrSessionReleased, which matches ErrSessionNotOwned", err)
	}
	if got := s.Err(); got == nil || err.Error() != got.Error() {
		t.Errorf("Events ended with %v, Err is %v; want the same error", err, got)
	}
}

func rowOpenAfterAStopReturnsANewHandle(t *testing.T) {
	r := lifetimeRuntime(t, nil)
	first := lifetimeSession(t, r)
	if err := r.End(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	awaitDone(t, first)
	second, err := r.Open(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("Open returned the stopped handle")
	}
	select {
	case <-second.Done():
		t.Fatal("the new handle is done")
	default:
	}
}

func rowOpenReturnsTheSameHandleUntilDoneCloses(t *testing.T) {
	r := lifetimeRuntime(t, nil)
	s := lifetimeSession(t, r)
	same, err := r.Open(t.Context(), "s1")
	if err != nil || same != s {
		t.Fatalf("Open = %p, %v; want the handle %p", same, err, s)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = r.End(t.Context(), "s1")
	}()
	for {
		var doneBefore bool
		select {
		case <-s.Done():
			doneBefore = true
		default:
		}
		got, err := r.Open(t.Context(), "s1")
		if err != nil {
			break
		}
		if got != s {
			select {
			case <-s.Done():
			default:
				t.Fatal("Open returned a new handle while the old one was not done")
			}
			break
		}
		if doneBefore {
			t.Fatal("Open returned the old handle after Done closed")
		}
	}
	<-ended
}

// flaky is a Store whose Append fails with err once armed.
type flaky struct {
	harness.Store
	armed atomic.Bool
	err   error
}

func (f *flaky) Append(ctx context.Context, id string, seq uint64, recs ...[]byte) error {
	if f.armed.Load() {
		return f.err
	}
	return f.Store.Append(ctx, id, seq, recs...)
}

func submitAfterArming(t *testing.T, st *flaky) *harness.Session {
	t.Helper()
	s := lifetimeSession(t, lifetimeRuntimeOn(t, st, nil))
	st.armed.Store(true)
	_, _ = s.Prompt(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
	awaitDone(t, s)
	return s
}

func rowAnAppendConflictStopsTheSessionWithCauseOwnershipLost(t *testing.T) {
	st := &flaky{Store: store.NewMemStore(), err: harness.ErrConflict}
	s := submitAfterArming(t, st)
	for _, w := range []error{harness.ErrOwnershipLost, harness.ErrSessionNotOwned, harness.ErrConflict} {
		if !errors.Is(s.Err(), w) {
			t.Errorf("Err = %v, want a match for %v", s.Err(), w)
		}
	}
}

func rowAStoreFailureStopsTheSessionWithNotOwnedAndTheFailure(t *testing.T) {
	disk := errors.New("disk full")
	st := &flaky{Store: store.NewMemStore(), err: disk}
	s := submitAfterArming(t, st)
	for _, w := range []error{harness.ErrSessionNotOwned, disk} {
		if !errors.Is(s.Err(), w) {
			t.Errorf("Err = %v, want a match for %v", s.Err(), w)
		}
	}
	for _, other := range []error{harness.ErrSessionReleased, harness.ErrSessionEnded, harness.ErrOwnershipLost} {
		if errors.Is(s.Err(), other) {
			t.Errorf("Err = %v, matches %v too", s.Err(), other)
		}
	}
}

// rejecting is a Sync that rejects every batch with err.
type rejecting struct{ err error }

func (r rejecting) Deliver(context.Context, protocol.SyncBatch) (protocol.SyncAck, error) {
	return protocol.SyncAck{}, r.err
}

func syncedSession(t *testing.T, sync harness.Sync) *harness.Session {
	t.Helper()
	r, err := harness.New(harness.Options{Getenv: os.Getenv, Store: store.NewMemStore(), Sync: sync, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: "http://127.0.0.1:1", APIKeyEnv: "HARNESS_E2E_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s := lifetimeSession(t, r)
	awaitDone(t, s)
	return s
}

func rowAStaleSyncEpochStopsTheSessionWithCauseOwnershipLost(t *testing.T) {
	s := syncedSession(t, rejecting{harness.ErrStaleEpoch})
	for _, w := range []error{harness.ErrOwnershipLost, harness.ErrSessionNotOwned} {
		if !errors.Is(s.Err(), w) {
			t.Errorf("Err = %v, want a match for %v", s.Err(), w)
		}
	}
}

func rowASyncRejectionStopsTheSessionWithNotOwnedAndTheRejection(t *testing.T) {
	s := syncedSession(t, rejecting{harness.ErrSyncRejected})
	for _, w := range []error{harness.ErrSessionNotOwned, harness.ErrSyncRejected} {
		if !errors.Is(s.Err(), w) {
			t.Errorf("Err = %v, want a match for %v", s.Err(), w)
		}
	}
	for _, other := range []error{harness.ErrSessionReleased, harness.ErrSessionEnded, harness.ErrOwnershipLost} {
		if errors.Is(s.Err(), other) {
			t.Errorf("Err = %v, matches %v too", s.Err(), other)
		}
	}
}
