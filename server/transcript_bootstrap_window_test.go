package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// newHarnessCountingLoadSession builds a harness identical to newHarnessDir,
// except every call through Options.LoadSession (engine.LoadSession's
// whole-journal replay, engine/store.go:1438) increments the returned
// counter. It is the seam this file's O(window) tests use to PROVE a
// windowed bootstrap never falls back to the full replay path, rather than
// merely observing that its answer happens to look right.
func newHarnessCountingLoadSession(t *testing.T, dir string, prov provider.Provider) (*harness, *int32) {
	t.Helper()
	const token = "secret-run-token"
	var count int32
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		orig := o.LoadSession
		o.LoadSession = func(id string) (*engine.Session, error) {
			atomic.AddInt32(&count, 1)
			return orig(id)
		}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}, &count
}

// TestColdWindowedBootstrap_LatestWindowNoFullReplay is this design's core
// claim (docs/design/fast-transcript-bootstrap.md §1, §3): stream_from=1
// combined with limit, against a session this process has never made
// resident, answers the LATEST window with the correct cursor triple and
// does so WITHOUT calling engine.LoadSession — the whole-journal read the
// design names as the 9.5s cost. Before this change, coldWindowedBootstrap
// does not exist and handleMessages rejects the combination outright (400),
// so this test fails red for two independent reasons pre-change: the
// request itself is rejected, and (were it not) LoadSession is the only
// path handleMessages has for a non-resident session.
func TestColdWindowedBootstrap_LatestWindowNoFullReplay(t *testing.T) {
	dir := t.TempDir()
	h, loadCount := newHarnessCountingLoadSession(t, dir, &scriptedProvider{name: "test"})
	// 20 turns = 40 messages: comfortably more than the requested window,
	// so "latest window" is a real subset, not the whole history by
	// accident.
	sess := coldMessages(t, dir, 20)

	const limit = 6
	resp, data := h.do("GET", "/session/"+sess.ID+"/message?stream_from=1&limit="+itoa(limit), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET stream_from+limit = %d: %s", resp.StatusCode, data)
	}
	var got transcriptResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}

	if n := atomic.LoadInt32(loadCount); n != 0 {
		t.Errorf("engine.LoadSession called %d times for a windowed bootstrap of a non-resident session, want 0 (O(window), not O(journal))", n)
	}

	if len(got.Messages) != limit {
		t.Fatalf("got %d messages, want %d (the latest window)", len(got.Messages), limit)
	}
	if len(got.Seqs) != limit {
		t.Fatalf("got %d seqs, want %d", len(got.Seqs), limit)
	}

	// Independent oracle: engine.ReadMessagePage's own before_seq/limit page
	// endpoint (handleMessagePage) already answers this exact tail read in
	// production. The windowed bootstrap's Messages must match it byte for
	// byte, and Seqs must match its FirstSeq..LastSeq range — proving
	// "latest window" against the ALREADY-TRUSTED mechanism, not against
	// this change's own implementation.
	pageResp, pageData := h.do("GET", "/session/"+sess.ID+"/message?before_seq=0&limit="+itoa(limit), nil)
	if pageResp.StatusCode != 200 {
		t.Fatalf("GET before_seq=0&limit oracle = %d: %s", pageResp.StatusCode, pageData)
	}
	var page pageResponse
	if err := json.Unmarshal(pageData, &page); err != nil {
		t.Fatalf("decode page oracle: %v (%s)", err, pageData)
	}
	if len(page.Messages) != limit {
		t.Fatalf("oracle page has %d messages, want %d", len(page.Messages), limit)
	}
	for i := range page.Messages {
		if got.Messages[i].ID != page.Messages[i].ID {
			t.Errorf("message[%d].ID = %s, want %s (oracle page)", i, got.Messages[i].ID, page.Messages[i].ID)
		}
		wantSeq := int64(page.FirstSeq + i)
		if got.Seqs[i] != wantSeq {
			t.Errorf("seqs[%d] = %d, want %d (oracle FirstSeq+%d)", i, got.Seqs[i], wantSeq, i)
		}
	}

	if got.StreamFrom <= 0 {
		t.Errorf("stream_from = %d, want > 0 for a non-empty window", got.StreamFrom)
	}
	if got.LiveFrom < got.StreamFrom {
		t.Errorf("live_from = %d, want >= stream_from %d", got.LiveFrom, got.StreamFrom)
	}
}

// TestColdWindowedBootstrap_ParityWithFullRead is the oracle the task brief
// asks for directly: for one untouched cold session, the windowed path
// (limit covering the whole history) and the existing full-read path must
// describe the identical instant — same Messages, same StreamFrom, same
// LiveFrom, same Seqs — because nothing journals for this session between
// the two calls. Calling the windowed path FIRST proves it alone already
// journals everything the full path would; calling the full (unwindowed)
// path SECOND as the oracle means its answer is untouched by this change
// (transcriptSyncedThrough is unmodified for the no-limit case).
func TestColdWindowedBootstrap_ParityWithFullRead(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})
	sess := coldMessages(t, dir, 3) // 6 messages

	windowed, wmeta := h.do("GET", "/session/"+sess.ID+"/message?stream_from=1&limit=100", nil)
	if windowed.StatusCode != 200 {
		t.Fatalf("GET windowed = %d: %s", windowed.StatusCode, wmeta)
	}
	var gotWindowed transcriptResponse
	if err := json.Unmarshal(wmeta, &gotWindowed); err != nil {
		t.Fatalf("decode windowed: %v (%s)", err, wmeta)
	}

	gotFull, fullMeta := getTranscript(t, h, sess.ID)
	if fullMeta.status != 200 {
		t.Fatalf("GET full = %d: %s", fullMeta.status, fullMeta.body)
	}

	if len(gotWindowed.Messages) != len(gotFull.Messages) {
		t.Fatalf("windowed has %d messages, full has %d, want equal (limit=100 covers all 6)", len(gotWindowed.Messages), len(gotFull.Messages))
	}
	for i := range gotFull.Messages {
		if gotWindowed.Messages[i].ID != gotFull.Messages[i].ID {
			t.Errorf("message[%d].ID = %s, want %s (full-read oracle)", i, gotWindowed.Messages[i].ID, gotFull.Messages[i].ID)
		}
	}
	if len(gotWindowed.Seqs) != len(gotFull.Seqs) {
		t.Fatalf("windowed has %d seqs, full has %d", len(gotWindowed.Seqs), len(gotFull.Seqs))
	}
	for i := range gotFull.Seqs {
		if gotWindowed.Seqs[i] != gotFull.Seqs[i] {
			t.Errorf("seqs[%d] = %d, want %d (full-read oracle)", i, gotWindowed.Seqs[i], gotFull.Seqs[i])
		}
	}
	if gotWindowed.StreamFrom != gotFull.StreamFrom {
		t.Errorf("stream_from = %d, want %d (full-read oracle)", gotWindowed.StreamFrom, gotFull.StreamFrom)
	}
	if gotWindowed.LiveFrom != gotFull.LiveFrom {
		t.Errorf("live_from = %d, want %d (full-read oracle)", gotWindowed.LiveFrom, gotFull.LiveFrom)
	}
}

// TestColdWindowedBootstrap_ResidencyRaceFallsBackConsistently is the
// regression test for §4.3 of the design doc: a concurrent prompt
// promoting id to resident strictly between coldWindowedBootstrap's
// engine.ReadMessagePage read and its second liveSessionObject recheck.
// The recheck must catch this and fall back to transcriptSyncedThrough —
// now cheap, since the session is resident — which answers from the
// session's CURRENT (post-race) history, so the response is consistent
// with a subsequent live read: no message the race added is missing, and
// nothing the stale windowed page already had is duplicated (the fallback
// discards the windowed page outright rather than merging it).
func TestColdWindowedBootstrap_ResidencyRaceFallsBackConsistently(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("raced")}})
	sess := coldMessages(t, dir, 3) // 6 messages, on disk, never touched by h

	var raceRan bool
	h.srv.coldWindowBootstrapRace = func() {
		raceRan = true
		resp, data := h.do("POST", "/session/"+sess.ID+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "go"}},
		})
		if resp.StatusCode != 202 {
			t.Errorf("coldWindowBootstrapRace: prompt_async = %d: %s", resp.StatusCode, data)
			return
		}
		h.waitIdle(sess.ID)
	}
	t.Cleanup(func() { h.srv.coldWindowBootstrapRace = nil })

	resp, data := h.do("GET", "/session/"+sess.ID+"/message?stream_from=1&limit=3", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET stream_from+limit = %d: %s", resp.StatusCode, data)
	}
	if !raceRan {
		t.Fatal("coldWindowBootstrapRace never ran")
	}
	var got transcriptResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}

	if h.srv.residentSession(sess.ID) == nil {
		t.Fatal("expected session to be resident after the race promoted it")
	}

	// The fallback answers from full current history: 6 original messages
	// (user+assistant per turn) plus the raced turn's own user+assistant
	// pair = 8, never the stale 3-message window the cold path had already
	// read.
	if len(got.Messages) != 8 {
		t.Fatalf("got %d messages, want 8 (fell back to the full, post-race history, not the stale window)", len(got.Messages))
	}

	seen := make(map[string]bool, len(got.Messages))
	for _, m := range got.Messages {
		if seen[m.ID] {
			t.Errorf("message %s appears twice in the response (overlap)", m.ID)
		}
		seen[m.ID] = true
	}

	// No gap vs. a subsequent live read: resuming GET /event from
	// live_from and running one more turn delivers exactly that turn's own
	// messages, nothing already covered by got.Messages and nothing
	// missing.
	sse := h.openSSE("?from="+itoa64(got.LiveFrom)+"&session="+sess.ID, "")
	resp2, data2 := h.do("POST", "/session/"+sess.ID+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go again"}},
	})
	if resp2.StatusCode != 202 {
		t.Fatalf("prompt_async (after) = %d: %s", resp2.StatusCode, data2)
	}
	h.waitIdle(sess.ID)

	evs := sse.collectUntilIdle(t)
	for _, ev := range evs {
		if ev.Type == evtMessage && ev.Message != nil && seen[ev.Message.ID] {
			t.Errorf("live read from live_from re-delivered message %s, already present in the bootstrap response (gap-safety violated)", ev.Message.ID)
		}
	}
}

// TestTranscriptStreamFrom_LimitAcceptedBeforeSeqStillRejected pins the
// exact query-grammar relaxation this design makes: stream_from+limit is
// now a legal, meaningful combination; stream_from+before_seq stays
// rejected, because pairing a cursor-establishing read with an explicit
// historical anchor is still two intentions on one request.
func TestTranscriptStreamFrom_LimitAcceptedBeforeSeqStillRejected(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("")

	resp, data := h.do("GET", "/session/"+id+"/message?stream_from=1&limit=5", nil)
	if resp.StatusCode != 200 {
		t.Errorf("GET stream_from=1&limit=5 = %d, want 200: %s", resp.StatusCode, data)
	}

	resp2, data2 := h.do("GET", "/session/"+id+"/message?stream_from=1&before_seq=5", nil)
	if resp2.StatusCode != 400 {
		t.Errorf("GET stream_from=1&before_seq=5 = %d, want 400: %s", resp2.StatusCode, data2)
	}

	resp3, data3 := h.do("GET", "/session/"+id+"/message?stream_from=1&before_seq=5&limit=5", nil)
	if resp3.StatusCode != 400 {
		t.Errorf("GET stream_from=1&before_seq=5&limit=5 = %d, want 400: %s", resp3.StatusCode, data3)
	}
}

// TestColdWindowedBootstrap_ManagedChildSession proves handlers.go's design
// §4.6: a child (task-tool) session gets no special-casing. It is stored,
// indexed, and paged through the identical SessionDir/SessionIndex/
// ReadMessagePage machinery as a root session, so the windowed bootstrap
// must serve it exactly the same way.
func TestColdWindowedBootstrap_ManagedChildSession(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})
	child := coldMessages(t, dir, 4) // 8 messages; stands in for a child's own log

	resp, data := h.do("GET", "/session/"+child.ID+"/message?stream_from=1&limit=3", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET stream_from+limit(child) = %d: %s", resp.StatusCode, data)
	}
	var got transcriptResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(got.Messages))
	}
	last := child.History()[len(child.History())-1]
	if got.Messages[len(got.Messages)-1].ID != last.ID {
		t.Errorf("last windowed message = %s, want %s (the child session's own newest message)", got.Messages[len(got.Messages)-1].ID, last.ID)
	}
}

// TestColdWindowedBootstrap_StaleIndexStillCorrect is §4.7: a missing or
// stale sidecar (engine/index.go's SessionIndex) triggers a transparent
// refold inside engine.ReadSessionIndex/ReadMessagePage — this design
// invents no new fallback for it. Deleting the sidecar file must not
// change the windowed bootstrap's answer at all, only (invisibly) its
// cost.
func TestColdWindowedBootstrap_StaleIndexStillCorrect(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})
	sess := coldMessages(t, dir, 3) // 6 messages

	// engine/index.go's sessionIndexSuffix, unexported: "<id>.index.json".
	idxPath := filepath.Join(dir, sess.ID+".index.json")
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("expected a sidecar index at %s (coldMessages should have flushed one on write): %v", idxPath, err)
	}
	if err := os.Remove(idxPath); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	resp, data := h.do("GET", "/session/"+sess.ID+"/message?stream_from=1&limit=4", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET stream_from+limit (no sidecar) = %d: %s", resp.StatusCode, data)
	}
	var got transcriptResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("got %d messages, want 4 (the sidecar's absence must only cost a refold, never change the answer)", len(got.Messages))
	}
	history := sess.History()
	want := history[len(history)-4:]
	for i := range want {
		if got.Messages[i].ID != want[i].ID {
			t.Errorf("message[%d].ID = %s, want %s", i, got.Messages[i].ID, want[i].ID)
		}
	}
}

// TestColdWindowedBootstrap_EmptySession is §4.8: a session with zero
// durable messages returns an empty window and stream_from=0, exactly the
// same deliberate zero-value transcriptWatermarkLocked's own doc comment
// specifies for the unwindowed path.
func TestColdWindowedBootstrap_EmptySession(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("") // created, never prompted: zero durable messages

	resp, data := h.do("GET", "/session/"+id+"/message?stream_from=1&limit=10", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET stream_from+limit (empty session) = %d: %s", resp.StatusCode, data)
	}
	var got transcriptResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("got %d messages, want 0", len(got.Messages))
	}
	if got.StreamFrom != 0 {
		t.Errorf("stream_from = %d, want 0 for an empty transcript", got.StreamFrom)
	}
}

// TestColdWindowedBootstrap_VerySmallSessionReturnsWholeHistory is §4.8's
// other half: a session with FEWER durable messages than the requested
// limit returns everything it has, not an error and not a short page
// padded with anything synthetic.
func TestColdWindowedBootstrap_VerySmallSessionReturnsWholeHistory(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})
	sess := coldMessages(t, dir, 1) // 2 messages

	resp, data := h.do("GET", "/session/"+sess.ID+"/message?stream_from=1&limit=100", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET stream_from+limit(small) = %d: %s", resp.StatusCode, data)
	}
	var got transcriptResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (the session's whole history, smaller than the requested limit)", len(got.Messages))
	}
}

// TestColdWindowedBootstrap_AfterCompaction is the design's §4.4 premise
// exercised against real production code: a cold session cannot be
// mid-compaction (compaction requires residency), so the only compaction
// state a windowed bootstrap can ever observe is one already fully landed
// before this call started. This builds a session, compacts it directly
// (engine.Session.Compact, bypassing the harness entirely, so the process
// answering the GET below has never touched it), and requests a window
// small enough that engine.ReadMessagePage's tailPage must give up on the
// compact record and fall back to foldedPage (engine/messagepage.go) —
// proving the windowed bootstrap is correct across that internal fallback,
// not merely in the common uncompacted case.
func TestColdWindowedBootstrap_AfterCompaction(t *testing.T) {
	dir := t.TempDir()
	seedProv := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("three", provider.Usage{InputTokens: 30}),
		// A fourth queued turn for Compact's own summarization call below:
		// seed uses this same provider registry for every call it makes,
		// unlike the harness-mediated compaction race tests elsewhere in
		// this package, which reload the session through a SEPARATE
		// provider registry (the harness's own).
		compactAsstTurn("summary", provider.Usage{InputTokens: 5}),
	}}
	seed := engine.NewSession(engine.Config{
		Providers:  provider.Registry{seedProv.name: seedProv},
		Model:      message.ModelRef{Provider: seedProv.name, Model: "m1"},
		SessionDir: dir,
		WorkDir:    dir,
	})
	for i, text := range []string{"go1", "go2", "go3"} {
		if _, err := seed.Prompt(context.Background(), text); err != nil {
			t.Fatalf("seed Prompt %d: %v", i, err)
		}
	}
	if _, err := seed.Compact(context.Background(), engine.CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := seed.PersistErr(); err != nil {
		t.Fatalf("seed PersistErr: %v", err)
	}

	// A fresh harness over the SAME dir: this process has never loaded or
	// journaled a byte of seed.ID.
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", "/session/"+seed.ID+"/message?stream_from=1&limit=2", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET stream_from+limit (post-compaction) = %d: %s", resp.StatusCode, data)
	}
	var got transcriptResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}

	// Independent oracle: the already-trusted before_seq/limit page for the
	// identical window.
	pageResp, pageData := h.do("GET", "/session/"+seed.ID+"/message?before_seq=0&limit=2", nil)
	if pageResp.StatusCode != 200 {
		t.Fatalf("GET before_seq=0&limit oracle (post-compaction) = %d: %s", pageResp.StatusCode, pageData)
	}
	var page pageResponse
	if err := json.Unmarshal(pageData, &page); err != nil {
		t.Fatalf("decode page oracle: %v (%s)", err, pageData)
	}
	if len(got.Messages) != len(page.Messages) {
		t.Fatalf("got %d messages, oracle has %d", len(got.Messages), len(page.Messages))
	}
	for i := range page.Messages {
		if got.Messages[i].ID != page.Messages[i].ID {
			t.Errorf("message[%d].ID = %s, want %s (oracle)", i, got.Messages[i].ID, page.Messages[i].ID)
		}
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
