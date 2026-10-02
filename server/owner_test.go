package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

type fakeOwnership struct {
	lost     chan struct{}
	id       string
	owner    *fakeOwner
	releases sync.Once
}

func (o *fakeOwnership) Lost() <-chan struct{} { return o.lost }

func (o *fakeOwnership) Release() {
	o.releases.Do(func() { o.owner.released <- o.id })
}

type fakeOwner struct {
	mu       sync.Mutex
	err      error
	acquired []string
	held     map[string]*fakeOwnership
	released chan string
}

func newFakeOwner() *fakeOwner {
	return &fakeOwner{held: map[string]*fakeOwnership{}, released: make(chan string, 16)}
}

func (f *fakeOwner) Acquire(_ context.Context, id string) (Ownership, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, id)
	if f.err != nil {
		return nil, f.err
	}
	o := &fakeOwnership{lost: make(chan struct{}), id: id, owner: f}
	f.held[id] = o
	return o, nil
}

func (f *fakeOwner) acquireCalls(id string) (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.acquired {
		if a == id {
			n++
		}
	}
	return n
}

func (f *fakeOwner) loseOwnership(id string) {
	f.mu.Lock()
	o := f.held[id]
	f.mu.Unlock()
	close(o.lost)
}

type loadRecordingStore struct {
	engine.SessionStore
	mu      sync.Mutex
	loadIDs []string
}

func (s *loadRecordingStore) Load(id string) ([][]byte, error) {
	s.mu.Lock()
	s.loadIDs = append(s.loadIDs, id)
	s.mu.Unlock()
	return s.SessionStore.Load(id)
}

func (s *loadRecordingStore) loaded(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.loadIDs {
		if l == id {
			return true
		}
	}
	return false
}

type cancelWatchProvider struct {
	name     string
	started  chan struct{}
	canceled chan struct{}
}

func (p *cancelWatchProvider) Name() string {
	if p.name == "" {
		return "test"
	}
	return p.name
}

func (p *cancelWatchProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	return &cancelWatchStream{p: p, ctx: ctx}, nil
}

type cancelWatchStream struct {
	p   *cancelWatchProvider
	ctx context.Context
}

func (s *cancelWatchStream) Next() (provider.Event, error) {
	close(s.p.started)
	<-s.ctx.Done()
	close(s.p.canceled)
	return provider.Event{}, s.ctx.Err()
}

func (s *cancelWatchStream) Close() error { return nil }

func newOwnerServer(t *testing.T, store engine.SessionStore, prov provider.Provider, mutate ...func(*Options)) *harness {
	t.Helper()
	return newOwnerServerCap(t, store, prov, 3, mutate...)
}

func newOwnerServerCap(t *testing.T, store engine.SessionStore, prov provider.Provider, maxResumes int, mutate ...func(*Options)) *harness {
	t.Helper()
	const token = "secret-run-token"
	model := message.ModelRef{Provider: prov.Name(), Model: "m1"}
	var srv *Server
	mkCfg := func(m message.ModelRef) engine.Config {
		if m.IsZero() {
			m = model
		}
		cfg := engine.Config{
			Providers:      provider.Registry{prov.Name(): prov},
			Model:          m,
			SessionStore:   store,
			MaxTurnResumes: maxResumes,
			OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
		}
		if tp, ok := prov.(interface{ tools() []engine.Tool }); ok {
			cfg.Tools = tp.tools()
		}
		return cfg
	}
	opts := Options{
		Store:             store,
		RunToken:          token,
		HeartbeatInterval: 5 * time.Millisecond,
		NewSession: func(m message.ModelRef, workDir string, parent string) (*engine.Session, error) {
			cfg := mkCfg(m)
			cfg.WorkDir = workDir
			cfg.ParentSession = parent
			return engine.NewSession(cfg), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(mkCfg(message.ModelRef{}), id)
		},
	}
	for _, m := range mutate {
		m(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { srv.Close() })
	return &harness{t: t, token: token, srv: srv, ts: ts}
}

func TestOwnerAcquireRefusedDoesNotLoad(t *testing.T) {
	store := &loadRecordingStore{SessionStore: engine.NewMemStore()}
	seed := newOwnerServer(t, store, &scriptedProvider{name: "test"})
	id := seed.createSession("")
	owner := newFakeOwner()
	owner.err = errors.New("lease held elsewhere")
	h := newOwnerServer(t, store, &scriptedProvider{name: "test"}, func(o *Options) { o.SessionOwner = owner })
	store.mu.Lock()
	store.loadIDs = nil
	store.mu.Unlock()

	resp, body := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("session not owned by this server")) {
		t.Errorf("body = %s, want it to name the ownership refusal", body)
	}
	if store.loaded(id) {
		t.Errorf("Load(%q) ran although Acquire refused", id)
	}
}

func TestOwnerAcquiredOncePerResidency(t *testing.T) {
	owner := newFakeOwner()
	h := newOwnerServer(t, engine.NewMemStore(), &scriptedProvider{name: "test"}, func(o *Options) {
		o.SessionOwner = owner
		o.MaxResident = 1
	})
	id := h.createSession("")
	for range 3 {
		if resp, body := h.do("GET", "/session/"+id, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET status = %d: %s", resp.StatusCode, body)
		}
	}
	if n := owner.acquireCalls(id); n != 1 {
		t.Fatalf("Acquire(%q) called %d times, want 1", id, n)
	}

	other := h.createSession("")
	if got := <-owner.released; got != id {
		t.Fatalf("Release for %q, want the evicted %q", got, id)
	}
	if n := owner.acquireCalls(other); n != 1 {
		t.Errorf("Acquire(%q) called %d times, want 1", other, n)
	}
}

func TestOwnershipLostStopsServing(t *testing.T) {
	store := engine.NewMemStore()
	prov := &cancelWatchProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	owner := newFakeOwner()
	h := newOwnerServer(t, store, prov, func(o *Options) { o.SessionOwner = owner })
	id := h.createSession("")

	if resp, body := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{"parts": []map[string]string{{"type": "text", "text": "hi"}}}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status = %d: %s", resp.StatusCode, body)
	}
	<-prov.started
	owner.loseOwnership(id)
	<-prov.canceled
	h.srv.wg.Wait()

	recs, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if bytes.Contains(r, []byte(`"child_turn.settled"`)) {
			t.Fatalf("lost turn was settled: %s", r)
		}
	}
	reloaded, err := engine.LoadSession(engine.Config{
		Providers:      provider.Registry{"test": prov},
		Model:          message.ModelRef{Provider: "test", Model: "m1"},
		SessionStore:   store,
		MaxTurnResumes: 3,
	}, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.ResumableTurn() {
		t.Error("ResumableTurn() = false; the next holder cannot resume the turn")
	}

	resp, body := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("session not owned by this server")) {
		t.Errorf("GET after loss = %d %s, want 409 session not owned by this server", resp.StatusCode, body)
	}
}

func TestNilOwnerAsksNobody(t *testing.T) {
	h := newOwnerServer(t, engine.NewMemStore(), &scriptedProvider{name: "test"})
	id := h.createSession("")
	if resp, body := h.do("GET", "/session/"+id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d: %s", resp.StatusCode, body)
	}
}

func TestEventsLogConflictFencesServer(t *testing.T) {
	store := engine.NewMemStore()
	h := newOwnerServer(t, store, &scriptedProvider{name: "test"})
	id := h.createSession("")

	n, err := store.Len(defaultEventLogID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(defaultEventLogID, n, []byte(`{"type":"other.writer"}`)); err != nil {
		t.Fatal(err)
	}

	select {
	case <-h.srv.Fenced():
		t.Fatal("Fenced() closed before the conflict")
	default:
	}
	snapshot := func() (int64, int) {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		return h.srv.seq, len(h.srv.journal)
	}
	seq0, ring0 := snapshot()
	for range 2 {
		h.srv.emitDurable(Event{Type: evtSessionCreated, SessionID: id})
		select {
		case <-h.srv.Fenced():
		default:
			t.Fatal("Fenced() still open after the conflict")
		}
		if seq, ring := snapshot(); seq != seq0 || ring != ring0 {
			t.Fatalf("after fenced emit seq=%d ring=%d, want seq=%d ring=%d", seq, ring, seq0, ring0)
		}
	}

	resp, body := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("session not owned by this server")) {
		t.Errorf("GET after fence = %d %s, want 409 session not owned by this server", resp.StatusCode, body)
	}
	resp, body = h.do("POST", "/session", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("POST /session after fence = %d %s, want 409", resp.StatusCode, body)
	}
}

func hasSettled(t *testing.T, store engine.SessionStore, id string) bool {
	t.Helper()
	return hasRecord(t, store, id, "child_turn.settled")
}

func hasRecord(t *testing.T, store engine.SessionStore, id, typ string) bool {
	t.Helper()
	recs, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if bytes.Contains(r, []byte(`"`+typ+`"`)) {
			return true
		}
	}
	return false
}

func TestEventsLogConflictCancelsTurnUnsettled(t *testing.T) {
	store := engine.NewMemStore()
	prov := &cancelWatchProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	h := newOwnerServer(t, store, prov)
	id := h.createSession("")
	if resp, body := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{"parts": []map[string]string{{"type": "text", "text": "hi"}}}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status = %d: %s", resp.StatusCode, body)
	}
	<-prov.started

	n, err := store.Len(defaultEventLogID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(defaultEventLogID, n, []byte(`{"type":"other.writer"}`)); err != nil {
		t.Fatal(err)
	}
	h.srv.emitDurable(Event{Type: evtSessionCreated, SessionID: id})
	<-prov.canceled
	h.srv.wg.Wait()

	if hasSettled(t, store, id) {
		t.Error("fenced turn was settled")
	}
	reloaded, err := engine.LoadSession(engine.Config{
		Providers:      provider.Registry{"test": prov},
		Model:          message.ModelRef{Provider: "test", Model: "m1"},
		SessionStore:   store,
		MaxTurnResumes: 3,
	}, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.ResumableTurn() {
		t.Error("ResumableTurn() = false; the next holder cannot resume the turn")
	}
}

func TestOwnershipLostSuspendsChildren(t *testing.T) {
	dir, err := os.MkdirTemp("", "owner-children")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	rootProv := &scriptedProvider{name: "root"}
	childProv := &cancelWatchProvider{name: "child", started: make(chan struct{}), canceled: make(chan struct{})}
	owner := newFakeOwner()
	h := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, func(o *Options) { o.SessionOwner = owner }, rootProv, childProv)
	t.Cleanup(func() { h.srv.Close() })
	root := h.createSession("root/m1")
	resp, data := h.do("POST", "/session", map[string]string{
		"parent_id": root, "agent": engine.AgentExplore, "prompt": "wait", "model": "child/m1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("spawn status = %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)
	<-childProv.started

	h.srv.mu.Lock()
	lostSeq := h.srv.seq
	h.srv.mu.Unlock()
	owner.loseOwnership(root)
	<-childProv.canceled

	mgr := h.srv.SessionManager()
	for {
		changed := mgr.Changed()
		if _, info, ok := mgr.SessionAndInfo(child.ID); ok && info.Suspended {
			break
		}
		<-changed
	}

	store := h.srv.store
	for _, id := range []string{root, child.ID} {
		if hasSettled(t, store, id) {
			t.Errorf("session %s has a settle record", id)
		}
	}
	if hasRecord(t, store, child.ID, "task.outcome_committed") {
		t.Error("child log has a committed outcome")
	}
	if hasRecord(t, store, root, "task.notify_queued") {
		t.Error("parent log has a queued task notification")
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	for _, ev := range h.srv.journal {
		if ev.SessionID == child.ID && ev.Seq > lostSeq {
			t.Errorf("durable emit for child after loss: %s seq %d", ev.Type, ev.Seq)
		}
	}
}

func TestRefusingOwnerBlocksSpawnAndClaimLoads(t *testing.T) {
	store := &loadRecordingStore{SessionStore: engine.NewMemStore()}
	seed := newOwnerServer(t, store, &scriptedProvider{name: "test"})
	id := seed.createSession("")
	owner := newFakeOwner()
	owner.err = errors.New("lease held elsewhere")
	h := newOwnerServer(t, store, &scriptedProvider{name: "test"}, func(o *Options) { o.SessionOwner = owner })
	store.mu.Lock()
	store.loadIDs = nil
	store.mu.Unlock()

	resp, body := h.do("POST", "/session", map[string]string{"parent_id": id, "agent": engine.AgentExplore, "prompt": "go"})
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("session not owned by this server")) {
		t.Errorf("spawn = %d %s, want 409 session not owned by this server", resp.StatusCode, body)
	}
	if _, _, _, code, _ := h.srv.claimForPrompt(id); code != http.StatusConflict {
		t.Errorf("claimForPrompt code = %d, want 409", code)
	}
	if store.loaded(id) {
		t.Errorf("Load(%q) ran although Acquire refused", id)
	}
}

type blockingLoadStore struct {
	engine.SessionStore
	id      string
	armed   bool
	reached chan struct{}
	release chan struct{}
}

func (s *blockingLoadStore) Load(id string) ([][]byte, error) {
	if s.armed && id == s.id {
		close(s.reached)
		<-s.release
	}
	return s.SessionStore.Load(id)
}

func TestOwnershipLostDuringColdLoadRefusesClaim(t *testing.T) {
	mem := engine.NewMemStore()
	seed := newOwnerServer(t, mem, &scriptedProvider{name: "test"})
	id := seed.createSession("")
	store := &blockingLoadStore{SessionStore: mem, id: id, reached: make(chan struct{}), release: make(chan struct{})}
	owner := newFakeOwner()
	h := newOwnerServer(t, store, &scriptedProvider{name: "test"}, func(o *Options) { o.SessionOwner = owner })

	store.armed = true
	type result struct{ code int }
	res := make(chan result, 1)
	go func() {
		_, _, _, code, _ := h.srv.claimForPrompt(id)
		res <- result{code}
	}()
	<-store.reached
	owner.loseOwnership(id)
	for {
		h.srv.mu.Lock()
		_, lost := h.srv.refused[id]
		h.srv.mu.Unlock()
		if lost {
			break
		}
		runtime.Gosched()
	}
	close(store.release)
	if got := (<-res).code; got != http.StatusConflict {
		t.Errorf("claimForPrompt code = %d, want 409", got)
	}
	if h.srv.residentSession(id) != nil {
		t.Error("session became resident after loss")
	}
}
