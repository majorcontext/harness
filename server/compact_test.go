package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// compactAsstTurn builds a scripted assistant reply carrying usage, with a
// fresh unique message ID per call (server package's shared asstTurn helper
// hardcodes a deterministic ID, which is fine for ordinary tests but
// collides across turns for compaction's ID-based splice/range assertions).
var compactTurnSeq int

func compactAsstTurn(text string, usage provider.Usage) []provider.Event {
	compactTurnSeq++
	msg := &message.Message{
		ID:    fmt.Sprintf("msg_asst_%d", compactTurnSeq),
		Role:  message.RoleAssistant,
		Parts: message.Parts{&message.Text{Text: text}},
	}
	return []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn, Usage: usage}}
}

// promptAndWaitIdle posts a synchronous-from-the-test's-point-of-view
// prompt_async (waits on GET /session/{id}/wait?until=idle before
// returning), so a test can build up turn history without manually
// polling SSE.
func (h *harness) promptAndWaitIdle(id, text string) {
	h.t.Helper()
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
	})
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	resp, data = h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("wait status %d: %s", resp.StatusCode, data)
	}
}

func (h *harness) getSessionJSON(id string) sessionJSON {
	h.t.Helper()
	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("get session status %d: %s", resp.StatusCode, data)
	}
	var sess sessionJSON
	if err := json.Unmarshal(data, &sess); err != nil {
		h.t.Fatalf("decode session: %v (%s)", err, data)
	}
	return sess
}

// TestCompactEndpointFoldsHistoryAndReportsResult is the red-first test for
// POST /session/{id}/compact's happy path: it folds the oldest turns,
// returns turns_folded/first_id/last_id/summary, and GET /session then
// shows compaction happened.
func TestCompactEndpointFoldsHistoryAndReportsResult(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("three", provider.Usage{InputTokens: 30}),
		compactAsstTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")
	h.promptAndWaitIdle(id, "go3")

	before := h.getSessionJSON(id)
	if before.CompactionCount != 0 {
		t.Fatalf("CompactionCount before compact = %d, want 0", before.CompactionCount)
	}

	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status %d: %s", resp.StatusCode, data)
	}
	var out compactResponseJSON
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode compact response: %v (%s)", err, data)
	}
	if out.TurnsFolded != 2 {
		t.Fatalf("turns_folded = %d, want 2", out.TurnsFolded)
	}
	if out.FirstID == "" || out.LastID == "" {
		t.Errorf("first_id/last_id empty: %+v", out)
	}
	if out.Summary == nil || out.Summary.Parts.Text() == "" {
		t.Fatalf("summary missing or empty: %+v", out)
	}

	after := h.getSessionJSON(id)
	if after.CompactionCount != 1 {
		t.Errorf("CompactionCount after compact = %d, want 1", after.CompactionCount)
	}
	if after.LastCompactedAt.IsZero() {
		t.Error("LastCompactedAt is zero after a successful compaction")
	}

	// The messages endpoint reflects the trimmed history: the summary
	// message, then the kept turn's user+assistant pair.
	_, data = h.do("GET", "/session/"+id+"/message", nil)
	var msgs []message.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages after compact = %d, want 3", len(msgs))
	}
	if msgs[0].ID != out.Summary.ID {
		t.Errorf("messages[0].ID = %q, want the summary id %q", msgs[0].ID, out.Summary.ID)
	}
}

// TestCompactEndpointKeepTurnsFloor is the red-first test for the hard
// floor on keep_turns: 0 or negative is a 400, never silently clamped.
func TestCompactEndpointKeepTurnsFloor(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	h.promptAndWaitIdle(id, "go1")

	for _, kt := range []int{0, -1, -5} {
		resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": kt})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("keep_turns=%d status = %d, want 400: %s", kt, resp.StatusCode, data)
		}
	}
}

// TestCompactEndpointNoopReturns200WithZeroTurnsFolded is the red-first test
// for §2's minimum-fold rule at the wire boundary: nothing worth folding is
// a 200 with turns_folded 0, never an error.
func TestCompactEndpointNoopReturns200WithZeroTurnsFolded(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	h.promptAndWaitIdle(id, "go1")

	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status %d: %s", resp.StatusCode, data)
	}
	var out compactResponseJSON
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.TurnsFolded != 0 {
		t.Errorf("turns_folded = %d, want 0 (only 1 turn exists, default keep_turns is 2)", out.TurnsFolded)
	}
	if out.SkipReason != "not_enough_turns" {
		t.Errorf("skip_reason = %q, want %q (review follow-up on PR #136, Finding C)", out.SkipReason, "not_enough_turns")
	}
}

// TestCompactEndpointReportsSkipReason is the red-first test for the review
// follow-up on PR #136, Finding C: POST /session/{id}/compact's response
// used to collapse three distinct turns_folded==0 situations (nothing to
// fold, a lone prior summary, and the summarizer running and returning
// empty) into the identical wire shape, hiding from an operator which one
// happened — only the last of those actually cost a billed provider call.
// skip_reason must distinguish them, and must be entirely absent
// (omitempty) on a real fold.
func TestCompactEndpointReportsSkipReason(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 10}),
		compactAsstTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")

	// A real fold must carry no skip_reason at all on the wire, not even an
	// empty string (omitempty).
	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status %d: %s", resp.StatusCode, data)
	}
	var out compactResponseJSON
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.TurnsFolded != 1 {
		t.Fatalf("turns_folded = %d, want 1", out.TurnsFolded)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["skip_reason"]; present {
		t.Errorf("skip_reason present in response for a real fold: %s (review follow-up on PR #136, Finding C)", data)
	}
}

// TestCompactEndpointBusySessionIs409 is the red-first test for the run-slot
// discipline (docs/design/context-compaction.md §4): a compaction request
// against an already-busy session is rejected with 409, exactly like
// prompt_async/goal.
func TestCompactEndpointBusySessionIs409(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hang"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/compact", map[string]any{})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("compact on busy session status = %d, want 409: %s", resp.StatusCode, data)
	}

	prov.releaseAll()
	h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
}

// panicAtCallProv serves scripted turns normally, then panics on the call at
// index panicAt — used to force a genuine panic mid-Compact (the tool-less
// compaction summarization call is always the call right after the ordinary
// prompt turns that built up foldable history).
type panicAtCallProv struct {
	name    string
	mu      sync.Mutex
	turns   [][]provider.Event
	call    int
	panicAt int
}

func (p *panicAtCallProv) Name() string { return p.name }

func (p *panicAtCallProv) Stream(_ context.Context, _ *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.call == p.panicAt {
		panic("forced panic for TestCompactPanicReleasesClaim")
	}
	ev := p.turns[p.call]
	p.call++
	return &scriptedStream{events: ev}, nil
}

// blockAtCallProv serves scripted turns normally, then blocks on the call
// at index blockAt until release closes — used to hold a real Compact
// call's summarization request open deterministically (the tool-less
// compaction summarization call is always the call right after the
// ordinary prompt turns that built up foldable history, mirroring
// panicAtCallProv's own convention above). started closes the instant
// that blocking call begins.
type blockAtCallProv struct {
	name     string
	mu       sync.Mutex
	turns    [][]provider.Event
	call     int
	blockAt  int
	started  chan struct{}
	release  chan struct{}
	startSet sync.Once
}

func (p *blockAtCallProv) Name() string { return p.name }

func (p *blockAtCallProv) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	n := p.call
	p.call++
	p.mu.Unlock()
	if n == p.blockAt {
		p.startSet.Do(func() { close(p.started) })
		return &blockAtCallStream{ctx: ctx, release: p.release}, nil
	}
	return &scriptedStream{events: p.turns[n]}, nil
}

type blockAtCallStream struct {
	ctx     context.Context
	release chan struct{}
	// done tracks whether the single EventDone has already been
	// returned. The compaction summarizer's own Next loop
	// (runCompactionSummary, engine/compact.go) calls Next in a loop
	// until it sees io.EOF — it does NOT stop merely because it received
	// an EventDone, unlike the ordinary turn loop other blocking test
	// streams in this package are built for. Without this flag, a
	// second call after release closes would take the SAME <-s.release
	// case again (a closed channel always receives immediately) and
	// return the identical EventDone forever — an infinite loop, never
	// reaching io.EOF, hanging the whole request. A live test run caught
	// this exact hang.
	done bool
}

func (s *blockAtCallStream) Next() (provider.Event, error) {
	if s.done {
		return provider.Event{}, io.EOF
	}
	select {
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	case <-s.release:
		s.done = true
		msg := &message.Message{ID: "msg_released", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "released"}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	}
}

func (s *blockAtCallStream) Close() error { return nil }

// TestCompactBracketsRunSlotWithSessionManager is the regression test for
// a review finding: handleCompact claimed the server's own run slot
// (claimForPrompt) but never reported it to SessionManager at all —
// unlike runPrompt/runGoal's identical ReportTurnStart/ReportTurnEnd
// bracket. triggerResumeLocked flips a root to StatusRunning BEFORE
// calling its ExternalRunner, and runOrQueueText treats a workdir-held
// or draining refusal as requiring a revert of that commitment — but a
// task notification arriving while a compact call held the REAL slot,
// with SessionManager never told about it at all, would see the root
// StatusIdle (compact's claim was invisible to SessionManager) and try
// to resume it directly, racing compact's own Session.Compact call on
// the same session. Proves the bracket is in place: SessionManager's own
// view of the root is StatusRunning for the WHOLE duration of a compact
// call, exactly like an ordinary prompt turn.
func TestCompactBracketsRunSlotWithSessionManager(t *testing.T) {
	prov := &blockAtCallProv{
		name: "test",
		turns: [][]provider.Event{
			compactAsstTurn("one", provider.Usage{InputTokens: 10}),
			compactAsstTurn("two", provider.Usage{InputTokens: 10}),
			compactAsstTurn("three", provider.Usage{InputTokens: 10}),
		},
		blockAt: 3, // the compaction summarization call
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	// Released via a plain defer, NOT t.Cleanup: newHarness's own
	// t.Cleanup(ts.Close) — registered AFTER this one would be — blocks
	// until every outstanding request on the test server completes, and
	// Cleanup funcs run in LIFO order. A t.Cleanup registered here would
	// run AFTER ts.Close already started waiting on this test's own
	// still-blocked compact request — deadlock. A plain defer, in
	// contrast, always runs at THIS function's own return (including via
	// t.Fatal's runtime.Goexit unwind), strictly before ts.Close's own
	// later t.Cleanup — releasing the request before ts.Close ever waits
	// on it, on every exit path, not just the successful one.
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(prov.release) })
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")
	h.promptAndWaitIdle(id, "go3")

	// SessionManager adopts a root the first time ReportTurnStart sees it
	// (or handleCreate's own AdoptRoot) — confirm the baseline is idle
	// before compact claims the slot.
	if info, ok := h.srv.SessionManager().Info(id); !ok || info.Status != engine.StatusIdle {
		t.Fatalf("test setup: SessionManager view before compact = %+v ok=%v, want tracked and idle", info, ok)
	}

	compactDone := make(chan struct{})
	go func() {
		defer close(compactDone)
		resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 1})
		t.Logf("compact response: status=%d body=%s", resp.StatusCode, data)
	}()
	select {
	case <-prov.started:
	case <-time.After(2 * time.Second):
		t.Fatal("compact's summarization call never started")
	}

	if info, ok := h.srv.SessionManager().Info(id); !ok || info.Status != engine.StatusRunning {
		t.Fatalf("SessionManager view while compact is in flight = %+v ok=%v, want tracked and StatusRunning — the bracket is missing", info, ok)
	}

	releaseOnce.Do(func() { close(prov.release) })
	select {
	case <-compactDone:
	case <-time.After(2 * time.Second):
		t.Fatal("compact never completed after being released")
	}
}

// TestCompactPanicReleasesClaim is the regression test for handleCompact's
// panic-unsafe wg.Done: a plain `s.wg.Done()` at the handler's tail is never
// reached if Compact (or either of the tail's own maybeDispatchQueued/
// maybeAutoArmGoal calls) panics, leaking the earlier wg.Add and hanging
// Drain forever. A `defer s.wg.Done()` registered right after the claim
// succeeds runs during the panic's unwind — same ordering as the normal
// path (defers still run after the body's tail calls), but panic-safe.
//
// net/http recovers a panicking handler per-connection (closing that
// connection, logging "http: panic serving ..."), so the client observes a
// broken connection rather than a stack trace — this test only cares that
// the server's own claim bookkeeping survives: Drain must complete promptly
// afterward, proving s.wg returned to zero rather than staying stuck above
// zero forever.
func TestCompactPanicReleasesClaim(t *testing.T) {
	prov := &panicAtCallProv{
		name: "test",
		turns: [][]provider.Event{
			compactAsstTurn("one", provider.Usage{InputTokens: 10}),
			compactAsstTurn("two", provider.Usage{InputTokens: 10}),
			compactAsstTurn("three", provider.Usage{InputTokens: 10}),
		},
		panicAt: 3, // the compaction summarization call, right after the 3 prompt turns above
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")
	h.promptAndWaitIdle(id, "go3")

	req, err := http.NewRequest("POST", h.ts.URL+"/session/"+id+"/compact",
		bytes.NewReader([]byte(`{"keep_turns":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	// Deliberately not h.do: the forced panic aborts net/http's connection
	// mid-response, so the client call errors -- that is the expected shape
	// here, not a test failure.
	if resp, err := h.ts.Client().Do(req); err == nil {
		resp.Body.Close()
	}

	drainDone := make(chan struct{})
	go func() {
		h.srv.Drain(context.Background())
		close(drainDone)
	}()
	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not complete after the forced compact panic -- the run-slot claim leaked")
	}
}

// TestCompactEndpointUnknownSessionIs404 mirrors prompt_async/goal's
// unknown-session handling.
func TestCompactEndpointUnknownSessionIs404(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session/ses_nope/compact", map[string]any{})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, data)
	}
}

// TestCompactEndpointRequiresAuth mirrors every other write endpoint's
// run-token auth requirement.
func TestCompactEndpointRequiresAuth(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	req, err := http.NewRequest("POST", h.ts.URL+"/session/"+id+"/compact", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without auth = %d, want 401", resp.StatusCode)
	}
}

// TestCompactEndpointSummaryEventBeforeHistoryCompactedEvent is the
// red-first test for §4's live event surface at the server boundary: an SSE
// tailer sees the summary's "message" event strictly before the durable
// "history.compacted" event.
func TestCompactEndpointSummaryEventBeforeHistoryCompactedEvent(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 10}),
		compactAsstTurn("gist", provider.Usage{InputTokens: 5}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status %d: %s", resp.StatusCode, data)
	}
	var out compactResponseJSON
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}

	var sawSummaryMessage, sawCompacted bool
	for !sawCompacted {
		ev := sse.nextEvent(t)
		switch ev.Type {
		case "message":
			if ev.Message != nil && ev.Message.ID == out.Summary.ID {
				sawSummaryMessage = true
			}
		case "history.compacted":
			if !sawSummaryMessage {
				t.Fatal("history.compacted event arrived before the summary's message event")
			}
			sawCompacted = true
			if ev.CompactTurnsFolded != out.TurnsFolded || ev.CompactSummaryID != out.Summary.ID {
				t.Errorf("history.compacted event = %+v, want it to carry the compact result", ev)
			}
		}
	}
	if !sawSummaryMessage {
		t.Fatal("never saw the summary's message event")
	}
}
