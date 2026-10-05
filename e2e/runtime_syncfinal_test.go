package e2e

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func TestContractRuntimeSyncReceiverContract(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"sync_conflict_is_final_and_ends_the_session", rowSyncConflictIsFinalAndEndsTheSession},
		{"sync_invalid_request_is_final_and_ends_the_session", rowSyncInvalidRequestIsFinalAndEndsTheSession},
		{"sync_too_large_is_final_and_ends_the_session", rowSyncTooLargeIsFinalAndEndsTheSession},
		{"sync_unauthorized_and_forbidden_are_final", rowSyncUnauthorizedAndForbiddenAreFinal},
		{"sync_server_error_is_sent_again", rowSyncServerErrorIsSentAgain},
		{"sync_splits_a_batch_under_the_body_cap", rowSyncSplitsABatchUnderTheBodyCap},
		{"catch_up_conflict_skips_the_session_and_reports_it", rowCatchUpConflictSkipsTheSessionAndReportsIt},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func finalRejection(t *testing.T, status int, code string, want error) {
	t.Helper()
	rcv := newSyncReceiver(t, func(int, protocol.SyncBatch) (int, string) { return status, code })
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	rcv.awaitCalls(t, 1)
	if err := s.Release(t.Context()); !errors.Is(err, want) {
		t.Errorf("status %d: Release = %v, want %v", status, err, want)
	}
	if err := r.Close(t.Context()); err != nil {
		t.Errorf("status %d: Close = %v, want nil", status, err)
	}
	if n := len(rcv.snapshot()); n != 1 {
		t.Errorf("status %d: requests = %d, want 1: the sender stops the session and does not send again", status, n)
	}
}

func rowSyncConflictIsFinalAndEndsTheSession(t *testing.T) {
	finalRejection(t, http.StatusConflict, "sync_conflict", harness.ErrConflict)
}

func rowSyncInvalidRequestIsFinalAndEndsTheSession(t *testing.T) {
	finalRejection(t, http.StatusBadRequest, "invalid_request", harness.ErrInvalidRequest)
}

func rowSyncTooLargeIsFinalAndEndsTheSession(t *testing.T) {
	finalRejection(t, http.StatusRequestEntityTooLarge, "too_large", harness.ErrSyncRejected)
}

func rowSyncUnauthorizedAndForbiddenAreFinal(t *testing.T) {
	finalRejection(t, http.StatusUnauthorized, "unauthorized", harness.ErrSyncRejected)
	finalRejection(t, http.StatusForbidden, "forbidden", harness.ErrSyncRejected)
}

func rowSyncServerErrorIsSentAgain(t *testing.T) {
	rcv := newSyncReceiver(t, func(n int, _ protocol.SyncBatch) (int, string) {
		if n <= 2 {
			return http.StatusBadGateway, "internal"
		}
		return 0, ""
	})
	st := harness.NewMemStore()
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if n := len(rcv.snapshot()); n < 3 {
		t.Errorf("requests = %d, want the batch sent again after each 502", n)
	}
	equalLogs(t, st, rcv.store, "s1")
}

func rowSyncSplitsABatchUnderTheBodyCap(t *testing.T) {
	var up atomic.Bool
	rcv := newSyncReceiver(t, func(int, protocol.SyncBatch) (int, string) {
		if !up.Load() {
			return http.StatusServiceUnavailable, "internal"
		}
		return 0, ""
	})
	fake, st := harnesstest.NewChat(t, syncReply, syncReply), harness.NewMemStore()
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 1, rcv, fake.URL(), nil)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"a", "b"} {
		pdf := append([]byte("%PDF-1.4\n"), make([]byte, 12<<20)...)
		pdf[len(pdf)-1] = byte(i + 1)
		in := protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: "see"}, {Type: protocol.PartBlob, MediaType: "application/pdf", Data: pdf}}}
		if _, err := s.Submit(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		awaitTurns(t, s, i+1)
	}
	up.Store(true)
	if err := r.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	equalLogs(t, st, rcv.store, "s1")
	blobCalls := 0
	for _, c := range rcv.snapshot() {
		if len(c.batch.Blobs) > 0 {
			blobCalls++
		}
	}
	if blobCalls < 2 {
		t.Errorf("requests with blobs = %d, want the two attachments in separate requests", blobCalls)
	}
}

func awaitTurns(t *testing.T, s *harness.Session, n int) {
	t.Helper()
	ended := 0
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			if ended++; ended == n {
				return
			}
		}
	}
}

func rowCatchUpConflictSkipsTheSessionAndReportsIt(t *testing.T) {
	st := harness.NewMemStore()
	seedSessions(t, st, "s1", "s2")
	rcv := newSyncReceiver(t, func(_ int, b protocol.SyncBatch) (int, string) {
		if b.Session == "s1" {
			return http.StatusConflict, "sync_conflict"
		}
		return 0, ""
	})
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	err = r.CatchUp(t.Context())
	if !errors.Is(err, harness.ErrConflict) || !strings.Contains(err.Error(), "s1") {
		t.Errorf("CatchUp = %v, want ErrConflict that names s1", err)
	}
	equalLogs(t, st, rcv.store, "s2")
	if err := r.Close(t.Context()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}
