package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// --- scripted provider (copied pattern from engine/engine_test.go) ---

type scriptedProvider struct {
	name  string
	mu    sync.Mutex
	turns [][]provider.Event
	call  int
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Stream(_ context.Context, _ *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.call >= len(p.turns) {
		return nil, io.ErrUnexpectedEOF
	}
	events := p.turns[p.call]
	p.call++
	return &scriptedStream{events: events}, nil
}

type scriptedStream struct {
	events []provider.Event
	i      int
}

func (s *scriptedStream) Next() (provider.Event, error) {
	if s.i >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

func (s *scriptedStream) Close() error { return nil }

func asstTurn(text string) []provider.Event {
	msg := &message.Message{ID: message.ProviderCallID("m", text, 12), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}}
	return []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}}
}

// blockingProvider blocks in Next until its context is cancelled or release is
// closed — no sleeps, deterministic for busy/abort tests.
type blockingProvider struct {
	name    string
	started chan struct{}
	release chan struct{}
	once    sync.Once
	relOnce sync.Once
}

func newBlockingProvider(name string) *blockingProvider {
	return &blockingProvider{name: name, started: make(chan struct{}), release: make(chan struct{})}
}

func (p *blockingProvider) Name() string { return p.name }

// releaseAll unblocks every parked Next; safe to call more than once (a test may
// release explicitly and a t.Cleanup may release again).
func (p *blockingProvider) releaseAll() { p.relOnce.Do(func() { close(p.release) }) }

func (p *blockingProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	return &blockingStream{p: p, ctx: ctx}, nil
}

type blockingStream struct {
	p   *blockingProvider
	ctx context.Context
}

func (s *blockingStream) Next() (provider.Event, error) {
	s.p.once.Do(func() { close(s.p.started) })
	select {
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	case <-s.p.release:
		msg := &message.Message{ID: "msg_released", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "released"}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	}
}

func (s *blockingStream) Close() error { return nil }

// --- harness ---

type harness struct {
	t     *testing.T
	dir   string
	token string
	srv   *Server
	ts    *httptest.Server
}

func newHarness(t *testing.T, prov provider.Provider) *harness {
	t.Helper()
	return newHarnessDir(t, t.TempDir(), prov)
}

func newHarnessDir(t *testing.T, dir string, prov provider.Provider) *harness {
	t.Helper()
	return newHarnessOpts(t, dir, prov, 0)
}

// newHarnessOpts builds a harness with an explicit MaxResident (0 = default).
func newHarnessOpts(t *testing.T, dir string, prov provider.Provider, maxResident int) *harness {
	t.Helper()
	const token = "secret-run-token"
	srv := newServer(t, dir, prov, maxResident)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

// newServer builds a *Server without an HTTP listener, so its handlers can be
// driven directly (e.g. inside a synctest bubble, where real sockets are
// unavailable).
func newServer(t *testing.T, dir string, prov provider.Provider, maxResident int, mutate ...func(*Options)) *Server {
	t.Helper()
	const token = "secret-run-token"
	model := message.ModelRef{Provider: prov.Name(), Model: "m1"}
	var srv *Server
	mkCfg := func(m message.ModelRef) engine.Config {
		if m.IsZero() {
			m = model
		}
		return engine.Config{
			Providers:  provider.Registry{prov.Name(): prov},
			Model:      m,
			SessionDir: dir,
			OnEvent:    func(ev engine.Event) { srv.Publish(ev) },
		}
	}
	opts := Options{
		SessionDir:        dir,
		RunToken:          token,
		Version:           "9.9.9",
		HeartbeatInterval: 20 * time.Millisecond,
		MaxResident:       maxResident,
		NewSession: func(m message.ModelRef, workDir string, parentSession string) (*engine.Session, error) {
			cfg := mkCfg(m)
			cfg.WorkDir = workDir
			cfg.ParentSession = parentSession
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
	return srv
}

// newCORSHarness builds a harness whose server echoes origin in CORS headers.
func newCORSHarness(t *testing.T, origin string) *harness {
	t.Helper()
	const token = "secret-run-token"
	dir := t.TempDir()
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.CORSOrigin = origin
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

func (h *harness) do(method, path string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp, data
}

// doRaw is do's counterpart for a caller that needs to send a literal JSON
// body string — e.g. to distinguish an absent field from an explicit empty
// string, which map[string]string cannot express (both marshal to "" or
// omission depending on struct tags, but a plain map always includes the
// key).
func (h *harness) doRaw(method, path, rawBody string) (*http.Response, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, strings.NewReader(rawBody))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp, data
}

func (h *harness) createSession(model string) string {
	h.t.Helper()
	var body any
	if model != "" {
		body = map[string]string{"model": model}
	}
	return h.createSessionBody(body)
}

// createSessionBody posts an arbitrary POST /session body (e.g. to exercise
// workdir / share_workdir) and returns the new session's ID.
func (h *harness) createSessionBody(body any) string {
	h.t.Helper()
	resp, data := h.do("POST", "/session", body)
	if resp.StatusCode != 201 {
		h.t.Fatalf("create session status %d: %s", resp.StatusCode, data)
	}
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		h.t.Fatal(err)
	}
	return s.ID
}

// --- SSE client ---

type sseItem struct {
	id        string
	ev        Event
	heartbeat bool
}

type sseStream struct {
	items chan sseItem
	stop  func()
}

func (h *harness) openSSE(query string, lastEventID string) *sseStream {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", h.ts.URL+"/event"+query, nil)
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		cancel()
		h.t.Fatalf("sse status %d", resp.StatusCode)
	}
	items := make(chan sseItem, 128)
	go func() {
		defer close(items)
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		var cur sseItem
		have := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "":
				if have {
					select {
					case items <- cur:
					case <-ctx.Done():
						return
					}
					cur = sseItem{}
					have = false
				}
			case strings.HasPrefix(line, ":"):
				select {
				case items <- sseItem{heartbeat: true}:
				case <-ctx.Done():
					return
				}
			case strings.HasPrefix(line, "id: "):
				cur.id = line[len("id: "):]
				have = true
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(line[len("data: "):]), &cur.ev); err != nil {
					return
				}
				have = true
			}
		}
	}()
	s := &sseStream{items: items, stop: cancel}
	h.t.Cleanup(cancel)
	return s
}

// nextEvent returns the next non-heartbeat event.
func (s *sseStream) nextEvent(t *testing.T) Event {
	t.Helper()
	for it := range s.items {
		if it.heartbeat {
			continue
		}
		return it.ev
	}
	t.Fatal("sse stream closed before an event arrived")
	return Event{}
}

// waitFor returns the next event of the given type.
func (s *sseStream) waitFor(t *testing.T, typ string) Event {
	t.Helper()
	for {
		ev := s.nextEvent(t)
		if ev.Type == typ {
			return ev
		}
	}
}

// collectUntilIdle reads events until a session.status idle arrives, returning
// all events (including the idle one).
func (s *sseStream) collectUntilIdle(t *testing.T) []Event {
	t.Helper()
	var out []Event
	for {
		ev := s.nextEvent(t)
		out = append(out, ev)
		if ev.Type == "session.status" && ev.Status == "idle" {
			return out
		}
	}
}

// --- tests ---

func TestHealthNoAuth(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	req, _ := http.NewRequest("GET", h.ts.URL+"/health", nil) // no auth header
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	var body struct {
		Version string `json:"version"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Version != "9.9.9" {
		t.Errorf("version = %q", body.Version)
	}
}

// TestHealthReportsVCSInfo guards against the incident where an engineer
// burned 30 minutes suspecting a stale box binary because /health only ever
// echoed the config version (0.1.0-dev) with no way to tell which commit was
// actually running. /health must additionally surface vcs_revision and
// vcs_time from runtime/debug.ReadBuildInfo — present as empty strings
// (never omitted) when build info carries no VCS settings, e.g. a go test
// binary, which is exactly what this test runs as.
func TestHealthReportsVCSInfo(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	req, _ := http.NewRequest("GET", h.ts.URL+"/health", nil)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"vcs_revision", "vcs_time"} {
		raw, ok := body[key]
		if !ok {
			t.Fatalf("health response missing %q key entirely: %v", key, body)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("%s not a string: %v", key, err)
		}
		// A go test binary carries no VCS settings, so both are expected
		// empty here — the point of this test is that the KEYS are always
		// present, never omitted, so a client never has to special-case
		// "field absent" vs "field empty".
	}
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})

	// No token.
	req, _ := http.NewRequest("GET", h.ts.URL+"/session", nil)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}

	// Wrong token.
	req, _ = http.NewRequest("GET", h.ts.URL+"/session", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("wrong-token status = %d, want 401", resp.StatusCode)
	}

	// Right token.
	resp2, _ := h.do("GET", "/session", nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("good-token status = %d, want 200", resp2.StatusCode)
	}
}

func TestCORSPreflightUnauthenticated(t *testing.T) {
	h := newCORSHarness(t, "https://inspector.example")
	// Preflight to an authed route, carrying no Authorization header.
	req, _ := http.NewRequest("OPTIONS", h.ts.URL+"/session/abc/prompt_async", nil)
	req.Header.Set("Origin", "https://inspector.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://inspector.example" {
		t.Errorf("ACAO = %q, want origin echoed", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); got != "GET, POST, DELETE, OPTIONS" {
		t.Errorf("ACAM = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "Authorization, Content-Type, Last-Event-ID" {
		t.Errorf("ACAH = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Max-Age = %q, want 600", got)
	}
	if got := resp.Header.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

func TestCORSAllowedOnAuthedResponse(t *testing.T) {
	h := newCORSHarness(t, "https://inspector.example")
	resp, _ := h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://inspector.example" {
		t.Errorf("ACAO = %q, want origin echoed", got)
	}
	if got := resp.Header.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

func TestCORSWildcardEchoed(t *testing.T) {
	h := newCORSHarness(t, "*")
	resp, _ := h.do("GET", "/session", nil)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q, want *", got)
	}
}

func TestCORSHeaderOnUnauthorized(t *testing.T) {
	h := newCORSHarness(t, "https://inspector.example")
	// A real (non-preflight) request without credentials must still carry ACAO
	// so the browser can read the 401 body.
	req, _ := http.NewRequest("GET", h.ts.URL+"/session", nil)
	req.Header.Set("Origin", "https://inspector.example")
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://inspector.example" {
		t.Errorf("ACAO on 401 = %q, want origin echoed", got)
	}
}

func TestCORSDisabledByDefault(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	// No CORS headers on a normal response.
	resp, _ := h.do("GET", "/session", nil)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want empty when CORS disabled", got)
	}
	// OPTIONS gets no special handling (mux has no OPTIONS route → 405), and
	// certainly no CORS headers.
	req, _ := http.NewRequest("OPTIONS", h.ts.URL+"/session", nil)
	req.Header.Set("Origin", "https://inspector.example")
	resp2, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO on OPTIONS = %q, want empty when CORS disabled", got)
	}
}

func TestCreateListGetMessages(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})

	id := h.createSession("test/m1")

	// List includes it.
	resp, data := h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list status %d", resp.StatusCode)
	}
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0]["id"] != id {
		t.Fatalf("list = %s", data)
	}

	// Detail.
	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	var detail struct {
		ID       string `json:"id"`
		Model    string `json:"model"`
		Status   string `json:"status"`
		Messages int    `json:"messages"`
	}
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.ID != id || detail.Model != "test/m1" || detail.Status != "idle" || detail.Messages != 0 {
		t.Fatalf("detail = %s", data)
	}

	// Messages empty -> [] not null.
	resp, data = h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("messages status %d", resp.StatusCode)
	}
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("empty messages = %s", data)
	}

	// Unknown session -> 404.
	resp, _ = h.do("GET", "/session/ses_nope", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown status = %d, want 404", resp.StatusCode)
	}
}

func TestPromptAsyncFlow(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	var acc struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(data, &acc); err != nil {
		t.Fatal(err)
	}

	// The assistant message must arrive as a durable record with a seq.
	var gotUser, gotAsst bool
	for {
		ev := sse.nextEvent(t)
		if ev.Type == "message" && ev.Message != nil {
			switch ev.Message.Role {
			case message.RoleUser:
				gotUser = true
				if ev.Message.Parts.Text() != "hello" {
					t.Errorf("user text = %q", ev.Message.Parts.Text())
				}
			case message.RoleAssistant:
				gotAsst = true
				if ev.Seq == 0 {
					t.Errorf("assistant message missing seq")
				}
				if ev.Message.Parts.Text() != "done" {
					t.Errorf("assistant text = %q", ev.Message.Parts.Text())
				}
			}
		}
		if gotUser && gotAsst {
			break
		}
	}

	// Messages endpoint now returns both messages.
	_, data = h.do("GET", "/session/"+id+"/message", nil)
	var msgs []message.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
}

func TestReplayFromSeq(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one"), asstTurn("two")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	// First connection captures the full durable sequence of prompt A.
	s1 := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "a"}},
	})
	evs := s1.collectUntilIdle(t)
	s1.stop()

	// Find the busy event seq; replay from there must return exactly the
	// records after it (user, assistant, turn.end, idle), in ascending order.
	var busySeq int64
	for _, ev := range evs {
		if ev.Type == "session.status" && ev.Status == "busy" {
			busySeq = ev.Seq
		}
	}
	if busySeq == 0 {
		t.Fatal("no busy event captured")
	}

	s2 := h.openSSE("?from="+strconv.FormatInt(busySeq, 10), "")
	// Replay: everything with seq > busySeq up to now.
	var replaySeqs []int64
	var last int64
	idleSeen := false
	for !idleSeen {
		ev := s2.nextEvent(t)
		if ev.Seq <= busySeq {
			t.Fatalf("replayed event with seq %d <= from %d", ev.Seq, busySeq)
		}
		if ev.Seq <= last {
			t.Fatalf("replay not ascending: %d after %d", ev.Seq, last)
		}
		last = ev.Seq
		replaySeqs = append(replaySeqs, ev.Seq)
		if ev.Type == "session.status" && ev.Status == "idle" {
			idleSeen = true
		}
	}
	if len(replaySeqs) != 4 {
		t.Fatalf("replay count = %d, want 4 (user, assistant, turn.end, idle): %v", len(replaySeqs), replaySeqs)
	}

	// Now a live event: prompt B must stream through the same connection.
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "b"}},
	})
	live := s2.waitFor(t, "message")
	if live.Seq <= last {
		t.Fatalf("live event seq %d not greater than last replayed %d", live.Seq, last)
	}
}

func TestLastEventIDHeader(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	s1 := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "a"}},
	})
	evs := s1.collectUntilIdle(t)
	s1.stop()

	var busySeq int64
	for _, ev := range evs {
		if ev.Type == "session.status" && ev.Status == "busy" {
			busySeq = ev.Seq
		}
	}

	// Last-Event-ID header is equivalent to ?from=.
	s2 := h.openSSE("", strconv.FormatInt(busySeq, 10))
	ev := s2.nextEvent(t)
	if ev.Seq != busySeq+1 {
		t.Fatalf("first replayed seq = %d, want %d", ev.Seq, busySeq+1)
	}
}

func TestSessionFilter(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("a"), asstTurn("b")}}
	h := newHarness(t, prov)
	// Both sessions default to the same workdir (the server process cwd); this
	// test is about SSE filtering, not workdir exclusivity, so opt both into
	// share_workdir to keep the two prompts below independent of each other.
	idA := h.createSessionBody(map[string]any{"model": "test/m1", "share_workdir": true})
	idB := h.createSessionBody(map[string]any{"model": "test/m1", "share_workdir": true})

	sse := h.openSSE("?from=0&session="+idA, "")

	// Prompt B first, then A. The filter must drop all of B's records.
	h.do("POST", "/session/"+idB+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "b"}},
	})
	h.do("POST", "/session/"+idA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "a"}},
	})

	evs := sse.collectUntilIdle(t)
	for _, ev := range evs {
		if ev.SessionID != idA {
			t.Fatalf("filtered stream leaked session %s: %+v", ev.SessionID, ev)
		}
	}
}

func TestConcurrentPromptConflict(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	t.Cleanup(func() { close(prov.release) })

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != 409 {
		t.Fatalf("second prompt status %d, want 409: %s", resp.StatusCode, data)
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e); e.Error == "" {
		t.Errorf("409 body missing error: %s", data)
	}
}

func TestAbortCancels(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	t.Cleanup(func() { close(prov.release) })

	sse := h.openSSE("?from=0", "")

	resp, _ := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt status %d", resp.StatusCode)
	}

	// Busy status must be visible.
	sse.waitFor(t, "session.status") // busy
	// Wait for the provider to actually be streaming (blocked in Next).
	<-prov.started

	// Status endpoint reports busy.
	_, data := h.do("GET", "/session/status", nil)
	var st1 map[string]struct {
		Type string `json:"type"`
	}
	json.Unmarshal(data, &st1)
	if st1[id].Type != "busy" {
		t.Fatalf("status before abort = %q, want busy: %s", st1[id].Type, data)
	}

	// Abort is 204 and cancels the prompt.
	resp, _ = h.do("POST", "/session/"+id+"/abort", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("abort status %d, want 204", resp.StatusCode)
	}

	// A deliberate abort journals a durable session.aborted (no error field),
	// then the idle transition.
	aborted := sse.waitFor(t, "session.aborted")
	if aborted.Seq == 0 {
		t.Errorf("session.aborted missing seq")
	}
	if aborted.Error != "" {
		t.Errorf("session.aborted must not carry an error: %q", aborted.Error)
	}
	idle := sse.waitFor(t, "session.status")
	for idle.Status != "idle" {
		idle = sse.waitFor(t, "session.status")
	}

	_, data = h.do("GET", "/session/status", nil)
	var st2 map[string]struct {
		Type string `json:"type"`
	}
	json.Unmarshal(data, &st2)
	if st2[id].Type != "idle" {
		t.Fatalf("status after abort = %q, want idle: %s", st2[id].Type, data)
	}

	// session.aborted is replayable.
	replay := h.openSSE("?from=0&session="+id, "")
	if ev := replay.waitFor(t, "session.aborted"); ev.SessionID != id {
		t.Errorf("replayed abort session = %q", ev.SessionID)
	}
	replay.stop()

	// The session remains promptable (the slot was freed).
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "next"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("post-abort prompt status %d, want 202: %s", resp.StatusCode, data)
	}
}

func TestAbortUnknownSessionNotFound(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session/ses_nope/abort", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("abort unknown status = %d, want 404", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e); e.Error == "" {
		t.Errorf("404 body missing error: %s", data)
	}
}

func TestAbortIdleSessionIsIdempotent(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")
	resp, _ := h.do("POST", "/session/"+id+"/abort", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("abort idle status = %d, want 204", resp.StatusCode)
	}
}

func TestAbortColdSessionIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	// Seed a session on disk only (a prior process wrote its log).
	seedProv := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("hi")}}
	seed := engine.NewSession(engine.Config{
		Providers:  provider.Registry{"test": seedProv},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	if _, err := seed.Prompt(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	id := seed.ID

	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, _ := h.do("POST", "/session/"+id+"/abort", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("abort cold status = %d, want 204", resp.StatusCode)
	}

	// Nothing was running, so the abort must not have pulled the session
	// into memory — an existence check against the session log is enough.
	h.srv.mu.Lock()
	_, loaded := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if loaded {
		t.Errorf("abort on a cold session loaded it into memory")
	}
}

func TestColdSessionResumes(t *testing.T) {
	dir := t.TempDir()

	// Seed a session on disk with a separate engine + provider (simulating a
	// prior process). This writes <id>.jsonl but no events.jsonl.
	seedProv := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("cold-1")}}
	seed := engine.NewSession(engine.Config{
		Providers:  provider.Registry{"test": seedProv},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	if _, err := seed.Prompt(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	id := seed.ID

	// Fresh server over the same dir; the session is only on disk.
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("resumed")}}
	h := newHarnessDir(t, dir, prov)

	sse := h.openSSE("?from=0&session="+id, "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "again"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("cold prompt status %d: %s", resp.StatusCode, data)
	}

	// The resumed turn's assistant message streams through.
	var got bool
	for !got {
		ev := sse.nextEvent(t)
		if ev.Type == "message" && ev.Message != nil && ev.Message.Role == message.RoleAssistant &&
			ev.Message.Parts.Text() == "resumed" {
			got = true
		}
	}

	// History grew: seed(user,asst) + resume(user,asst) = 4.
	_, data = h.do("GET", "/session/"+id+"/message", nil)
	var msgs []message.Message
	json.Unmarshal(data, &msgs)
	if len(msgs) != 4 {
		t.Fatalf("resumed history = %d messages, want 4", len(msgs))
	}
}

func TestBootReconcileAppendsMissingMessages(t *testing.T) {
	dir := t.TempDir()

	// Seed a session log with messages but no events journal.
	seedProv := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("hi")}}
	seed := engine.NewSession(engine.Config{
		Providers:  provider.Registry{"test": seedProv},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	if _, err := seed.Prompt(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	id := seed.ID

	// Booting the server reconciles the journal from session logs.
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	// events.jsonl now holds message records for both seeded messages.
	data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var msgRecords int
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("bad journal line: %s", line)
		}
		if ev.Type == "message" && ev.SessionID == id {
			msgRecords++
			if ev.Seq == 0 {
				t.Errorf("reconciled message missing seq: %s", line)
			}
		}
	}
	if msgRecords != 2 {
		t.Fatalf("reconciled message records = %d, want 2", msgRecords)
	}

	// The detail endpoint reflects the reconciled seq.
	_, detail := h.do("GET", "/session/"+id, nil)
	var d struct {
		Seq      int64 `json:"seq"`
		Messages int   `json:"messages"`
	}
	json.Unmarshal(detail, &d)
	if d.Messages != 2 || d.Seq == 0 {
		t.Fatalf("detail after reconcile = %s", detail)
	}

	// Reconcile is idempotent: a second boot adds no new records.
	newHarnessDir(t, dir, &scriptedProvider{name: "test"})
	data2, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if !bytes.Equal(data, data2) {
		t.Fatalf("second reconcile changed the journal:\nbefore=%s\nafter=%s", data, data2)
	}
}

// chanWriter is an io.Writer that hands each write to a channel, so a test can
// observe stream output and synchronize on it without a data race.
type chanWriter struct{ ch chan string }

func (w chanWriter) Write(p []byte) (int, error) {
	w.ch <- string(p)
	return len(p), nil
}

// noopFlusher satisfies http.Flusher for direct s.stream calls.
type noopFlusher struct{}

func (noopFlusher) Flush() {}

// TestHeartbeat exercises the heartbeat ticker on fake time: inside a synctest
// bubble the fake clock advances only when every goroutine is durably blocked,
// so the ticker fires deterministically with no wall-clock wait. s.stream needs
// only an io.Writer and http.Flusher, so it is driven directly — no httptest
// server, no real ticker.
func TestHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s Server
		ctx, cancel := context.WithCancel(context.Background())
		writes := make(chan string) // unbuffered: each write blocks until read
		sub := &subscriber{ch: make(chan Event)}
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.stream(ctx, chanWriter{writes}, noopFlusher{}, sub, nil, time.Second)
		}()

		// With the test goroutine parked here and stream blocked in its select,
		// the fake clock advances a full second and fires the ticker; the first
		// (and only) write must be the heartbeat comment.
		if got := <-writes; got != ": heartbeat\n\n" {
			t.Fatalf("first stream write = %q, want heartbeat", got)
		}

		// End the stream before the bubble exits so fake time can stop and the
		// goroutine is not reported as a leak.
		cancel()
		<-done
	})
}

// errThenOKProvider fails its first Stream call and succeeds afterward. err,
// when set, is returned instead of the default "provider exploded" — used by
// turn.end sanitization tests to inject a credential-shaped error string.
type errThenOKProvider struct {
	name  string
	mu    sync.Mutex
	calls int
	err   error
}

func (p *errThenOKProvider) Name() string { return p.name }

func (p *errThenOKProvider) Stream(_ context.Context, _ *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		if p.err != nil {
			return nil, p.err
		}
		return nil, errors.New("provider exploded")
	}
	return &scriptedStream{events: asstTurn("recovered")}, nil
}

func TestSessionErrorOnPromptFailure(t *testing.T) {
	prov := &errThenOKProvider{name: "test"}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "boom"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	var acc struct {
		Seq int64 `json:"seq"`
	}
	json.Unmarshal(data, &acc)

	// session.error is durable (has a seq) and precedes the idle transition.
	errEv := sse.waitFor(t, "session.error")
	if errEv.Seq == 0 {
		t.Errorf("session.error missing seq")
	}
	if errEv.Error == "" {
		t.Errorf("session.error missing detail")
	}
	idle := sse.waitFor(t, "session.status")
	for idle.Status != "idle" {
		idle = sse.waitFor(t, "session.status")
	}
	if idle.Seq <= errEv.Seq {
		t.Errorf("idle seq %d not after error seq %d", idle.Seq, errEv.Seq)
	}

	// Replayable from the acknowledged seq.
	replay := h.openSSE("?from="+strconv.FormatInt(acc.Seq, 10), "")
	re := replay.waitFor(t, "session.error")
	if re.Error == "" {
		t.Errorf("replayed session.error missing detail")
	}
	replay.stop()

	// A subsequent prompt on the same session works (provider recovers).
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "again"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("recovery prompt status %d: %s", resp.StatusCode, data)
	}
	var got bool
	for !got {
		ev := sse.nextEvent(t)
		if ev.Type == "message" && ev.Message != nil && ev.Message.Parts.Text() == "recovered" {
			got = true
		}
	}
}

func TestFromWinsOverLastEventID(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	s1 := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "a"}},
	})
	evs := s1.collectUntilIdle(t)
	s1.stop()
	var maxSeq int64
	for _, ev := range evs {
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
	}

	// from=maxSeq (query) must win over a stale Last-Event-ID header of 0:
	// no records replay (all have seq <= maxSeq), only live events follow.
	s2 := h.openSSE("?from="+strconv.FormatInt(maxSeq, 10), "0")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "b"}},
	})
	// The very first event must be a fresh live record with seq > maxSeq; if
	// the header had won, we'd replay the seq-1 busy record from prompt A.
	first := s2.nextEvent(t)
	if first.Seq <= maxSeq {
		t.Fatalf("first event seq %d <= %d: header wrongly won over from", first.Seq, maxSeq)
	}
}

func TestModelOverridePersists(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("ok")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
		"model": "test/m2",
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	ev := sse.waitFor(t, "model")
	if ev.Model.String() != "test/m2" {
		t.Fatalf("model event = %q, want test/m2", ev.Model.String())
	}
	// Detail reflects the swapped model.
	_, detail := h.do("GET", "/session/"+id, nil)
	var d struct {
		Model string `json:"model"`
	}
	json.Unmarshal(detail, &d)
	if d.Model != "test/m2" {
		t.Fatalf("detail model = %q, want test/m2", d.Model)
	}
}

// TestListStatusErrorOnBadSessionDir verifies that a disk failure enumerating
// sessions surfaces as a 500, not an empty listing. SessionDir is repointed at
// a regular file so engine.ListSessions fails (ENOTDIR) — a real error a caller
// must not mistake for "no sessions".
func TestListStatusErrorOnBadSessionDir(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})

	bad := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.srv.opts.SessionDir = bad

	for _, path := range []string{"/session", "/session/status"} {
		resp, data := h.do("GET", path, nil)
		if resp.StatusCode != 500 {
			t.Fatalf("%s status = %d, want 500: %s", path, resp.StatusCode, data)
		}
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e); e.Error == "" {
			t.Errorf("%s 500 body missing error: %s", path, data)
		}
	}
}

// TestMaxResidentEvictsLongestIdle verifies that resident sessions are capped:
// with MaxResident=2, prompting three sessions unloads the longest-idle one
// from memory while it stays listable, status-reportable, and promptable from
// disk, with its journal records intact.
func TestMaxResidentEvictsLongestIdle(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("a"), asstTurn("b"), asstTurn("c"), asstTurn("reload"),
	}}
	h := newHarnessOpts(t, t.TempDir(), prov, 2)

	var ids []string
	for i := 0; i < 3; i++ {
		id := h.createSession("test/m1")
		ids = append(ids, id)
		sse := h.openSSE("?from=0&session="+id, "")
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "go"}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt %d status %d: %s", i, resp.StatusCode, data)
		}
		sse.collectUntilIdle(t) // wait for the prompt (and its eviction) to finish
		sse.stop()
	}

	// The longest-idle session (the first) is unloaded; the two newest remain.
	h.srv.mu.Lock()
	_, resident0 := h.srv.sessions[ids[0]]
	_, resident1 := h.srv.sessions[ids[1]]
	_, resident2 := h.srv.sessions[ids[2]]
	nResident := len(h.srv.sessions)
	h.srv.mu.Unlock()
	if resident0 {
		t.Errorf("oldest session %s still resident, want evicted", ids[0])
	}
	if !resident1 || !resident2 {
		t.Errorf("newer sessions evicted, want resident (%s=%v %s=%v)", ids[1], resident1, ids[2], resident2)
	}
	if nResident != 2 {
		t.Errorf("resident count = %d, want 2 (MaxResident)", nResident)
	}

	// The evicted session is still listed by /session (loaded from disk).
	_, data := h.do("GET", "/session", nil)
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %d sessions, want 3: %s", len(list), data)
	}

	// ... and by /session/status, as idle.
	_, data = h.do("GET", "/session/status", nil)
	var status map[string]struct {
		Type string `json:"type"`
	}
	json.Unmarshal(data, &status)
	if status[ids[0]].Type != "idle" {
		t.Fatalf("evicted session status = %q, want idle: %s", status[ids[0]].Type, data)
	}

	// Its journal records are unaffected: the messages endpoint reloads the two
	// messages (user + assistant) from disk without re-resident-ing it.
	_, data = h.do("GET", "/session/"+ids[0]+"/message", nil)
	var msgs []message.Message
	json.Unmarshal(data, &msgs)
	if len(msgs) != 2 {
		t.Fatalf("evicted session messages = %d, want 2: %s", len(msgs), data)
	}

	// It remains promptable via a transparent reload.
	sse := h.openSSE("?from=0&session="+ids[0], "")
	resp, data := h.do("POST", "/session/"+ids[0]+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "again"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("reload prompt status %d: %s", resp.StatusCode, data)
	}
	var got bool
	for !got {
		ev := sse.nextEvent(t)
		if ev.Type == "message" && ev.Message != nil && ev.Message.Role == message.RoleAssistant &&
			ev.Message.Parts.Text() == "reload" {
			got = true
		}
	}
	sse.stop()
}

// TestDrainAbortsInFlightPromptBeforeClose verifies shutdown drain: a prompt
// blocked mid-stream is cancelled by Drain when its deadline expires, the
// resulting session.aborted is journaled to the still-open file, and the file
// is not closed until Drain returns. Run inside a synctest bubble so the Drain
// deadline fires on fake time; handlers are driven directly (no real socket).
func TestDrainAbortsInFlightPromptBeforeClose(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := newBlockingProvider("test")
		t.Cleanup(func() { close(prov.release) })
		srv := newServer(t, dir, prov, 0)

		// Create a session directly through the handler.
		crec := httptest.NewRecorder()
		creq := httptest.NewRequest("POST", "/session", strings.NewReader(`{"model":"test/m1"}`))
		srv.handleCreate(crec, creq)
		if crec.Code != 201 {
			t.Fatalf("create status %d: %s", crec.Code, crec.Body)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(crec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		id := created.ID

		// Start a prompt; the blocking provider parks in Next.
		prec := httptest.NewRecorder()
		preq := httptest.NewRequest("POST", "/session/"+id+"/prompt_async",
			strings.NewReader(`{"parts":[{"type":"text","text":"go"}]}`))
		preq.SetPathValue("id", id)
		srv.handlePrompt(prec, preq)
		if prec.Code != 202 {
			t.Fatalf("prompt status %d: %s", prec.Code, prec.Body)
		}
		<-prov.started // the prompt is now blocked mid-stream

		// Drain with a short deadline: on fake time the deadline expires (the
		// prompt never completes on its own), Drain cancels it, and the
		// resulting session.aborted must be journaled before Drain returns.
		dctx, dcancel := context.WithTimeout(context.Background(), time.Second)
		defer dcancel()
		srv.Drain(dctx)

		// After Drain: the aborted record is in the journal and the file is
		// still open (Drain must precede Close).
		srv.mu.Lock()
		var aborted bool
		for _, ev := range srv.journal {
			if ev.Type == evtSessionAborted && ev.SessionID == id {
				aborted = true
			}
		}
		fileOpen := srv.jf != nil
		srv.mu.Unlock()
		if !aborted {
			t.Fatal("Drain returned without journaling session.aborted")
		}
		if !fileOpen {
			t.Fatal("journal file closed before Drain returned")
		}

		// The record reached the on-disk journal while the file was open.
		data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte(`"type":"`+evtSessionAborted+`"`)) {
			t.Fatalf("events.jsonl missing session.aborted:\n%s", data)
		}
		if err := srv.Close(); err != nil {
			t.Fatalf("Close after Drain: %v", err)
		}
	})
}

// TestStreamStopsOnClosing verifies that an SSE stream parked in its select
// returns promptly when the server's closing signal fires (drain start), even
// though its request context is never cancelled. Run inside a synctest bubble:
// the heartbeat ticker fires on fake time to prove the stream is idle in its
// loop, and the closing signal must then end it without any wall-clock wait.
func TestStreamStopsOnClosing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Server{closing: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sub := &subscriber{ch: make(chan Event)}
		writes := make(chan string) // unbuffered: heartbeat write blocks until read
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.stream(ctx, chanWriter{writes}, noopFlusher{}, sub, nil, time.Second)
		}()

		// The stream parks in its select; fake time advances to the heartbeat,
		// which it emits — proof it is looping idle with a live request context.
		if got := <-writes; got != ": heartbeat\n\n" {
			t.Fatalf("first write = %q, want heartbeat", got)
		}
		select {
		case <-done:
			t.Fatal("stream returned before the closing signal")
		default:
		}
		if ctx.Err() != nil {
			t.Fatal("precondition: request context must still be live")
		}

		// Drain begins: the closing signal must end the stream promptly, even
		// though its request context is never cancelled.
		close(s.closing)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("stream did not return after the closing signal (ctx still live)")
		}
		if ctx.Err() != nil {
			t.Fatal("request context should still be live after the stream returned")
		}
	})
}

// TestDrainClosesStreamsThenHonorsGraceBudget is the end-to-end shutdown-order
// test: a blocked prompt plus a connected SSE client. Drain's first act is to
// close the streams (so http.Server.Shutdown, run after Drain, sees idle
// connections), while the blocked prompt still gets the full grace budget
// before it is aborted. On fake time, the SSE stream must end at drain start —
// before the deadline, with the prompt still running — and Drain must not
// return (abort the prompt) until the grace budget has elapsed.
func TestDrainClosesStreamsThenHonorsGraceBudget(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := newBlockingProvider("test")
		t.Cleanup(func() { close(prov.release) })
		srv := newServer(t, dir, prov, 0)

		// Create a session and start a prompt that parks in the provider.
		crec := httptest.NewRecorder()
		creq := httptest.NewRequest("POST", "/session", strings.NewReader(`{"model":"test/m1"}`))
		srv.handleCreate(crec, creq)
		if crec.Code != 201 {
			t.Fatalf("create status %d: %s", crec.Code, crec.Body)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(crec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		id := created.ID

		prec := httptest.NewRecorder()
		preq := httptest.NewRequest("POST", "/session/"+id+"/prompt_async",
			strings.NewReader(`{"parts":[{"type":"text","text":"go"}]}`))
		preq.SetPathValue("id", id)
		srv.handlePrompt(prec, preq)
		if prec.Code != 202 {
			t.Fatalf("prompt status %d: %s", prec.Code, prec.Body)
		}
		<-prov.started // prompt is now blocked mid-stream

		// A connected SSE client, driven directly (no socket inside a bubble).
		sub := &subscriber{ch: make(chan Event, 8)}
		srv.mu.Lock()
		srv.subs[sub] = struct{}{}
		srv.mu.Unlock()
		sseCtx, sseCancel := context.WithCancel(context.Background())
		defer sseCancel()
		sseDone := make(chan struct{})
		var runningAtSSEEnd bool
		go func() {
			defer close(sseDone)
			// A large interval so the heartbeat never fires ahead of the drain
			// deadline; an unbuffered writer so a broken build blocks (not
			// spins) — the closing signal is what must end this stream.
			srv.stream(sseCtx, chanWriter{make(chan string)}, noopFlusher{}, sub, nil, time.Hour)
			srv.mu.Lock()
			if st := srv.sessions[id]; st != nil {
				runningAtSSEEnd = st.running
			}
			srv.mu.Unlock()
		}()
		synctest.Wait() // SSE client parks in its select

		// Shut down like serveCmd: Drain first (full grace budget for prompts).
		grace := time.Second
		drainStart := time.Now()
		dctx, dcancel := context.WithTimeout(context.Background(), grace)
		defer dcancel()
		drainDone := make(chan struct{})
		var drainReturn time.Time
		go func() {
			srv.Drain(dctx)
			drainReturn = time.Now()
			close(drainDone)
		}()

		// Drain's first act closes the SSE stream — at drain start, before the
		// grace deadline, with the prompt still running.
		synctest.Wait()
		select {
		case <-sseDone:
		default:
			t.Fatal("SSE stream did not end at drain start (closing signal)")
		}
		if sseCtx.Err() != nil {
			t.Fatal("SSE request context was cancelled; the stream must end via the closing signal, not ctx")
		}
		if !runningAtSSEEnd {
			t.Fatal("prompt was already aborted when the SSE stream ended; want SSE end at drain start, before the grace deadline")
		}

		<-drainDone
		if elapsed := drainReturn.Sub(drainStart); elapsed < grace {
			t.Fatalf("Drain returned after %v, want >= full grace budget %v (blocked prompt must get the whole budget)", elapsed, grace)
		}
		srv.mu.Lock()
		var aborted bool
		for _, ev := range srv.journal {
			if ev.Type == evtSessionAborted && ev.SessionID == id {
				aborted = true
			}
		}
		srv.mu.Unlock()
		if !aborted {
			t.Fatal("no session.aborted journaled after the grace expiry")
		}
	})
}

// createSessionDirect drives handleCreate without an HTTP listener (for
// synctest bubbles, where real sockets are unavailable) and returns the new id.
func createSessionDirect(t *testing.T, srv *Server, model string) string {
	t.Helper()
	body := `{}`
	if model != "" {
		body = `{"model":"` + model + `"}`
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/session", strings.NewReader(body))
	srv.handleCreate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body)
	}
	var c struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// TestPromptDuringDrainRejected verifies the admission gate: once Drain has set
// the draining flag, a new prompt_async is rejected with 503 and never spawns a
// runPrompt goroutine — the provider is never asked to stream and the journal
// gains no busy/message/idle record for that session. This is the data-loss
// guard: a 202 accepted during drain would acknowledge a prompt whose records
// could be lost on shutdown. Run in a synctest bubble; handlers driven directly.
func TestPromptDuringDrainRejected(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("nope")}}
		srv := newServer(t, dir, prov, 0)
		id := createSessionDirect(t, srv, "test/m1")

		// Drain with no in-flight prompt returns at once but leaves draining set.
		dctx, dcancel := context.WithCancel(context.Background())
		defer dcancel()
		srv.Drain(dctx)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/session/"+id+"/prompt_async",
			strings.NewReader(`{"parts":[{"type":"text","text":"go"}]}`))
		req.SetPathValue("id", id)
		srv.handlePrompt(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("prompt during drain status %d, want 503: %s", rec.Code, rec.Body)
		}
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Error == "" {
			t.Errorf("503 body missing error: %s", rec.Body)
		}

		// Let any erroneously-spawned goroutine run to completion on fake time.
		synctest.Wait()

		// The provider was never asked to stream: runPrompt never ran.
		prov.mu.Lock()
		calls := prov.call
		prov.mu.Unlock()
		if calls != 0 {
			t.Errorf("provider Stream called %d times; runPrompt spawned despite drain", calls)
		}
		// The journal holds only session.created for this session — no busy
		// status, no message, no idle.
		srv.mu.Lock()
		for _, ev := range srv.journal {
			if ev.SessionID == id && ev.Type != evtSessionCreated {
				t.Errorf("unexpected journal record after drained prompt: type=%s seq=%d", ev.Type, ev.Seq)
			}
		}
		srv.mu.Unlock()
	})
}

// TestMaxResidentEvictsOnCreate verifies that resident sessions are capped even
// when sessions are created but never prompted: with MaxResident=2, three
// creates leave only two resident, and all three remain listed (the evicted one
// reloads from its persisted-on-create disk log).
func TestMaxResidentEvictsOnCreate(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	dir := t.TempDir()
	h := newHarnessOpts(t, dir, prov, 2)

	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, h.createSession("test/m1"))
	}

	h.srv.mu.Lock()
	nResident := len(h.srv.sessions)
	h.srv.mu.Unlock()
	if nResident != 2 {
		t.Fatalf("resident count = %d, want 2 (MaxResident) after 3 creates with no prompts", nResident)
	}

	// All three are still listed (the evicted one loaded from disk).
	_, data := h.do("GET", "/session", nil)
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %d sessions, want 3: %s", len(list), data)
	}
	listed := map[string]bool{}
	for _, s := range list {
		if id, ok := s["id"].(string); ok {
			listed[id] = true
		}
	}
	for _, id := range ids {
		if !listed[id] {
			t.Errorf("session %s missing from list (evicted create-only session must persist to disk)", id)
		}
	}
}

// TestShutdownConcurrentDrainAndAdmissionGate is the end-to-end shutdown test,
// driven through a real http.Server + TCP listener (not a synctest bubble) with
// the actual serveCmd-style concurrent server.Shutdown+Drain. It sets up a
// connected SSE client and an in-flight (blocked) prompt, then shuts down under
// a grace budget and asserts:
//
//   - shutdown completes within a bounded time (it does not hang);
//   - the in-flight prompt's terminal record (session.aborted, since the
//     blocked prompt is cancelled at grace expiry) is journaled to the still-open
//     file BEFORE Close;
//   - a prompt_async attempted mid-drain (after the SSE stream has ended, which
//     proves draining is set) gets 503 or a connection error — never a
//     lost-record 202.
func TestShutdownConcurrentDrainAndAdmissionGate(t *testing.T) {
	dir := t.TempDir()
	prov := newBlockingProvider("test")
	t.Cleanup(prov.releaseAll)
	srv := newServer(t, dir, prov, 0)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := &http.Server{Handler: srv}
	go func() { _ = httpSrv.Serve(ln) }()
	base := "http://" + ln.Addr().String()
	const token = "secret-run-token"
	client := &http.Client{}
	t.Cleanup(client.CloseIdleConnections)

	req := func(method, path, body string) (*http.Response, []byte, error) {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		rq, err := http.NewRequest(method, base+path, r)
		if err != nil {
			return nil, nil, err
		}
		rq.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(rq)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp, data, nil
	}

	newSession := func() string {
		t.Helper()
		resp, data, err := req("POST", "/session", `{"model":"test/m1"}`)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 201 {
			t.Fatalf("create status %d: %s", resp.StatusCode, data)
		}
		var c struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}

	// Session A carries the blocked in-flight prompt; session B is idle and used
	// for the mid-drain admission attempt.
	idA := newSession()
	idB := newSession()

	resp, data, err := req("POST", "/session/"+idA+"/prompt_async", `{"parts":[{"type":"text","text":"go"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 202 {
		t.Fatalf("prompt A status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // prompt A is now blocked mid-stream

	// A connected SSE client. Its reader goroutine drains frames into a channel
	// and closes sseClosed when the server ends the stream (EOF).
	sseReq, _ := http.NewRequest("GET", base+"/event?from=0", nil)
	sseReq.Header.Set("Authorization", "Bearer "+token)
	sseResp, err := client.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	if sseResp.StatusCode != 200 {
		t.Fatalf("sse status %d", sseResp.StatusCode)
	}
	sseData := make(chan string, 64)
	sseClosed := make(chan struct{})
	go func() {
		defer close(sseClosed)
		defer sseResp.Body.Close()
		br := bufio.NewReader(sseResp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				select {
				case sseData <- line:
				default:
				}
			}
		}
	}()
	// Read at least one replayed frame to confirm the stream is live and the
	// subscriber is registered before shutdown begins.
	select {
	case <-sseData:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream produced no events; not live before shutdown")
	}

	// Shut down exactly like serveCmd: Shutdown and Drain run concurrently under
	// one grace budget.
	grace := 300 * time.Millisecond
	shutCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	start := time.Now()
	shutDone := make(chan error, 1)
	go func() { shutDone <- Shutdown(shutCtx, httpSrv, srv) }()

	// Drain's first act closes the SSE tail; the client's stream must end
	// promptly, well before the grace deadline (the blocked prompt keeps running).
	select {
	case <-sseClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream did not end after shutdown began (closing signal)")
	}

	// The SSE stream ending means Drain closed the closing signal, which it does
	// in the same critical section that sets draining=true. So a prompt now must
	// be refused: 503 (admission gate) or a connection error (listener closed) —
	// never a 202 whose record could be lost.
	dresp, ddata, derr := req("POST", "/session/"+idB+"/prompt_async", `{"parts":[{"type":"text","text":"late"}]}`)
	if derr == nil {
		if dresp.StatusCode == http.StatusAccepted {
			t.Fatalf("mid-drain prompt returned 202 (lost-record risk): %s", ddata)
		}
		if dresp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("mid-drain prompt status %d, want 503 or connection error: %s", dresp.StatusCode, ddata)
		}
	}

	// Shutdown returns within a bounded time (the blocked prompt is aborted at
	// grace expiry; the helper then returns) — it must not hang.
	select {
	case err := <-shutDone:
		// Shutdown returns nil normally; a context.DeadlineExceeded is tolerable
		// if a connection lingered to the deadline. Either way it returned.
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return; concurrent Drain+Shutdown hung")
	}
	if elapsed := time.Since(start); elapsed > grace+3*time.Second {
		t.Fatalf("shutdown took %v, far beyond grace budget %v", elapsed, grace)
	}

	// The in-flight prompt's terminal record is journaled (aborted at grace
	// expiry) and the file was still open when it landed (Shutdown waited for
	// Drain, and Close has not run yet).
	srv.mu.Lock()
	var terminal, fileOpen bool
	for _, ev := range srv.journal {
		if ev.SessionID == idA && (ev.Type == evtSessionAborted || ev.Type == evtMessage) {
			terminal = true
		}
	}
	fileOpen = srv.jf != nil
	srv.mu.Unlock()
	if !terminal {
		t.Fatal("in-flight prompt terminal record not journaled by shutdown")
	}
	if !fileOpen {
		t.Fatal("journal file closed before shutdown returned")
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(onDisk, []byte(`"type":"`+evtSessionAborted+`"`)) {
		t.Fatalf("events.jsonl missing terminal record before Close:\n%s", onDisk)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close after shutdown: %v", err)
	}
}

// TestClaimForPromptSurvivesEvictionRace guards FIX 2: with MaxResident=1, a
// running session must never be evicted when a second session appears, and the
// atomic claim must never leave two divergent *engine.Session objects for one
// log. The old getOrLoad-then-claim two-step had a window where a concurrent
// eviction could unload the session between resolve and claim, letting a racing
// cold-load insert a duplicate. claimForPrompt closes that window: resolve and
// claim happen under one lock, and a running session is skipped by eviction.
func TestClaimForPromptSurvivesEvictionRace(t *testing.T) {
	dir := t.TempDir()
	prov := newBlockingProvider("test")
	t.Cleanup(prov.releaseAll)
	h := newHarnessOpts(t, dir, prov, 1) // MaxResident=1: any 2nd resident evicts

	idA := h.createSession("test/m1")
	h.srv.mu.Lock()
	origSess := h.srv.sessions[idA].sess
	h.srv.mu.Unlock()

	// Watch A's stream so we can wait for it to reach idle later.
	sse := h.openSSE("?from=0&session="+idA, "")

	// Claim A for a prompt; claimForPrompt sets running before releasing the
	// lock, then the provider parks the prompt (A is now running).
	resp, data := h.do("POST", "/session/"+idA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt A status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	// Create B. Under MaxResident=1 this drives eviction, but running A must be
	// spared (evictResidentLocked skips running sessions); idle B is the victim.
	idB := h.createSession("test/m1")

	h.srv.mu.Lock()
	stA, aResident := h.srv.sessions[idA]
	h.srv.mu.Unlock()
	if !aResident {
		t.Fatal("running session A was evicted under MaxResident pressure")
	}
	if stA.sess != origSess {
		t.Fatal("session A engine identity changed while running (divergent duplicate created)")
	}

	// A concurrent prompt on the running A must be a clean 409, not a second
	// claim that could diverge state.
	resp, data = h.do("POST", "/session/"+idA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "again"}},
	})
	if resp.StatusCode != 409 {
		t.Fatalf("concurrent prompt on running A = %d, want 409: %s", resp.StatusCode, data)
	}
	_ = idB

	// Release; A completes and returns to idle.
	prov.releaseAll()
	sse.collectUntilIdle(t)
	sse.stop()

	// Re-prompt A and wait for it to fully complete (idle) so the log is stable:
	// it must resolve to one consistent state. Whether A is still resident or was
	// evicted after completing, the session the server prompts has a history
	// consistent with its on-disk log — there is no second, divergent session.
	sse2 := h.openSSE("?from=0&session="+idA, "")
	resp, data = h.do("POST", "/session/"+idA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "final"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("re-prompt A status %d: %s", resp.StatusCode, data)
	}
	sse2.collectUntilIdle(t)
	sse2.stop()

	// The messages endpoint (server's view) and a fresh independent load from
	// disk must agree on the message IDs — proof there is one consistent state.
	_, data = h.do("GET", "/session/"+idA+"/message", nil)
	var serverMsgs []message.Message
	if err := json.Unmarshal(data, &serverMsgs); err != nil {
		t.Fatal(err)
	}
	diskSess, err := engine.LoadSession(engine.Config{
		Providers:  provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	}, idA)
	if err != nil {
		t.Fatal(err)
	}
	diskMsgs := diskSess.History()
	if len(serverMsgs) != len(diskMsgs) {
		t.Fatalf("server history (%d msgs) diverges from disk (%d msgs)", len(serverMsgs), len(diskMsgs))
	}
	for i := range serverMsgs {
		if serverMsgs[i].ID != diskMsgs[i].ID {
			t.Fatalf("message %d id mismatch: server %q vs disk %q (divergent sessions)", i, serverMsgs[i].ID, diskMsgs[i].ID)
		}
	}
}
