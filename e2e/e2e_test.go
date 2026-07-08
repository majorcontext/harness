// Package e2e holds black-box ephemerality tests that exercise the real
// `harness serve` binary as a subprocess against a fake Anthropic Messages
// API. They prove the durability contract survives an abrupt SIGKILL: after a
// kill (mid-prompt, or idle, or with a truncated log tail) a fresh serve
// process on the same session directory reloads exactly the records that were
// durably complete pre-kill, replays its event journal with gap-free
// monotonic sequence numbers, and is immediately promptable again.
//
// These tests spawn real processes and issue real SIGKILLs — testing/synctest
// does not apply. Where they must wait for an out-of-process condition (a
// server becoming healthy, an async prompt finishing) they poll on a short
// interval bounded by a deadline; every such loop is documented at its site.
//
// The whole package is skipped under `go test -short` (it builds a binary and
// forks processes). CI runs the full suite, so it exercises there; keep total
// runtime well under ~30s.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// harnessBin is the path to the harness binary built once by TestMain.
var harnessBin string

// TestMain builds the real harness binary a single time for the whole package
// run. It skips the build under -short, where every test skips anyway. The
// testing flags are already registered by the generated test main, so a
// flag.Parse here makes testing.Short() meaningful before m.Run.
func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		bin, cleanup, err := buildHarness()
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e: building harness:", err)
			os.Exit(1)
		}
		harnessBin = bin
		code := m.Run()
		cleanup()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// buildHarness compiles ./cmd/harness into a temp dir and returns the binary
// path plus a cleanup func. It runs from the repo root (the parent of this
// package's directory).
func buildHarness() (string, func(), error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	root := filepath.Dir(wd) // <root>/e2e -> <root>
	dir, err := os.MkdirTemp("", "harness-e2e-bin")
	if err != nil {
		return "", nil, err
	}
	bin := filepath.Join(dir, "harness")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/harness")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("go build: %v\n%s", err, out)
	}
	return bin, func() { os.RemoveAll(dir) }, nil
}

const testToken = "e2e-run-token"

// --- fake Anthropic Messages API ---------------------------------------

// fakeAnthropic is an httptest server that speaks the Anthropic Messages API
// SSE wire format (shapes copied from provider/anthropic/stream_test.go). The
// request numbered stallOn streams one text delta, signals firstDelta, then
// blocks until the connection dies or the test unblocks it — so a SIGKILL can
// land mid-stream. Every other request returns a complete end_turn turn with a
// unique message id.
type fakeAnthropic struct {
	stallOn int32 // 1-based request number to stall on; 0 = never stall

	firstDelta chan struct{} // closed once the stalled request has flushed its first delta
	unblock    chan struct{} // closed by cleanup to release a stalled handler

	mu    sync.Mutex
	count int32
	fdOne sync.Once
}

func newFakeAnthropic(stallOn int32) *fakeAnthropic {
	return &fakeAnthropic{
		stallOn:    stallOn,
		firstDelta: make(chan struct{}),
		unblock:    make(chan struct{}),
	}
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.count++
	n := f.count
	f.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	if f.stallOn != 0 && n == f.stallOn {
		// Stream a partial turn — message_start, a text block open, one text
		// delta — then stall so a kill lands mid-write, mid-prompt. The turn
		// never reaches message_stop, so the assistant message is never
		// assembled or persisted.
		io.WriteString(w, sse("message_start", `{"type":"message_start","message":{"id":"msg_stalled","usage":{"input_tokens":5}}}`))
		io.WriteString(w, sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		io.WriteString(w, sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streaming"}}`))
		flusher.Flush()
		f.fdOne.Do(func() { close(f.firstDelta) })
		select {
		case <-r.Context().Done(): // client (killed subprocess) went away
		case <-f.unblock: // test cleanup
		}
		return
	}

	io.WriteString(w, completeTurn(fmt.Sprintf("msg_%d", n), "done"))
	flusher.Flush()
}

func (f *fakeAnthropic) close() { close(f.unblock) }

// sse formats one server-sent event (matches provider/anthropic/stream_test.go).
func sse(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

// completeTurn is a full end_turn SSE stream with a single text block.
func completeTurn(msgID, text string) string {
	return strings.Join([]string{
		sse("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"usage":{"input_tokens":5}}}`, msgID)),
		sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sse("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text)),
		sse("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`),
		sse("message_stop", `{"type":"message_stop"}`),
	}, "")
}

// --- serve subprocess management ---------------------------------------

// lockedBuffer is a concurrency-safe buffer for capturing a subprocess's
// stderr while the test also reads it on failure.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serveProc is a running `harness serve` subprocess.
type serveProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	addr   string
	stderr *lockedBuffer

	mu     sync.Mutex
	waited bool
}

// startServe launches `harness serve` on a free port with the given session
// dir and config, then waits (bounded) for /health. The process is killed at
// test cleanup.
func startServe(t *testing.T, sessDir, configPath string) *serveProc {
	t.Helper()
	addr := freeAddr(t)
	cmd := exec.Command(harnessBin, "serve", "-addr", addr)
	cmd.Dir = t.TempDir()
	cmd.Env = cleanEnv(map[string]string{
		"HARNESS_RUN_TOKEN":   testToken,
		"HARNESS_SESSION_DIR": sessDir,
		"HARNESS_CONFIG":      configPath,
		"ANTHROPIC_API_KEY":   "e2e-dummy-key",
	})
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting serve: %v", err)
	}
	p := &serveProc{t: t, cmd: cmd, addr: addr, stderr: stderr}
	t.Cleanup(p.kill)
	p.waitHealthy()
	return p
}

// kill terminates the process with SIGKILL and reaps it. Idempotent.
func (p *serveProc) kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waited {
		return
	}
	p.waited = true
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

// waitHealthy polls GET /health until 200 or a deadline. Real cross-process
// startup: poll on a short interval bounded by a deadline (synctest N/A).
func (p *serveProc) waitHealthy() {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + p.addr + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	p.t.Fatalf("serve did not become healthy on %s\nstderr:\n%s", p.addr, p.stderr.String())
}

// freeAddr returns a localhost address that was free a moment ago. There is a
// tiny window between closing the probe listener and the subprocess binding;
// acceptable for a local test harness.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// cleanEnv builds a child environment from the parent's, stripping any keys we
// set (and proxy vars) so the test's own shell can't leak a real API key,
// config, or proxy into the subprocess, then applying overrides.
func cleanEnv(overrides map[string]string) []string {
	strip := map[string]bool{
		"HARNESS_RUN_TOKEN": true, "HARNESS_SESSION_DIR": true,
		"HARNESS_CONFIG": true, "ANTHROPIC_API_KEY": true,
		"OPENAI_API_KEY": true, "HTTP_PROXY": true, "HTTPS_PROXY": true,
		"http_proxy": true, "https_proxy": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strip[k] {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// --- HTTP client helpers -----------------------------------------------

func (p *serveProc) do(method, path string, body any) (*http.Response, []byte) {
	p.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			p.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+p.addr+path, rdr)
	if err != nil {
		p.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// createSession creates a session and returns its id.
func (p *serveProc) createSession() string {
	p.t.Helper()
	resp, data := p.do(http.MethodPost, "/session", map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		p.t.Fatalf("create session: status %d body %s", resp.StatusCode, data)
	}
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		p.t.Fatalf("decode session: %v (%s)", err, data)
	}
	if s.ID == "" {
		p.t.Fatalf("empty session id: %s", data)
	}
	return s.ID
}

// prompt fires an async prompt (expects 202).
func (p *serveProc) prompt(id, text string) {
	p.t.Helper()
	body := map[string]any{"parts": []map[string]string{{"type": "text", "text": text}}}
	resp, data := p.do(http.MethodPost, "/session/"+id+"/prompt_async", body)
	if resp.StatusCode != http.StatusAccepted {
		p.t.Fatalf("prompt_async: status %d body %s", resp.StatusCode, data)
	}
}

type apiMessage struct {
	ID    string `json:"id"`
	Role  string `json:"role"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

func (p *serveProc) messages(id string) []apiMessage {
	p.t.Helper()
	resp, data := p.do(http.MethodGet, "/session/"+id+"/message", nil)
	if resp.StatusCode != http.StatusOK {
		p.t.Fatalf("get messages: status %d body %s", resp.StatusCode, data)
	}
	var msgs []apiMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		p.t.Fatalf("decode messages: %v (%s)", err, data)
	}
	return msgs
}

// listSessionIDs returns the ids reported by GET /session.
func (p *serveProc) listSessionIDs() []string {
	p.t.Helper()
	resp, data := p.do(http.MethodGet, "/session", nil)
	if resp.StatusCode != http.StatusOK {
		p.t.Fatalf("list sessions: status %d body %s", resp.StatusCode, data)
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		p.t.Fatalf("decode session list: %v (%s)", err, data)
	}
	ids := make([]string, len(list))
	for i, s := range list {
		ids[i] = s.ID
	}
	return ids
}

// waitMessages polls GET /session/{id}/message until it has want messages or a
// deadline. Real async prompt completion across a process boundary: poll on a
// short interval bounded by a deadline (synctest N/A).
func (p *serveProc) waitMessages(id string, want int) []apiMessage {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []apiMessage
	for time.Now().Before(deadline) {
		last = p.messages(id)
		if len(last) >= want {
			return last
		}
		time.Sleep(15 * time.Millisecond)
	}
	p.t.Fatalf("session %s: got %d messages, want %d\nstderr:\n%s", id, len(last), want, p.stderr.String())
	return nil
}

type apiEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
	Message   *struct {
		ID string `json:"id"`
	} `json:"message"`
}

// eventReplay connects to GET /event?from=0, reads the durable replay batch,
// and disconnects. /event is a long-lived stream (replay, then live, then
// heartbeats), so the read is bounded by a context deadline: the replay is
// written and flushed immediately on connect, and the subsequent block hits
// the deadline, at which point we return what we collected.
func (p *serveProc) eventReplay() []apiEvent {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.addr+"/event?from=0", nil)
	if err != nil {
		p.t.Fatalf("event request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatalf("GET /event: %v", err)
	}
	defer resp.Body.Close()
	var events []apiEvent
	dec := newSSEScanner(resp.Body)
	for {
		data, err := dec.next()
		if err != nil {
			break // deadline reached or stream ended: replay already consumed
		}
		var ev apiEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	return events
}

// sseScanner extracts the data payload of each SSE frame from a reader.
type sseScanner struct {
	r   *bufReader
	buf bytes.Buffer
}

func newSSEScanner(r io.Reader) *sseScanner { return &sseScanner{r: newBufReader(r)} }

func (s *sseScanner) next() ([]byte, error) {
	s.buf.Reset()
	got := false
	for {
		line, err := s.r.readLine()
		if err != nil {
			if got {
				return s.buf.Bytes(), nil
			}
			return nil, err
		}
		switch {
		case line == "":
			if got {
				return s.buf.Bytes(), nil
			}
		case strings.HasPrefix(line, "data:"):
			s.buf.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			got = true
		}
	}
}

// bufReader is a minimal line reader that respects the underlying reader's
// (context-driven) read errors so the bounded read in eventReplay terminates.
type bufReader struct {
	r   io.Reader
	buf []byte
}

func newBufReader(r io.Reader) *bufReader { return &bufReader{r: r} }

func (b *bufReader) readLine() (string, error) {
	for {
		if i := bytes.IndexByte(b.buf, '\n'); i >= 0 {
			line := string(bytes.TrimRight(b.buf[:i], "\r"))
			b.buf = b.buf[i+1:]
			return line, nil
		}
		tmp := make([]byte, 4096)
		n, err := b.r.Read(tmp)
		if n > 0 {
			b.buf = append(b.buf, tmp[:n]...)
			continue
		}
		if err != nil {
			return "", err
		}
	}
}

// --- config ------------------------------------------------------------

// writeConfig writes a harness config pointing the anthropic provider's
// base_url at the fake server, and returns its path.
func writeConfig(t *testing.T, baseURL string) string {
	t.Helper()
	cfg := map[string]any{
		"model": "anthropic/claude-fable-5",
		"providers": map[string]any{
			"anthropic": map[string]any{
				"api_key_env": "ANTHROPIC_API_KEY",
				"base_url":    baseURL,
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// --- shared assertions -------------------------------------------------

// assertContiguousSeqs asserts the durable events have strictly increasing,
// gap-free sequence numbers 1..max, and that no message id repeats (boot
// reconcile must not duplicate).
func assertContiguousSeqs(t *testing.T, events []apiEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no durable events replayed")
	}
	var prev int64
	msgIDs := map[string]bool{}
	for i, ev := range events {
		if ev.Seq <= prev {
			t.Fatalf("event %d seq %d not strictly increasing (prev %d)", i, ev.Seq, prev)
		}
		if ev.Seq != prev+1 {
			t.Fatalf("gap in seqs: event %d seq %d, prev %d", i, ev.Seq, prev)
		}
		prev = ev.Seq
		if ev.Type == "message" && ev.Message != nil {
			if msgIDs[ev.Message.ID] {
				t.Fatalf("duplicate message id in journal: %s", ev.Message.ID)
			}
			msgIDs[ev.Message.ID] = true
		}
	}
}

// assertUniqueMessageIDs asserts every message id is distinct.
func assertUniqueMessageIDs(t *testing.T, msgs []apiMessage) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.ID == "" {
			t.Fatalf("message with empty id: %+v", m)
		}
		if seen[m.ID] {
			t.Fatalf("duplicate message id: %s", m.ID)
		}
		seen[m.ID] = true
	}
}

func skipShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e: skipped under -short (builds a binary and forks real subprocesses)")
	}
}

// --- tests -------------------------------------------------------------

// TestKillMidPrompt is the core kill-9 case: SIGKILL a serve process while the
// provider is stalled mid-stream, then prove a fresh process on the same
// session dir recovers exactly the durably-complete records, replays a
// gap-free journal, reconciles without duplication, and is promptable again.
func TestKillMidPrompt(t *testing.T) {
	skipShort(t)

	fake := newFakeAnthropic(1) // stall the very first upstream request
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	t.Cleanup(fake.close)

	sessDir := t.TempDir()
	cfgPath := writeConfig(t, srv.URL)

	// Process 1: create a session and start a prompt that will stall upstream.
	p1 := startServe(t, sessDir, cfgPath)
	id := p1.createSession()
	p1.prompt(id, "first prompt that stalls mid-stream")

	// Wait until the fake has streamed the first delta, so the kill lands
	// mid-prompt, mid-write window.
	select {
	case <-fake.firstDelta:
	case <-time.After(10 * time.Second):
		t.Fatalf("provider never streamed first delta\nstderr:\n%s", p1.stderr.String())
	}

	// SIGKILL — no drain, no graceful shutdown.
	p1.kill()

	// Process 2: same session dir. Boot must reconcile the journal.
	p2 := startServe(t, sessDir, cfgPath)

	// The session is listable.
	ids := p2.listSessionIDs()
	if !contains(ids, id) {
		t.Fatalf("session %s not listed after restart: %v", id, ids)
	}

	// Only the durably-complete records survive: the user message (persisted at
	// prompt start) but not the stalled assistant message (never assembled).
	msgs := p2.messages(id)
	if len(msgs) != 1 {
		t.Fatalf("post-kill messages = %d, want 1 (user only): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Fatalf("surviving message role = %q, want user", msgs[0].Role)
	}
	assertUniqueMessageIDs(t, msgs)

	// The journal replays with strictly increasing, gap-free seqs and no
	// duplicated message ids from reconcile.
	events := p2.eventReplay()
	assertContiguousSeqs(t, events)
	if !hasSessionEvent(events, id) {
		t.Fatalf("no durable events for session %s: %+v", id, events)
	}

	// A new prompt succeeds end-to-end against the (now unblocked) fake.
	p2.prompt(id, "second prompt after recovery")
	// Expect user(pre-kill) + user(new) + assistant(new) = 3.
	final := p2.waitMessages(id, 3)
	assertUniqueMessageIDs(t, final)
	if got := final[len(final)-1]; got.Role != "assistant" || textOf(got) == "" {
		t.Fatalf("final message not a non-empty assistant reply: %+v", got)
	}

	// Journal still contiguous after the recovery prompt.
	assertContiguousSeqs(t, p2.eventReplay())
}

// TestKillIdle kills a serve process between prompts (idle) and asserts the
// message count survives exactly and the session remains promptable.
func TestKillIdle(t *testing.T) {
	skipShort(t)

	fake := newFakeAnthropic(0) // never stall
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	t.Cleanup(fake.close)

	sessDir := t.TempDir()
	cfgPath := writeConfig(t, srv.URL)

	p1 := startServe(t, sessDir, cfgPath)
	id := p1.createSession()
	p1.prompt(id, "hello")
	before := p1.waitMessages(id, 2) // user + assistant
	if len(before) != 2 {
		t.Fatalf("pre-kill messages = %d, want 2", len(before))
	}

	// Kill while idle (prompt already complete).
	p1.kill()

	p2 := startServe(t, sessDir, cfgPath)
	after := p2.messages(id)
	if len(after) != len(before) {
		t.Fatalf("message count changed across kill: before %d, after %d", len(before), len(after))
	}
	assertUniqueMessageIDs(t, after)
	assertContiguousSeqs(t, p2.eventReplay())

	// Still promptable.
	p2.prompt(id, "again")
	final := p2.waitMessages(id, 4) // + user + assistant
	assertUniqueMessageIDs(t, final)
}

// TestTruncatedFinalLine appends a garbage half-line (a crash-mid-write tail)
// to the session log before restart and asserts the server still boots and
// serves the session's intact records.
func TestTruncatedFinalLine(t *testing.T) {
	skipShort(t)

	fake := newFakeAnthropic(0)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	t.Cleanup(fake.close)

	sessDir := t.TempDir()
	cfgPath := writeConfig(t, srv.URL)

	p1 := startServe(t, sessDir, cfgPath)
	id := p1.createSession()
	p1.prompt(id, "hello")
	before := p1.waitMessages(id, 2)
	p1.kill()

	// Corrupt the tail: append a truncated, unterminated JSON fragment to the
	// session log (no trailing newline), simulating a crash mid-write.
	logPath := filepath.Join(sessDir, id+".jsonl")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open session log: %v", err)
	}
	if _, err := f.WriteString(`{"type":"message","message":{"id":"msg_trunc`); err != nil {
		t.Fatalf("append garbage: %v", err)
	}
	f.Close()

	// Server still boots and serves the intact records (truncated tail ignored).
	p2 := startServe(t, sessDir, cfgPath)
	if !contains(p2.listSessionIDs(), id) {
		t.Fatalf("session %s not listed after truncated-tail restart", id)
	}
	after := p2.messages(id)
	if len(after) != len(before) {
		t.Fatalf("truncated tail changed message count: before %d, after %d", len(before), len(after))
	}
	assertUniqueMessageIDs(t, after)
	assertContiguousSeqs(t, p2.eventReplay())

	// And still promptable.
	p2.prompt(id, "again")
	p2.waitMessages(id, 4)
}

// --- small helpers -----------------------------------------------------

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func hasSessionEvent(events []apiEvent, id string) bool {
	for _, ev := range events {
		if ev.SessionID == id {
			return true
		}
	}
	return false
}

func textOf(m apiMessage) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
