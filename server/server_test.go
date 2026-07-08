package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
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
}

func newBlockingProvider(name string) *blockingProvider {
	return &blockingProvider{name: name, started: make(chan struct{}), release: make(chan struct{})}
}

func (p *blockingProvider) Name() string { return p.name }

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
	srv, err := New(Options{
		SessionDir:        dir,
		RunToken:          token,
		Version:           "9.9.9",
		HeartbeatInterval: 20 * time.Millisecond,
		NewSession: func(m message.ModelRef) (*engine.Session, error) {
			return engine.NewSession(mkCfg(m)), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(mkCfg(message.ModelRef{}), id)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
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

func (h *harness) createSession(model string) string {
	h.t.Helper()
	var body any
	if model != "" {
		body = map[string]string{"model": model}
	}
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
	// records after it (user, assistant, idle), in ascending order.
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
	if len(replaySeqs) != 3 {
		t.Fatalf("replay count = %d, want 3 (user, assistant, idle): %v", len(replaySeqs), replaySeqs)
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
	idA := h.createSession("test/m1")
	idB := h.createSession("test/m1")

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

func TestAbortUnknownSessionIsIdempotent(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, _ := h.do("POST", "/session/ses_nope/abort", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("abort unknown status = %d, want 204", resp.StatusCode)
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

func TestHeartbeat(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	sse := h.openSSE("?from=0", "")
	// Heartbeat interval is 20ms in tests; block until one arrives.
	for it := range sse.items {
		if it.heartbeat {
			return
		}
	}
	t.Fatal("no heartbeat received")
}

// errThenOKProvider fails its first Stream call and succeeds afterward.
type errThenOKProvider struct {
	name  string
	mu    sync.Mutex
	calls int
}

func (p *errThenOKProvider) Name() string { return p.name }

func (p *errThenOKProvider) Stream(_ context.Context, _ *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
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
