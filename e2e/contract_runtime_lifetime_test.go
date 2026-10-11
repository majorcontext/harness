package e2e

import (
	"context"
	"errors"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// leased is an Owner whose one grant ends when the test closes lost.
type leased struct{ lost chan struct{} }

func (l leased) Acquire(context.Context, string) (harness.Ownership, error) { return l, nil }
func (l leased) Epoch() uint64                                              { return 1 }
func (l leased) Lost() <-chan struct{}                                      { return l.lost }
func (l leased) Release(context.Context) error                              { return nil }

func lifetimeRuntime(t *testing.T, owner harness.Owner) *harness.Runtime {
	t.Helper()
	return lifetimeRuntimeOn(t, harness.NewMemStore(), owner)
}

func lifetimeRuntimeOn(t *testing.T, st harness.Store, owner harness.Owner) *harness.Runtime {
	t.Helper()
	r, err := harness.New(harness.Options{Store: st, Owner: owner, Config: config.Config{ContextWindowTokens: 100000,
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
