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
//
// The fallback still honors the caller's own limit (windowTranscriptTail,
// handlers.go): the response is the TAIL of the post-race history, not
// the whole 8 messages a plain fallback-ignores-limit read would have
// returned — proving windowing survives the fallback path too, not only
// coldWindowedBootstrap's own success path.
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

	// The fallback answers from the CURRENT (post-race) history — 6
	// original messages plus the raced turn's own user+assistant pair = 8
	// — windowed to the same limit=3 the request named, never the whole
	// 8 and never the stale 3-message window the cold path had already
	// read (a different 3: the race added 2 new messages, shifting the
	// tail).
	if len(got.Messages) != 3 {
		t.Fatalf("got %d messages, want 3 (limit=3, honored on the fallback path too, against the post-race history)", len(got.Messages))
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

// TestColdWindowedBootstrap_StreamFromParityAfterSeededJournal is the
// regression test for a correctness bug an Opus review of PR #265 found:
// coldWindowedBootstrap fed its tail window straight to
// transcriptWatermarkLocked, which scans s.journal -- the SERVER's own
// in-memory event log, process-wide and cumulative, independent of
// residency -- for a compaction summary absent from the passed-in
// history and, on finding one, capped the returned watermark toward it.
// That cap exists for a summary excluded by a LIVE compaction race (see
// transcriptWatermarkLocked's own doc comment); it is a false positive
// for a summary simply older than a bounded window, which is the only
// way a windowed, already-non-resident read can ever exclude one
// (docs/design/fast-transcript-bootstrap.md §4.4).
//
// The bug requires s.journal to ALREADY hold this session's compaction
// summary before the windowed call runs: lookupSession's LoadSession
// branch never registers a session as resident, so a plain GET
// stream_from=1 (no limit) journals the summary via
// transcriptCursorLocked and leaves the session just as cold as before --
// exactly what step 1 below does, mirroring the reviewer's repro. The
// harness is built FIRST, against an EMPTY dir (so its boot-time
// reconcile(), which also fully replays and journals every session
// already on disk, finds nothing -- the same setup
// TestTranscriptStreamFrom_ConsistentWithSnapshot uses for the identical
// reason) -- the seed session is written to that same dir only
// afterward, out-of-process. Without the explicit step-1 read below,
// the bug cannot manifest: a virgin session has nothing in s.journal
// yet, so the cap never engages regardless of windowing, which is why
// TestColdWindowedBootstrap_AfterCompaction (added earlier in this
// file, before this bug was found) never caught it.
func TestColdWindowedBootstrap_StreamFromParityAfterSeededJournal(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	seedProv := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("three", provider.Usage{InputTokens: 30}),
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

	// Step 1: ONE full stream_from=1 read seeds s.journal with the
	// compaction summary (and the two kept messages), and leaves the
	// session cold (see the doc comment above).
	full, fullMeta := getTranscript(t, h, seed.ID)
	if fullMeta.status != 200 {
		t.Fatalf("GET full (seed s.journal) = %d: %s", fullMeta.status, fullMeta.body)
	}
	if h.srv.residentSession(seed.ID) != nil {
		t.Fatal("seed.ID became resident from a plain GET -- test setup invariant broken")
	}

	// Step 2: a windowed read whose 2-message tail excludes the
	// compaction summary (post-compaction history is exactly 3 messages:
	// summary, kept-user, kept-assistant).
	resp, data := h.do("GET", "/session/"+seed.ID+"/message?stream_from=1&limit=2", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET windowed (post-seed) = %d: %s", resp.StatusCode, data)
	}
	var windowed transcriptResponse
	if err := json.Unmarshal(data, &windowed); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}

	if windowed.StreamFrom != full.StreamFrom {
		t.Errorf("windowed stream_from = %d, want %d (parity with the full path already established for this session -- a compaction summary OLDER than the window must never cap it)", windowed.StreamFrom, full.StreamFrom)
	}
	if windowed.LiveFrom != full.LiveFrom {
		t.Errorf("windowed live_from = %d, want %d (parity with the full path)", windowed.LiveFrom, full.LiveFrom)
	}
}

// TestColdWindowedBootstrap_ParityWithFullRead_CompactedPartialWindow
// extends TestColdWindowedBootstrap_ParityWithFullRead's parity oracle to
// a compacted session with a window SMALLER than the total history (that
// test's own limit=100 always covered everything, so it could never
// exercise a compaction summary sitting outside the window at all). Per
// docs/design/fast-transcript-bootstrap.md §4.1, stream_from/live_from
// must match the full path exactly whenever the window reaches the
// session's newest message -- which a "newest page" window always does --
// regardless of how small the window is or how much older history (a
// compaction summary included) it excludes.
//
// Unlike TestColdWindowedBootstrap_StreamFromParityAfterSeededJournal,
// this test writes the seed session to disk BEFORE the harness boots, so
// Server.reconcile's own startup replay (server/journal.go) journals the
// compaction summary into s.journal before either read below runs --
// s.journal is seeded here too, just by a different, equally realistic
// path (an already-populated SessionDir at process start) than the other
// test's explicit prior read.
func TestColdWindowedBootstrap_ParityWithFullRead_CompactedPartialWindow(t *testing.T) {
	dir := t.TempDir()
	seedProv := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("three", provider.Usage{InputTokens: 30}),
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

	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	// limit=2 of a 3-message post-compaction history (summary, kept-user,
	// kept-assistant): a genuinely partial window that excludes the
	// summary, called FIRST against a still-virgin s.journal.
	windowedResp, windowedData := h.do("GET", "/session/"+seed.ID+"/message?stream_from=1&limit=2", nil)
	if windowedResp.StatusCode != 200 {
		t.Fatalf("GET windowed = %d: %s", windowedResp.StatusCode, windowedData)
	}
	var windowed transcriptResponse
	if err := json.Unmarshal(windowedData, &windowed); err != nil {
		t.Fatalf("decode windowed: %v (%s)", err, windowedData)
	}
	if len(windowed.Messages) != 2 {
		t.Fatalf("got %d windowed messages, want 2 (a genuinely partial window)", len(windowed.Messages))
	}

	full, fullMeta := getTranscript(t, h, seed.ID)
	if fullMeta.status != 200 {
		t.Fatalf("GET full (oracle) = %d: %s", fullMeta.status, fullMeta.body)
	}
	if len(full.Messages) != 3 {
		t.Fatalf("got %d full messages, want 3 (summary + 2 kept)", len(full.Messages))
	}

	if windowed.StreamFrom != full.StreamFrom {
		t.Errorf("windowed stream_from = %d, want %d (full-read oracle)", windowed.StreamFrom, full.StreamFrom)
	}
	if windowed.LiveFrom != full.LiveFrom {
		t.Errorf("windowed live_from = %d, want %d (full-read oracle)", windowed.LiveFrom, full.LiveFrom)
	}
}

// TestColdWindowedBootstrap_MultiCompactionNeverExceedsTrueTip is the guard
// the reviewer asked for: a session with TWO compactions, so an earlier
// compaction's own summary (summary1) is folded away by a second
// compaction and replaced by a new summary (summary2) that sits at the
// EARLIEST ordinal (1) in the current, folded numbering even though it
// was created LAST, chronologically -- the shape where a windowed read
// could, in principle, exclude the one record whose true seq is the
// session's actual highest.
//
// # Why this cannot be driven end-to-end through live HTTP calls
//
// The natural way to get this state would be two REAL POST /compact
// calls against a live session (server/compact_test.go's own flow), then
// a windowed read on the SAME, now-idle session. That does not reach
// coldWindowedBootstrap at all: Server.handleCreate calls
// s.sessMgr.AdoptRoot(sess) for every root session (handlers.go:879), and
// "a root is adopted into sessMgr and never reaped" (handlers.go:3467-
// 3475) -- confirmed directly: after driving two live compactions on a
// session, then evicting it from s.sessions with MaxResident=1 (a second
// session's own prompt forces the LRU eviction), Server.residentSession
// (which checks ONLY s.sessions) correctly reports it gone, but
// Server.liveSessionObject -- the check coldWindowedBootstrap actually
// gates on -- still returns the session, resolved through
// s.sessMgr.Session instead. So a session THIS PROCESS has ever driven a
// live turn or compaction for can never reach coldWindowedBootstrap's
// cold branch again, for the rest of the process's life: the bail-out at
// the top of coldWindowedBootstrap (server/handlers.go) fires every time.
//
// This is not merely a test-authoring obstacle -- it is the same
// structural fact in production. The ONLY way a process's own s.journal
// can hold BOTH compactions' evtMessage/evtHistoryCompacted records at
// their true, incrementally-assigned (chronological) seqs is for that
// process to have been resident and driving the session through both
// live compactions -- and by the argument above, such a process can never
// again answer that same session's bootstrap from the cold branch. Every
// process that DOES reach the cold branch for this session only ever
// learns of both compactions from the FINAL, already-doubly-folded
// on-disk state, all at once (one full stream_from=1 read, or
// Server.reconcile's own startup replay) -- which is exactly
// TestColdWindowedBootstrap_StreamFromParityAfterSeededJournal and
// TestColdWindowedBootstrap_ParityWithFullRead_CompactedPartialWindow's
// own single-batch-fold shape, where the excluded summary always lands at
// the LOWEST seq of that batch (history[0], journaled first in array
// order) and so can only ever pull a windowed watermark DOWN, never up.
//
// # What this test does instead
//
// It builds the ACTUAL on-disk session through two REAL
// engine.Session.Compact calls (so ReadMessagePage's tailPage/foldedPage
// fold, SessionIndex, and the windowed HTTP path all run genuine,
// unmodified production code against a real doubly-compacted journal),
// then seeds THIS harness's own s.journal by calling emitDurableLocked
// directly, in the exact chronological order and shape a live
// two-compaction run would have produced -- summary1's own evtMessage,
// then its evtHistoryCompacted, THEN (after the kept turn) summary2's own
// evtMessage, then ITS evtHistoryCompacted -- so summary2 lands at a seq
// higher than the kept turn's own messages, exactly the property a real
// live run would have and the earlier two tests' setups cannot produce.
// This is the same class of construction
// TestTranscriptWatermarkLocked_CompactionSummarySandwich and
// fabricateExcludedBacklog (transcript_live_from_test.go) already use for
// a state "that has no HTTP-level trigger yet" -- here, provably no
// HTTP-level trigger CAN exist, not merely none is wired up yet. The kept
// turn's own two messages are marked seen (markSeenLocked) as part of the
// injection so the real windowed HTTP call below does not re-journal them
// itself and quietly overwrite the constructed ordering.
//
// # What it asserts
//
// Not exact parity with the full path (which does not hold in every
// direction for an already-landed multi-compaction session -- see
// docs/design/fast-transcript-bootstrap.md §4.4a). Instead, the two-part
// safety bound the fix actually guarantees:
//
//  1. windowed.StreamFrom never exceeds the session's true tip
//     (h.srv.currentSeq(), an unimpeachable upper bound sampled after
//     every injected event and the windowed read itself).
//  2. No message the full (unwindowed) path currently renders is
//     skipped: every entry in full.Messages is either already present in
//     windowed.Messages, or its own durably journaled seq is strictly
//     ABOVE windowed.StreamFrom -- so a consumer resuming GET /event from
//     windowed.StreamFrom is guaranteed to receive it. A live SSE resume
//     from windowed.StreamFrom is then driven for real, confirming
//     summary2 -- the specific excluded, high-seq record -- actually
//     arrives over the wire, not merely in the journal's own bookkeeping.
func TestColdWindowedBootstrap_MultiCompactionNeverExceedsTrueTip(t *testing.T) {
	dir := t.TempDir()
	// Harness FIRST, against an empty dir (reconcile finds nothing) --
	// the seed session below is written to this same dir only afterward,
	// out-of-process, exactly like
	// TestColdWindowedBootstrap_StreamFromParityAfterSeededJournal.
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	seedProv := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("three", provider.Usage{InputTokens: 30}),
		compactAsstTurn("summary1", provider.Usage{InputTokens: 5}),
		compactAsstTurn("four", provider.Usage{InputTokens: 40}),
		compactAsstTurn("five", provider.Usage{InputTokens: 50}),
		compactAsstTurn("summary2", provider.Usage{InputTokens: 5}),
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
	compact1, err := seed.Compact(context.Background(), engine.CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatalf("seed Compact #1: %v", err)
	}
	if compact1.Summary == nil {
		t.Fatal("compact #1 produced no summary")
	}
	summary1ID := compact1.Summary.ID

	for i, text := range []string{"go4", "go5"} {
		if _, err := seed.Prompt(context.Background(), text); err != nil {
			t.Fatalf("seed Prompt (post-compact1) %d: %v", i, err)
		}
	}
	compact2, err := seed.Compact(context.Background(), engine.CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatalf("seed Compact #2: %v", err)
	}
	if compact2.Summary == nil {
		t.Fatal("compact #2 produced no summary")
	}
	summary2ID := compact2.Summary.ID
	if err := seed.PersistErr(); err != nil {
		t.Fatalf("seed PersistErr: %v", err)
	}

	finalHistory := seed.History()
	if len(finalHistory) != 3 {
		t.Fatalf("seed's final history has %d messages, want 3 (summary2 + kept turn5 user+assistant)", len(finalHistory))
	}
	if finalHistory[0].ID != summary2ID {
		t.Fatalf("finalHistory[0].ID = %s, want summary2 %s", finalHistory[0].ID, summary2ID)
	}
	turn5User := finalHistory[1]
	turn5Asst := finalHistory[2]

	// Seed h.srv's own s.journal directly, in the chronological order and
	// shape a live two-compaction run would have produced (see the doc
	// comment above for why this cannot be driven through live HTTP calls
	// instead). markSeenLocked for the kept turn's own two messages so the
	// real windowed HTTP call below does not re-journal them itself.
	h.srv.mu.Lock()
	h.srv.markSeenLocked(seed.ID, turn5User.ID)
	h.srv.emitDurableLocked(&Event{Type: evtMessage, SessionID: seed.ID, Message: &turn5User})
	h.srv.markSeenLocked(seed.ID, turn5Asst.ID)
	h.srv.emitDurableLocked(&Event{Type: evtMessage, SessionID: seed.ID, Message: &turn5Asst})
	h.srv.emitDurableLocked(&Event{Type: evtMessage, SessionID: seed.ID, Message: compact1.Summary})
	h.srv.emitDurableLocked(&Event{
		Type: evtHistoryCompacted, SessionID: seed.ID,
		CompactFirstID: compact1.FirstID, CompactLastID: compact1.LastID,
		CompactTurnsFolded: compact1.TurnsFolded, CompactSummaryID: summary1ID,
	})
	h.srv.markSeenLocked(seed.ID, summary2ID)
	h.srv.emitDurableLocked(&Event{Type: evtMessage, SessionID: seed.ID, Message: compact2.Summary})
	h.srv.emitDurableLocked(&Event{
		Type: evtHistoryCompacted, SessionID: seed.ID,
		CompactFirstID: compact2.FirstID, CompactLastID: compact2.LastID,
		CompactTurnsFolded: compact2.TurnsFolded, CompactSummaryID: summary2ID,
	})
	h.srv.mu.Unlock()

	if h.srv.liveSessionObject(seed.ID) != nil {
		t.Fatal("seed.ID unexpectedly resident -- test setup invariant broken")
	}

	// The windowed read: limit=2 of the 3-message post-compaction-#2
	// history, excluding summary2 -- the record whose injected seq is the
	// session's true highest.
	resp, data := h.do("GET", "/session/"+seed.ID+"/message?stream_from=1&limit=2", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET windowed = %d: %s", resp.StatusCode, data)
	}
	var windowed transcriptResponse
	if err := json.Unmarshal(data, &windowed); err != nil {
		t.Fatalf("decode windowed: %v (%s)", err, data)
	}
	if len(windowed.Messages) != 2 {
		t.Fatalf("got %d windowed messages, want 2 (turn5's user+assistant pair, excluding summary2)", len(windowed.Messages))
	}
	for _, m := range windowed.Messages {
		if m.ID == summary2ID {
			t.Fatalf("windowed messages unexpectedly include summary2 %s -- test setup invariant broken (limit=2 should exclude it)", summary2ID)
		}
	}
	if h.srv.liveSessionObject(seed.ID) != nil {
		t.Fatal("the windowed GET itself made the session resident -- coldWindowedBootstrap must never do that")
	}

	// Oracle: full.Messages is what the unwindowed path currently renders
	// -- used ONLY for message-identity/seq membership below, never for
	// its own StreamFrom (a different, pre-existing full-path computation
	// this test does not exercise or claim anything about).
	full, fullMeta := getTranscript(t, h, seed.ID)
	if fullMeta.status != 200 {
		t.Fatalf("GET full (oracle) = %d: %s", fullMeta.status, fullMeta.body)
	}
	if len(full.Messages) != 3 {
		t.Fatalf("got %d full messages, want 3 (summary2 + turn5 user+assistant)", len(full.Messages))
	}

	// Assertion (a): never exceeds the session's true tip.
	trueTip := h.srv.currentSeq()
	if windowed.StreamFrom > trueTip {
		t.Errorf("windowed stream_from = %d, want <= the session's true tip %d", windowed.StreamFrom, trueTip)
	}
	if windowed.LiveFrom > trueTip {
		t.Errorf("windowed live_from = %d, want <= the session's true tip %d", windowed.LiveFrom, trueTip)
	}

	// Assertion (b): no message the full path currently renders is
	// skipped -- either already in the window, or still resumable above
	// windowed.StreamFrom.
	inWindow := make(map[string]bool, len(windowed.Messages))
	for _, m := range windowed.Messages {
		inWindow[m.ID] = true
	}
	seqByID := journalSeqByMessageID(h.srv, seed.ID)
	for _, m := range full.Messages {
		if inWindow[m.ID] {
			continue
		}
		seq, journaled := seqByID[m.ID]
		if !journaled {
			t.Errorf("message %s (currently rendered by the full path) was never journaled at all", m.ID)
			continue
		}
		if seq <= windowed.StreamFrom {
			t.Errorf("message %s (currently rendered, excluded from the window) has seq %d <= windowed stream_from %d -- a live resume from stream_from would never redeliver it (a gap)", m.ID, seq, windowed.StreamFrom)
		}
	}

	// Empirical confirmation: a real SSE resume from windowed.StreamFrom
	// actually redelivers summary2, the specific excluded, high-seq
	// record this test constructs.
	want := journalEventsAbove(h, seed.ID, windowed.StreamFrom)
	if len(want) == 0 {
		t.Fatalf("no durable events above windowed stream_from %d for session %s; expected at least summary2's own record", windowed.StreamFrom, seed.ID)
	}
	sse := h.openSSE("?from="+itoa64(windowed.StreamFrom)+"&session="+seed.ID, "")
	sawSummary2 := false
	for i := 0; i < len(want); i++ {
		ev := sse.nextEvent(t)
		if ev.Type == evtMessage && ev.Message != nil && ev.Message.ID == summary2ID {
			sawSummary2 = true
		}
	}
	if !sawSummary2 {
		t.Errorf("resuming SSE from windowed stream_from %d never redelivered summary2 %s, which the window excluded", windowed.StreamFrom, summary2ID)
	}
}

// TestTranscriptBootstrap_ResidentSessionHonorsLimit is the regression test
// for a Copilot review finding on PR #265: handleTranscriptBootstrap
// honored limit only on coldWindowedBootstrap's own success path, silently
// ignoring it on every fallback (a resident session, an unreadable index/
// page, or a lost residency race) and returning the WHOLE history instead
// — contradicting both the PR description and openapi.yaml's own
// "stream_from+limit narrows messages to the latest window" claim for a
// resident session, the single most common case (a console's own session
// is resident for as long as it stays actively open).
//
// windowTranscriptTail narrows transcriptSyncedThrough's own Messages/Seqs
// to their tail AFTER the cursor is computed from the complete history, so
// StreamFrom/LiveFrom must be identical to what a plain, unwindowed
// stream_from=1 read of the SAME resident session reports — narrowing the
// returned window never invalidates a cursor that already describes the
// whole history.
func TestTranscriptBootstrap_ResidentSessionHonorsLimit(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("one"), asstTurn("two"), asstTurn("three"),
	}}
	h := newHarness(t, prov)
	id := h.createSession("")
	for _, text := range []string{"go1", "go2", "go3"} {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": text}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt_async(%q) = %d: %s", text, resp.StatusCode, data)
		}
		h.waitIdle(id)
	}
	if h.srv.residentSession(id) == nil {
		t.Fatal("session not resident -- test setup invariant broken")
	}

	full, fullMeta := getTranscript(t, h, id) // stream_from=1, no limit
	if fullMeta.status != 200 {
		t.Fatalf("GET full = %d: %s", fullMeta.status, fullMeta.body)
	}
	if len(full.Messages) != 6 {
		t.Fatalf("got %d full messages, want 6 (3 turns' user+assistant pairs)", len(full.Messages))
	}

	resp, data := h.do("GET", "/session/"+id+"/message?stream_from=1&limit=2", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET windowed(resident) = %d: %s", resp.StatusCode, data)
	}
	if h.srv.residentSession(id) == nil {
		t.Fatal("session unexpectedly not resident anymore -- test setup invariant broken")
	}
	var windowed transcriptResponse
	if err := json.Unmarshal(data, &windowed); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}
	if len(windowed.Messages) != 2 {
		t.Fatalf("got %d windowed messages, want 2 (the resident session's own tail) -- limit was ignored on the resident fallback path", len(windowed.Messages))
	}

	wantTail := full.Messages[len(full.Messages)-2:]
	for i := range wantTail {
		if windowed.Messages[i].ID != wantTail[i].ID {
			t.Errorf("windowed.Messages[%d].ID = %s, want %s (full path's own tail)", i, windowed.Messages[i].ID, wantTail[i].ID)
		}
	}
	if len(windowed.Seqs) != 2 {
		t.Fatalf("got %d windowed seqs, want 2", len(windowed.Seqs))
	}
	wantSeqsTail := full.Seqs[len(full.Seqs)-2:]
	for i := range wantSeqsTail {
		if windowed.Seqs[i] != wantSeqsTail[i] {
			t.Errorf("windowed.Seqs[%d] = %d, want %d (full path's own tail)", i, windowed.Seqs[i], wantSeqsTail[i])
		}
	}

	// The cursor covers the resident session's COMPLETE history, computed
	// before narrowing to the tail -- so it must be identical to the
	// unwindowed full path's own cursor, not merely consistent with the
	// smaller returned window.
	if windowed.StreamFrom != full.StreamFrom {
		t.Errorf("windowed stream_from = %d, want %d (the full path's own cursor, unaffected by narrowing Messages/Seqs to the tail)", windowed.StreamFrom, full.StreamFrom)
	}
	if windowed.LiveFrom != full.LiveFrom {
		t.Errorf("windowed live_from = %d, want %d", windowed.LiveFrom, full.LiveFrom)
	}
}
