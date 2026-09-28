package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// TestCompactEndpointMarksContextUnknownUntilNextTurn is the red-first test
// for the corrected design: GET /session/{id}'s context.used_tokens must
// stop reporting the stale PRE-compaction reading right after a successful
// /compact, replacing it with a positive size ESTIMATE over the post-fold
// history — not the bare 0 an earlier fix reported, which rendered as an
// uninformative em dash on the console gauge — and the real measured value
// again once the next turn completes.
func TestCompactEndpointMarksContextUnknownUntilNextTurn(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("SUMMARY", provider.Usage{InputTokens: 5}),
		compactAsstTurn("three", provider.Usage{InputTokens: 30}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")
	if before := h.getSessionJSON(id).Context.UsedTokens; before != 20 {
		t.Fatalf("before compact context.used_tokens = %d, want 20", before)
	}

	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status %d: %s", resp.StatusCode, data)
	}

	if got := h.getSessionJSON(id).Context.UsedTokens; got <= 0 {
		t.Errorf("after compact context.used_tokens = %d, want a positive post-fold estimate, not 0", got)
	}

	h.promptAndWaitIdle(id, "go3")
	if got := h.getSessionJSON(id).Context.UsedTokens; got != 30 {
		t.Errorf("after the next turn context.used_tokens = %d, want 30", got)
	}
}

// TestCompactThenFailedTurnReportsTurnEndContextUsedTokensUnknown is the
// red-first test for recordTurnEnd's own share of the corrected design:
// after a successful /compact, a turn that ends without recording fresh
// usage (here, a provider failure) must still emit
// turn.end.context_used_tokens as a positive post-fold estimate, never the
// stale pre-compaction reading — mirroring the GET /session/{id} contract
// TestCompactEndpointMarksContextUnknownUntilNextTurn already pins for
// Session.context.
func TestCompactThenFailedTurnReportsTurnEndContextUsedTokensUnknown(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")

	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status %d: %s", resp.StatusCode, data)
	}

	// from=<seq after compact> so waitFor below sees go3's own turn.end, not
	// go1/go2's replayed turn.end records.
	sse := h.openSSE(fmt.Sprintf("?from=%d", h.getSessionJSON(id).Seq), "")
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go3"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}

	end := sse.waitFor(t, "turn.end")
	if end.Outcome != "error" {
		t.Fatalf("turn.end outcome = %q, want error (scripted provider is out of turns)", end.Outcome)
	}
	if end.ContextUsedTokens <= 0 {
		t.Errorf("turn.end context_used_tokens = %d, want a positive post-fold estimate after compact, not 0", end.ContextUsedTokens)
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

// TestCompactPanicDoesNotStrandSessionBusy is the regression test for the
// gap TestCompactPanicReleasesClaim's own doc comment deliberately leaves
// open: that test proves s.wg (Drain) recovers after a forced Compact
// panic, but says nothing about the SESSION itself. Before this fix,
// handleCompact called s.freeRunSlotAndEmitIdle and s.sessMgr.ReportTurnEnd
// as plain, non-deferred statements after st.sess.Compact — reached only on
// a normal return. net/http recovers a panicking handler per CONNECTION
// (net/http.(*conn).serve's own recover), not per PROCESS, so a panic
// inside Compact (or anything it calls, e.g. a native provider's
// transcoder choking on claude-code-produced history after an operator
// switches a delegated session's model mid-incident and then compacts) logs
// "http: panic serving ..." and closes that one connection, but the harness
// process stays up -- while this session's residency (st.running) and
// SessionManager node (status) are NEVER released, because the release
// statements were never reached. The session is left reporting status
// "busy", state "busy", and lineage.status "running" forever, with no
// runner process alive to ever finish it -- the exact shape of the live
// incident on session ses_01m1ht79e5fgfbx2cjx4cf4xm8.
//
// Red-verified: against the pre-fix handleCompact, this test times out
// waiting for lineage.status to leave "running" (waitForLineageStatus's own
// failure mode) after the forced panic.
func TestCompactPanicDoesNotStrandSessionBusy(t *testing.T) {
	prov := &panicAtCallProv{
		name: "test",
		turns: [][]provider.Event{
			compactAsstTurn("one", provider.Usage{InputTokens: 10}),
			compactAsstTurn("two", provider.Usage{InputTokens: 10}),
			compactAsstTurn("three", provider.Usage{InputTokens: 10}),
		},
		panicAt: 3, // the compaction summarization call, right after the 3 prompt turns above
	}
	// recoveryProv is a SEPARATE, healthy provider for the "run slot is
	// actually free" check at the end: panicAtCallProv panics on every call
	// once its own counter reaches panicAt (it never advances past the
	// panic), so re-prompting the SAME provider would panic again — this
	// time inside the async runPrompt goroutine handlePrompt spawns, which
	// nothing recovers, crashing the whole test binary rather than just
	// this one connection. A later prompt against a DIFFERENT provider
	// (mirroring an operator switching away after the failure, exactly
	// like the live incident's own model switch) proves the claim without
	// that trap.
	recoveryProv := &scriptedProvider{name: "recovery", turns: [][]provider.Event{asstTurn("still alive")}}
	model := message.ModelRef{Provider: prov.Name(), Model: "m1"}
	h := multiProviderHarness(t, model, nil, prov, recoveryProv)
	id := h.createSession("")
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
	// here, not a test failure. See TestCompactPanicReleasesClaim.
	if resp, err := h.ts.Client().Do(req); err == nil {
		resp.Body.Close()
	}

	// The process is still up (this HTTP call above returned/errored
	// instead of the whole test binary dying), so a plain, bounded poll
	// is enough to prove the session recovers -- or, before the fix,
	// times out here, which is the whole point of this regression test.
	lineage := waitForLineageStatus(t, h, id, "idle", 5*time.Second)
	if lineage["status"] != "idle" {
		t.Fatalf("lineage.status = %v after the forced compact panic, want idle", lineage["status"])
	}

	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var got struct {
		Status string `json:"status"`
		State  string `json:"state"`
		Queued int    `json:"queued"`
	}
	mustUnmarshal(t, data, &got)
	if got.Status != "idle" || got.State != "idle" {
		t.Errorf("after the forced compact panic, status=%q state=%q, want idle/idle", got.Status, got.State)
	}
	if got.Queued != 0 {
		t.Errorf("after the forced compact panic, queued = %d, want 0", got.Queued)
	}

	// A later, ordinary prompt on a DIFFERENT provider (see recoveryProv's
	// own doc comment above) must still be able to run -- proving the run
	// slot itself, not just its wire-visible status, is actually free.
	resp, data = h.do("POST", "/session/"+id+"/model", map[string]string{"model": "recovery/m1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set model status %d: %s", resp.StatusCode, data)
	}
	h.promptAndWaitIdle(id, "still alive")
	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var final struct {
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
	}
	mustUnmarshal(t, data, &final)
	if final.LastTurn == nil || final.LastTurn.Outcome != "completed" {
		t.Errorf("final last_turn = %+v, want outcome completed", final.LastTurn)
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

// TestCompactEndpointRejectsClaudeCodeDelegatedSession is the red-first test
// for guarding POST /session/{id}/compact against a session CURRENTLY
// delegated to the Claude Code CLI (engine.ClaudeCodeProviderFamily). That
// CLI manages its own context end to end (docs/design/context-compaction.md,
// "A session delegated to the Claude Code CLI"); harness's journal for such
// a session is only ever a passive record, so running harness's own
// summarizer against it would silently splice a journal nobody reads
// instead of doing anything the CLI's real context actually needs — the
// exact trap docs/design/context-compaction.md names. The endpoint must
// refuse with a clear 4xx naming the reason, before ever claiming the run
// slot or calling Session.Compact, rather than a 200 that accomplishes
// nothing or (worse) a 500 from a native-provider transcoder choking on
// claude-code-produced history.
func TestCompactEndpointDelegatesToClaudeCodeCLI(t *testing.T) {
	bin := buildFakeClaudeForServer(t)
	t.Setenv("FAKE_CLAUDE_MODE", "compact_turn")
	t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(t.TempDir(), "invocations.jsonl"))

	claudeModel := message.ModelRef{Provider: engine.ClaudeCodeProviderFamily, Model: "sonnet"}
	nativeProv := &scriptedProvider{name: "test"}
	h := claudeCodeSwitchHarness(t, claudeModel, engine.ClaudeCodeConfig{BinaryPath: bin}, nativeProv, 0)
	id := h.createSession("")
	sse := h.openSSE("", "")

	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact on a claude-code-delegated session status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out compactResponseJSON
	mustUnmarshal(t, data, &out)
	if !out.ClaudeCodeDelegated {
		t.Errorf("ClaudeCodeDelegated = false, want true")
	}
	if out.TurnsFolded != 0 || out.FirstID != "" || out.LastID != "" || out.Summary != nil {
		t.Errorf("delegated compact response carries native fold fields: %+v", out)
	}

	sse.waitFor(t, "compaction.started")
	sse.waitFor(t, "compaction.claude_code")
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

// TestCompactStartedAtWireEncodingDistinguishesAbsentFromSet: compact_
// started_at must omit its key entirely for a zero time (never encode
// the zero-time string, indistinguishable on the wire from a real start)
// and must round-trip a genuinely recorded one unchanged.
func TestCompactStartedAtWireEncodingDistinguishesAbsentFromSet(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name        string
		startedAt   time.Time
		wantPresent bool
	}{
		{"zero omits the key", time.Time{}, false},
		{"set round-trips", when, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(Event{Type: evtHistoryCompacted, SessionID: "ses_x", CompactStartedAt: tt.startedAt})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if present := bytes.Contains(data, []byte(`"compact_started_at"`)); present != tt.wantPresent {
				t.Errorf("marshaled event = %s, compact_started_at present = %v, want %v", data, present, tt.wantPresent)
			}
			var out Event
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !out.CompactStartedAt.Equal(tt.startedAt) {
				t.Errorf("CompactStartedAt round-tripped as %v, want %v", out.CompactStartedAt, tt.startedAt)
			}
		})
	}
}

// TestCompactEndpointHistoryCompactedCarriesCompactStartedAt: the durable
// history.compacted server event must carry when compaction began, not
// just when the summary settled — see docs/design/context-compaction.md
// §4. Without this, a fleet-wide question about compaction duration
// cannot be answered from the event stream alone.
func TestCompactEndpointHistoryCompactedCarriesCompactStartedAt(t *testing.T) {
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

	ev := sse.waitFor(t, "history.compacted")
	if ev.CompactStartedAt.IsZero() {
		t.Error("history.compacted CompactStartedAt is zero, want the instant the summarization call began")
	}
}

// TestCompactEndpointClaudeCodeCompactedCarriesCompactStartedAt mirrors the
// native-lane test above for the delegated compaction.claude_code event.
func TestCompactEndpointClaudeCodeCompactedCarriesCompactStartedAt(t *testing.T) {
	bin := buildFakeClaudeForServer(t)
	t.Setenv("FAKE_CLAUDE_MODE", "compact_turn")
	t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(t.TempDir(), "invocations.jsonl"))

	claudeModel := message.ModelRef{Provider: engine.ClaudeCodeProviderFamily, Model: "sonnet"}
	nativeProv := &scriptedProvider{name: "test"}
	h := claudeCodeSwitchHarness(t, claudeModel, engine.ClaudeCodeConfig{BinaryPath: bin}, nativeProv, 0)
	id := h.createSession("")
	sse := h.openSSE("", "")

	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact on a claude-code-delegated session status = %d, want 200: %s", resp.StatusCode, data)
	}

	sse.waitFor(t, "compaction.started")
	ev := sse.waitFor(t, "compaction.claude_code")
	if ev.CompactStartedAt.IsZero() {
		t.Error(`compaction.claude_code CompactStartedAt is zero, want the instant this stream observed the preceding "compacting" status`)
	}
}

// contextWindowHarness builds a server whose sessions run with an explicit
// engine.Config.ContextWindowTokens (mirroring requireWindowHarness's own
// NewSession override in server/context_window_required_test.go), so a test
// can assert the durable ContextWindowTokens field against a KNOWN value
// instead of "test/m1"'s unconfigured 0.
func contextWindowHarness(t *testing.T, prov provider.Provider, windowTokens int) *harness {
	t.Helper()
	const token = "secret-run-token"
	dir := t.TempDir()
	var srv *Server
	srv = newServer(t, dir, prov, 0, func(o *Options) {
		o.NewSession = func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
			if m.IsZero() {
				m = message.ModelRef{Provider: prov.Name(), Model: "m1"}
			}
			return engine.NewSession(engine.Config{
				Providers:           provider.Registry{prov.Name(): prov},
				Model:               m,
				SessionDir:          dir,
				WorkDir:             workDir,
				ParentSession:       parentSession,
				OnEvent:             func(ev engine.Event) { srv.Publish(ev) },
				ContextWindowTokens: windowTokens,
			}), nil
		}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

func TestCompactEndpointHistoryCompactedCarriesContextFields(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 10}),
		compactAsstTurn("gist", provider.Usage{InputTokens: 5}),
	}}
	const windowTokens = 1000
	h := contextWindowHarness(t, prov, windowTokens)
	id := h.createSession("test/m1")
	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status %d: %s", resp.StatusCode, data)
	}

	ev := sse.waitFor(t, "history.compacted")
	if ev.ContextUsedTokens <= 0 {
		t.Errorf("history.compacted context_used_tokens = %d, want a positive post-fold estimate", ev.ContextUsedTokens)
	}
	if ev.ContextWindowTokens != windowTokens {
		t.Errorf("history.compacted context_window_tokens = %d, want the session's configured window %d", ev.ContextWindowTokens, windowTokens)
	}
}

func TestCompactEndpointClaudeCodeCompactedCarriesContextFields(t *testing.T) {
	bin := buildFakeClaudeForServer(t)
	t.Setenv("FAKE_CLAUDE_MODE", "compact_turn")
	t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(t.TempDir(), "invocations.jsonl"))

	claudeModel := message.ModelRef{Provider: engine.ClaudeCodeProviderFamily, Model: "sonnet"}
	nativeProv := &scriptedProvider{name: "test"}
	const windowTokens = 1000
	h := claudeCodeSwitchHarness(t, claudeModel, engine.ClaudeCodeConfig{BinaryPath: bin}, nativeProv, windowTokens)
	id := h.createSession("")
	sse := h.openSSE("", "")

	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact on a claude-code-delegated session status = %d, want 200: %s", resp.StatusCode, data)
	}

	sse.waitFor(t, "compaction.started")
	ev := sse.waitFor(t, "compaction.claude_code")
	if ev.ContextUsedTokens != ev.PostTokens {
		t.Errorf("compaction.claude_code context_used_tokens = %d, want post_tokens %d", ev.ContextUsedTokens, ev.PostTokens)
	}
	if ev.ContextWindowTokens != windowTokens {
		t.Errorf("compaction.claude_code context_window_tokens = %d, want the session's configured window %d", ev.ContextWindowTokens, windowTokens)
	}
}
