package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// newWired builds a Runtime as cmd/harness does: the Owner and the Sync come
// from the config keys owner_epoch and sync.
func newWired(opts harness.Options) (*harness.Runtime, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Owner == nil {
		opts.Owner = harness.NewLocalOwner(uint64(max(opts.Config.OwnerEpoch, 1)))
	}
	if opts.Sync == nil && opts.Config.Sync != nil {
		opts.Sync = harness.NewHTTPSync(*opts.Config.Sync)
	}
	return harness.New(opts)
}

// heldOwner fails Acquire of the sessions in held with ErrSessionHeld, and
// grants any other session. Its grants fail Release with releaseErr.
type heldOwner struct {
	held       map[string]bool
	releaseErr error
}

func (o heldOwner) Acquire(_ context.Context, id string) (harness.Ownership, error) {
	if o.held[id] {
		return nil, fmt.Errorf("lease of %s: %w", id, harness.ErrSessionHeld)
	}
	return grant{o.releaseErr}, nil
}

type grant struct{ err error }

func (grant) Epoch() uint64                   { return 1 }
func (grant) Lost() <-chan struct{}           { return nil }
func (g grant) Release(context.Context) error { return g.err }

func TestContractRuntimeOwner(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a_held_session_fails_open_with_session_held_and_session_not_owned", rowAHeldSessionFailsOpenWithSessionHeldAndSessionNotOwned},
		{"catch_up_skips_a_session_that_another_owner_holds", rowCatchUpSkipsASessionThatAnotherOwnerHolds},
		{"a_release_error_reaches_the_caller_of_release_and_close", rowAReleaseErrorReachesTheCallerOfReleaseAndClose},
		{"the_local_owner_grants_a_session_once_at_a_time", rowTheLocalOwnerGrantsASessionOnceAtATime},
		{"new_reads_neither_owner_epoch_nor_sync_from_the_config", rowNewReadsNeitherOwnerEpochNorSyncFromTheConfig},
		{"a_run_token_without_a_serve_url_is_refused", rowARunTokenWithoutAServeURLIsRefused},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func rowAHeldSessionFailsOpenWithSessionHeldAndSessionNotOwned(t *testing.T) {
	st := harness.NewMemStore()
	seedSessions(t, st, "s1")
	r := lifetimeRuntimeOn(t, st, heldOwner{held: map[string]bool{"s1": true}})
	_, err := r.Open(t.Context(), "s1")
	if !errors.Is(err, harness.ErrSessionHeld) || !errors.Is(err, harness.ErrSessionNotOwned) {
		t.Errorf("Open = %v, want a match for ErrSessionHeld and ErrSessionNotOwned", err)
	}
	if got := protocol.CodeSessionNotOwned; !errors.Is(harness.ErrSessionHeld, harness.ErrSessionNotOwned) || got == "" {
		t.Errorf("ErrSessionHeld does not map to %q", got)
	}
}

func rowCatchUpSkipsASessionThatAnotherOwnerHolds(t *testing.T) {
	rcv := newSyncReceiver(t, nil)
	st := harness.NewMemStore()
	seedSessions(t, st, "s1", "s2")
	cfg := syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)
	r, err := harness.New(harness.Options{Getenv: os.Getenv, Store: st, Config: cfg, Sync: harness.NewHTTPSync(*cfg.Sync), Owner: heldOwner{held: map[string]bool{"s1": true}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	if err := r.CatchUp(t.Context()); err != nil {
		t.Fatalf("CatchUp = %v, want nil: a held session is skipped", err)
	}
	if len(readAll(t, rcv.store, "s2")) == 0 {
		t.Error("the receiver lacks s2")
	}
	if recs, _ := rcv.store.Read(t.Context(), "s1", 0, 10); len(recs) != 0 {
		t.Error("the receiver holds s1, which another owner holds")
	}
}

func rowAReleaseErrorReachesTheCallerOfReleaseAndClose(t *testing.T) {
	boom := errors.New("lease store down")
	for _, via := range []string{"Release", "Close"} {
		r := lifetimeRuntime(t, heldOwner{releaseErr: boom})
		s := lifetimeSession(t, r)
		var err error
		if via == "Release" {
			err = s.Release(t.Context())
		} else {
			err = r.Close(t.Context())
		}
		if !errors.Is(err, boom) {
			t.Errorf("%s = %v, want the release error", via, err)
		}
	}
}

func rowTheLocalOwnerGrantsASessionOnceAtATime(t *testing.T) {
	o := harness.NewLocalOwner(7)
	g, err := o.Acquire(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Epoch() != 7 || g.Lost() != nil {
		t.Errorf("epoch = %d, lost = %v; want 7 and a nil channel", g.Epoch(), g.Lost())
	}
	if _, err := o.Acquire(t.Context(), "s1"); !errors.Is(err, harness.ErrSessionHeld) {
		t.Errorf("second Acquire = %v, want ErrSessionHeld", err)
	}
	if err := g.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Acquire(t.Context(), "s1"); err != nil {
		t.Errorf("Acquire after Release = %v, want nil", err)
	}
}

func rowNewReadsNeitherOwnerEpochNorSyncFromTheConfig(t *testing.T) {
	rcv := newSyncReceiver(t, nil)
	st := harness.NewMemStore()
	r, err := harness.New(harness.Options{Getenv: os.Getenv, Store: st, Config: syncConfig(t, 7, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(rcv.snapshot()); n != 0 {
		t.Errorf("the receiver got %d requests, want none", n)
	}
	recs, err := st.Read(t.Context(), "s1", 0, 10)
	if err != nil || len(recs) < 2 || !bytes.Contains(recs[1].Data, []byte(`"epoch":1`)) {
		t.Errorf("owner.acquired = %s (err %v), want epoch 1", recs[1].Data, err)
	}
}

func rowARunTokenWithoutAServeURLIsRefused(t *testing.T) {
	_, err := harness.New(harness.Options{Getenv: os.Getenv, Store: harness.NewMemStore(), RunToken: "tok"})
	if err == nil {
		t.Error("New = nil, want an error")
	}
}
