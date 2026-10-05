package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

const (
	syncToken = "box-sync-token"
	syncPath  = "/v1/boxes/box-1/sync"
)

const syncBodyCap = 32 << 20

type syncCall struct {
	auth  string
	batch protocol.SyncBatch
	size  int
}

// syncReceiver is a fake control plane. It answers POST /v1/boxes/{id}/sync
// with harness.ApplySync over its own store, or with what script returns.
type syncReceiver struct {
	srv   *httptest.Server
	store harness.Store
	// script returns a status and an error code for call n with batch b, or 0 to apply the batch.
	script func(n int, b protocol.SyncBatch) (int, string)

	mu      sync.Mutex
	calls   []syncCall
	changed chan struct{}
}

func newSyncReceiver(t *testing.T, script func(n int, b protocol.SyncBatch) (int, string)) *syncReceiver {
	t.Helper()
	r := &syncReceiver{store: harness.NewMemStore(), script: script, changed: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+syncPath, r.handle)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *syncReceiver) handle(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, syncBodyCap))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeSyncError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	var b protocol.SyncBatch
	if err != nil || json.Unmarshal(body, &b) != nil {
		writeSyncError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, syncCall{req.Header.Get("Authorization"), b, len(body)})
	close(r.changed)
	r.changed = make(chan struct{})
	status, code := 0, ""
	if r.script != nil {
		status, code = r.script(len(r.calls), b)
	}
	var ack protocol.SyncAck
	if status == 0 {
		var err error
		switch ack, err = harness.ApplySync(req.Context(), r.store, b); {
		case errors.Is(err, harness.ErrStaleEpoch):
			status, code = http.StatusConflict, "stale_epoch"
		case errors.Is(err, harness.ErrConflict):
			status, code = http.StatusConflict, "sync_conflict"
		case errors.Is(err, harness.ErrInvalidRequest):
			status, code = http.StatusBadRequest, "invalid_request"
		case err != nil:
			status, code = http.StatusInternalServerError, "internal"
		}
	}
	if status != 0 {
		writeSyncError(w, status, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ack)
}

func writeSyncError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(protocol.ErrorBody{Error: protocol.Error{Code: code, Message: "scripted"}})
}

func (r *syncReceiver) snapshot() []syncCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// awaitCalls returns once the receiver has answered n requests.
func (r *syncReceiver) awaitCalls(t *testing.T, n int) []syncCall {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	for {
		r.mu.Lock()
		calls, changed := slices.Clone(r.calls), r.changed
		r.mu.Unlock()
		if len(calls) >= n {
			return calls
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("the receiver got %d requests, want %d", len(calls), n)
		}
	}
}

// syncConfig writes the config file that boxinit writes for a box and loads
// it as serve does.
func syncConfig(t *testing.T, epoch int, receiver *syncReceiver, modelURL string, extra map[string]any) config.Config {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "sync-token")
	if err := os.WriteFile(tokenFile, []byte(syncToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{
		"context_window_tokens": 100000,
		"providers":             map[string]any{"bifrost": map[string]any{"type": config.TypeOpenAICompat, "base_url": modelURL, "api_key_env": "HARNESS_E2E_KEY"}},
		"owner_epoch":           epoch,
		"sync":                  map[string]any{"url": receiver.srv.URL + syncPath, "token_file": tokenFile},
	}
	for k, v := range extra {
		raw[k] = v
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return *c
}

func awaitIdle(t *testing.T, s *harness.Session) {
	t.Helper()
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			return
		}
	}
}

func readAll(t *testing.T, st harness.Store, id string) [][]byte {
	t.Helper()
	recs, err := st.Read(t.Context(), id, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, r := range recs {
		out = append(out, r.Data)
	}
	return out
}

func equalLogs(t *testing.T, want, got harness.Store, id string) {
	t.Helper()
	w, g := readAll(t, want, id), readAll(t, got, id)
	if len(w) == 0 || len(w) != len(g) {
		t.Fatalf("session %s: receiver holds %d records, want %d", id, len(g), len(w))
	}
	for i := range w {
		if !bytes.Equal(w[i], g[i]) {
			t.Fatalf("session %s: record %d differs on the receiver", id, i+1)
		}
	}
}

// seedSessions stores one session for each id through a runtime with no Sync,
// and returns the head of each.
func seedSessions(t *testing.T, st harness.Store, ids ...string) map[string]uint64 {
	t.Helper()
	r, err := harness.New(harness.Options{Store: st, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: "http://127.0.0.1:1", APIKeyEnv: "HARNESS_E2E_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	heads := map[string]uint64{}
	for _, id := range ids {
		if _, err := r.Create(t.Context(), protocol.CreateSession{ID: id, Model: "bifrost/gpt-test"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if heads[id], err = st.Head(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	return heads
}

type noOwner struct{}

func (noOwner) Acquire(context.Context, string) (harness.Ownership, error) {
	return nil, errors.New("no grant")
}

type noSync struct{}

func (noSync) Deliver(context.Context, protocol.SyncBatch) (protocol.SyncAck, error) {
	return protocol.SyncAck{}, errors.New("no receiver")
}

func TestContractRuntimeSyncClient(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"sync_posts_the_log_with_the_bearer_token_and_the_epoch", rowSyncPostsTheLogWithTheBearerTokenAndTheEpoch},
		{"sync_resends_the_same_batch_after_a_failed_post", rowSyncResendsTheSameBatchAfterAFailedPost},
		{"sync_stale_epoch_is_final_and_ends_the_session", rowSyncStaleEpochIsFinalAndEndsTheSession},
		{"catch_up_replicates_every_stored_session_without_opening_it", rowCatchUpReplicatesEveryStoredSessionWithoutOpeningIt},
		{"catch_up_stale_epoch_fails", rowCatchUpStaleEpochFails},
		{"catch_up_gives_way_to_open_of_the_session_that_it_holds", rowCatchUpGivesWayToOpenOfTheSessionThatItHolds},
		{"close_ends_a_catch_up_that_retries", rowCloseEndsACatchUpThatRetries},
		{"catch_up_skips_a_session_that_it_cannot_read_and_reports_it", rowCatchUpSkipsASessionThatItCannotReadAndReportsIt},
		{"sync_reads_the_token_file_again_for_each_post", rowSyncReadsTheTokenFileAgainForEachPost},
		{"sync_sends_the_batch_again_while_the_token_file_is_unreadable", rowSyncSendsTheBatchAgainWhileTheTokenFileIsUnreadable},
		{"catch_up_reports_a_session_whose_open_fails_after_it_took_the_grant", rowCatchUpReportsASessionWhoseOpenFailsAfterItTookTheGrant},
		{"two_catch_ups_at_once_let_open_take_the_session", rowTwoCatchUpsAtOnceLetOpenTakeTheSession},
		{"plugin_gets_serve_url_and_run_token_from_options", rowPluginGetsServeUrlAndRunTokenFromOptions},
		{"owner_epoch_with_an_owner_is_refused", rowOwnerEpochWithAnOwnerIsRefused},
		{"sync_config_with_a_sync_option_is_refused", rowSyncConfigWithASyncOptionIsRefused},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

var syncReply = harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "done"}}

func rowSyncPostsTheLogWithTheBearerTokenAndTheEpoch(t *testing.T) {
	rcv, fake, st := newSyncReceiver(t, nil), harnesstest.NewChat(t, syncReply), harness.NewMemStore()
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 7, rcv, fake.URL(), nil)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, s)
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	equalLogs(t, st, rcv.store, "s1")
	calls := rcv.snapshot()
	for i, c := range calls {
		if c.auth != "Bearer "+syncToken || c.batch.Epoch != 7 || c.batch.Session != "s1" {
			t.Errorf("request %d: bearer token %v, epoch %d, session %q; want the token, epoch 7, s1", i, c.auth == "Bearer "+syncToken, c.batch.Epoch, c.batch.Session)
		}
	}
}

func rowSyncResendsTheSameBatchAfterAFailedPost(t *testing.T) {
	rcv := newSyncReceiver(t, func(n int, _ protocol.SyncBatch) (int, string) {
		if n == 1 {
			return http.StatusServiceUnavailable, "internal"
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
		t.Fatal(err)
	}
	calls := rcv.snapshot()
	if len(calls) < 2 || calls[0].batch.FromSeq != calls[1].batch.FromSeq || !slices.EqualFunc(calls[0].batch.Records, calls[1].batch.Records, bytes.Equal) {
		t.Fatalf("requests = %d, want the failed batch sent again unchanged", len(calls))
	}
	equalLogs(t, st, rcv.store, "s1")
}

func rowSyncStaleEpochIsFinalAndEndsTheSession(t *testing.T) {
	rcv := newSyncReceiver(t, func(int, protocol.SyncBatch) (int, string) { return http.StatusConflict, "stale_epoch" })
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	rcv.awaitCalls(t, 1)
	if err := s.Release(t.Context()); !errors.Is(err, harness.ErrSessionNotOwned) {
		t.Errorf("Release = %v, want ErrSessionNotOwned", err)
	}
	if err := r.Close(t.Context()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if n := len(rcv.snapshot()); n != 1 {
		t.Errorf("requests = %d, want 1: a stale epoch is not sent again", n)
	}
}

func rowCatchUpReplicatesEveryStoredSessionWithoutOpeningIt(t *testing.T) {
	st := harness.NewMemStore()
	heads := seedSessions(t, st, "s1", "s2")
	rcv := newSyncReceiver(t, nil)
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 9, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CatchUp(t.Context()); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	for id, head := range heads {
		equalLogs(t, st, rcv.store, id)
		if got, err := st.Head(t.Context(), id); err != nil || got != head {
			t.Errorf("session %s head = %d, %v; want %d: catch-up appends nothing", id, got, err, head)
		}
	}
	for _, c := range rcv.snapshot() {
		if c.batch.Epoch != 9 || c.auth != "Bearer "+syncToken {
			t.Errorf("batch of %s: epoch %d, bearer token %v; want epoch 9 and the token", c.batch.Session, c.batch.Epoch, c.auth == "Bearer "+syncToken)
		}
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func rowCatchUpStaleEpochFails(t *testing.T) {
	st := harness.NewMemStore()
	seedSessions(t, st, "s1")
	rcv := newSyncReceiver(t, func(int, protocol.SyncBatch) (int, string) { return http.StatusConflict, "stale_epoch" })
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CatchUp(t.Context()); !errors.Is(err, harness.ErrStaleEpoch) {
		t.Errorf("CatchUp = %v, want ErrStaleEpoch", err)
	}
	if err := r.Close(t.Context()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

func rowPluginGetsServeUrlAndRunTokenFromOptions(t *testing.T) {
	fake := harnesstest.NewChat(t, syncReply)
	raw := pluginConfig(t, map[string]any{"serve": true})
	raw["providers"] = map[string]any{"bifrost": map[string]any{"type": config.TypeOpenAICompat, "base_url": fake.URL(), "api_key_env": "HARNESS_E2E_KEY"}}
	raw["context_window_tokens"] = 100000
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: *c, ServeURL: "http://127.0.0.1:4242", RunToken: "run-token-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, s)
	reqs := fake.Requests()
	if len(reqs) == 0 || !strings.Contains(reqs[0].System, "SERVE: http://127.0.0.1:4242 TOKEN: run-token-1") {
		t.Errorf("the system prompt lacks the serve url and the run token of the plugin")
	}
}

func rowOwnerEpochWithAnOwnerIsRefused(t *testing.T) {
	rcv := newSyncReceiver(t, nil)
	_, err := harness.New(harness.Options{Store: harness.NewMemStore(), Owner: noOwner{}, Config: syncConfig(t, 3, rcv, "http://127.0.0.1:1", map[string]any{"sync": nil})})
	if !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("New = %v, want ErrInvalidRequest", err)
	}
}

func rowSyncConfigWithASyncOptionIsRefused(t *testing.T) {
	rcv := newSyncReceiver(t, nil)
	_, err := harness.New(harness.Options{Store: harness.NewMemStore(), Sync: noSync{}, Config: syncConfig(t, 0, rcv, "http://127.0.0.1:1", map[string]any{"owner_epoch": nil})})
	if !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("New = %v, want ErrInvalidRequest", err)
	}
}

func downUntil(n int) func(int, protocol.SyncBatch) (int, string) {
	return func(call int, _ protocol.SyncBatch) (int, string) {
		if call <= n {
			return http.StatusServiceUnavailable, "internal"
		}
		return 0, ""
	}
}

func rowCatchUpGivesWayToOpenOfTheSessionThatItHolds(t *testing.T) {
	st := harness.NewMemStore()
	seedSessions(t, st, "s1")
	rcv := newSyncReceiver(t, downUntil(2))
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.CatchUp(t.Context()) }()
	rcv.awaitCalls(t, 1)
	if _, err := r.Open(t.Context(), "s1"); err != nil {
		t.Fatalf("Open while catch-up retries = %v, want nil", err)
	}
	if err := <-done; err != nil {
		t.Errorf("CatchUp = %v, want nil: the opened session replicates itself", err)
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	equalLogs(t, st, rcv.store, "s1")
}

func rowCloseEndsACatchUpThatRetries(t *testing.T) {
	st := harness.NewMemStore()
	seedSessions(t, st, "s1")
	rcv := newSyncReceiver(t, func(int, protocol.SyncBatch) (int, string) { return http.StatusServiceUnavailable, "internal" })
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.CatchUp(t.Context()) }()
	rcv.awaitCalls(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Errorf("Close = %v, want nil: a catch-up resumes at the next start", err)
	}
	if err := <-done; !errors.Is(err, harness.ErrDraining) {
		t.Errorf("CatchUp = %v, want ErrDraining", err)
	}
}

type unreadableLog struct {
	harness.Store
	id string
}

func (u unreadableLog) Read(ctx context.Context, id string, after uint64, limit int) ([]harness.Record, error) {
	if id == u.id {
		return nil, errors.New("disk read failed")
	}
	return u.Store.Read(ctx, id, after, limit)
}

func rowCatchUpSkipsASessionThatItCannotReadAndReportsIt(t *testing.T) {
	mem := harness.NewMemStore()
	seedSessions(t, mem, "s1", "s2")
	rcv := newSyncReceiver(t, nil)
	r, err := harness.New(harness.Options{Store: unreadableLog{mem, "s1"}, Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CatchUp(t.Context()); err == nil || !strings.Contains(err.Error(), "s1") {
		t.Errorf("CatchUp = %v, want an error that names s1", err)
	}
	equalLogs(t, mem, rcv.store, "s2")
	if err := r.Close(t.Context()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

func rowSyncReadsTheTokenFileAgainForEachPost(t *testing.T) {
	st := harness.NewMemStore()
	seedSessions(t, st, "s1")
	var tokenFile string
	rcv := newSyncReceiver(t, func(n int, _ protocol.SyncBatch) (int, string) {
		if n == 1 {
			if err := os.WriteFile(tokenFile, []byte("rotated\n"), 0o600); err != nil {
				t.Error(err)
			}
			return http.StatusServiceUnavailable, "internal"
		}
		return 0, ""
	})
	c := syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)
	tokenFile = c.Sync.TokenFile
	r, err := harness.New(harness.Options{Store: st, Config: c})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CatchUp(t.Context()); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	calls := rcv.snapshot()
	if len(calls) < 2 || calls[0].auth != "Bearer "+syncToken || calls[1].auth != "Bearer rotated" {
		t.Errorf("requests = %d, want the second post to carry the rotated token", len(calls))
	}
	if err := r.Close(t.Context()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

type hookedStore struct {
	harness.Store
	read func(id string) error
}

func (h hookedStore) Read(ctx context.Context, id string, after uint64, limit int) ([]harness.Record, error) {
	if err := h.read(id); err != nil {
		return nil, err
	}
	return h.Store.Read(ctx, id, after, limit)
}

func rowSyncSendsTheBatchAgainWhileTheTokenFileIsUnreadable(t *testing.T) {
	mem := harness.NewMemStore()
	seedSessions(t, mem, "s1")
	rcv := newSyncReceiver(t, nil)
	c := syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)
	if err := os.Remove(c.Sync.TokenFile); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	st := hookedStore{mem, func(string) error {
		if reads.Add(1) == 2 {
			return os.WriteFile(c.Sync.TokenFile, []byte(syncToken+"\n"), 0o600)
		}
		return nil
	}}
	r, err := harness.New(harness.Options{Store: st, Config: c})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CatchUp(t.Context()); err != nil {
		t.Fatalf("CatchUp: %v, want nil once the token file can be read", err)
	}
	equalLogs(t, mem, rcv.store, "s1")
	if err := r.Close(t.Context()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

func rowCatchUpReportsASessionWhoseOpenFailsAfterItTookTheGrant(t *testing.T) {
	mem := harness.NewMemStore()
	seedSessions(t, mem, "s1")
	var failReads atomic.Bool
	st := hookedStore{mem, func(string) error {
		if failReads.Load() {
			return errors.New("disk read failed")
		}
		return nil
	}}
	rcv := newSyncReceiver(t, downUntil(1000))
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.CatchUp(t.Context()) }()
	rcv.awaitCalls(t, 1)
	failReads.Store(true)
	if _, err := r.Open(t.Context(), "s1"); err == nil {
		t.Fatal("Open = nil, want the read failure")
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "s1") {
		t.Errorf("CatchUp = %v, want an error that names s1: the failed Open left the session unreplicated", err)
	}
	if err := r.Close(t.Context()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

func rowTwoCatchUpsAtOnceLetOpenTakeTheSession(t *testing.T) {
	st := harness.NewMemStore()
	seedSessions(t, st, "s1")
	rcv := newSyncReceiver(t, downUntil(2))
	r, err := harness.New(harness.Options{Store: st, Config: syncConfig(t, 1, rcv, "http://127.0.0.1:1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- r.CatchUp(t.Context()) }()
	}
	rcv.awaitCalls(t, 1)
	if _, err := r.Open(t.Context(), "s1"); err != nil {
		t.Fatalf("Open while two catch-ups run = %v, want nil", err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Errorf("CatchUp = %v, want nil", err)
		}
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	equalLogs(t, st, rcv.store, "s1")
}
