package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// scriptedUsageAnthropic is a fake Anthropic Messages API that streams one
// scripted reply (text + explicit input/output token usage) per request, in
// order — driving a precise Usage/LastUsage trajectory across turns so a
// test can cross a compaction threshold deterministically, unlike
// fakeAnthropic (which only varies stall behavior).
type scriptedUsageAnthropic struct {
	mu    sync.Mutex
	turns []scriptedUsageTurn
	call  int
}

type scriptedUsageTurn struct {
	text         string
	inputTokens  int
	outputTokens int
}

func (f *scriptedUsageAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	i := f.call
	f.call++
	f.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if i >= len(f.turns) {
		// Out-of-script request: fail loudly and visibly rather than
		// hanging, so a scripting mistake shows up as a clear test failure.
		io.WriteString(w, sse("error", `{"type":"error","error":{"type":"invalid_request_error","message":"e2e: scriptedUsageAnthropic ran out of scripted turns"}}`))
		flusher.Flush()
		return
	}
	turn := f.turns[i]
	io.WriteString(w, completeTurnWithUsage(fmt.Sprintf("msg_%d", i), turn.text, turn.inputTokens, turn.outputTokens))
	flusher.Flush()
}

// requestCount reports how many requests have been served so far.
func (f *scriptedUsageAnthropic) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.call
}

// writeCompactionConfig writes a harness config pointing the anthropic
// provider at the fake server and enabling automatic compaction with a
// small, test-configured threshold.
func writeCompactionConfig(t *testing.T, baseURL string, contextWindowTokens int, keepTurns int) string {
	t.Helper()
	cfg := map[string]any{
		"model": "anthropic/claude-fable-5",
		"providers": map[string]any{
			"anthropic": map[string]any{
				"api_key_env": "ANTHROPIC_API_KEY",
				"base_url":    baseURL,
			},
		},
		"context_window_tokens": contextWindowTokens,
		"compaction_keep_turns": keepTurns,
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

// sessionUsageJSON is the subset of GET /session's usage sub-object this
// test needs.
type sessionUsageJSON struct {
	InputTokens     int `json:"input_tokens"`
	LastInputTokens int `json:"last_input_tokens"`
}

// sessionCompactionJSON is the subset of GET /session this test needs.
type sessionCompactionJSON struct {
	Messages        int              `json:"messages"`
	Usage           sessionUsageJSON `json:"usage"`
	CompactionCount int              `json:"compaction_count"`
	LastCompactedAt time.Time        `json:"last_compacted_at"`
}

func (p *serveProc) getSession(id string) sessionCompactionJSON {
	p.t.Helper()
	resp, data := p.do(http.MethodGet, "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		p.t.Fatalf("get session: status %d body %s", resp.StatusCode, data)
	}
	var s sessionCompactionJSON
	if err := json.Unmarshal(data, &s); err != nil {
		p.t.Fatalf("decode session: %v (%s)", err, data)
	}
	return s
}

// TestAutoCompactionAcrossRestart is the e2e keystone for docs/design/
// context-compaction.md: a real `harness serve` binary, driven by a scripted
// fake Anthropic provider, is pushed past a small test-configured
// context-window threshold; automatic compaction must fire, the session
// must keep working afterward with a visibly smaller usage picture, and a
// hard restart (SIGKILL, fresh process, same session dir) must replay the
// compact journal record and the trimmed history correctly — a post-
// compaction session restarts exactly as cleanly as an ordinary one.
func TestAutoCompactionAcrossRestart(t *testing.T) {
	skipShort(t)

	fake := &scriptedUsageAnthropic{turns: []scriptedUsageTurn{
		{text: "reply one", inputTokens: 100, outputTokens: 10},          // turn 1: under threshold
		{text: "reply two", inputTokens: 900, outputTokens: 10},          // turn 2: over threshold (800), triggers compaction before turn 3
		{text: "the gist of turns one and two", inputTokens: 40, outputTokens: 15}, // compaction's own summarization call
		{text: "reply three", inputTokens: 120, outputTokens: 10},        // turn 3: proceeds normally post-compaction
		{text: "reply four", inputTokens: 130, outputTokens: 10},         // turn 4: after restart, still working
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	sessDir := t.TempDir()
	// context_window_tokens=1000, default threshold 0.8 => 800; keep_turns=1
	// so folding can actually happen with just 2 completed turns.
	cfgPath := writeCompactionConfig(t, srv.URL, 1000, 1)

	p1 := startServe(t, sessDir, cfgPath)
	id := p1.createSession()

	p1.prompt(id, "go1")
	p1.waitMessages(id, 2) // user1, assistant1

	p1.prompt(id, "go2")
	p1.waitMessages(id, 4) // user1, assistant1, user2, assistant2

	before := p1.getSession(id)
	if before.CompactionCount != 0 {
		t.Fatalf("CompactionCount before turn 3 = %d, want 0", before.CompactionCount)
	}
	if before.Usage.LastInputTokens != 900 {
		t.Fatalf("LastInputTokens before turn 3 = %d, want 900 (turn 2's usage)", before.Usage.LastInputTokens)
	}

	// Turn 3: maybeAutoCompact must fire BEFORE this turn's own request —
	// folding turn 1 into a summary (keep_turns=1 keeps turn 2 verbatim) —
	// so the scripted provider sees the summarization call as its 3rd
	// request and turn 3's own worker call as its 4th, in that order.
	p1.prompt(id, "go3")
	// Post-compaction history: summary(1) + turn2(2) + turn3(2) = 5.
	final := p1.waitMessages(id, 5)
	assertUniqueMessageIDs(t, final)
	if final[0].Role != "user" {
		t.Fatalf("messages[0].Role = %q, want user (the compaction summary)", final[0].Role)
	}
	if textOf(final[0]) == "" {
		t.Fatalf("compaction summary message has no text")
	}

	after := p1.getSession(id)
	if after.CompactionCount != 1 {
		t.Fatalf("CompactionCount after turn 3 = %d, want 1 (automatic compaction must have fired)", after.CompactionCount)
	}
	if after.LastCompactedAt.IsZero() {
		t.Error("LastCompactedAt is zero after a successful compaction")
	}
	// The session must keep working: turn 3's own usage becomes the new
	// LastInputTokens, and it must be visibly smaller than the 900 that
	// triggered compaction — the "usage drop" the e2e is required to show.
	if after.Usage.LastInputTokens != 120 {
		t.Fatalf("LastInputTokens after turn 3 = %d, want 120 (turn 3's own usage, not the compaction call's)", after.Usage.LastInputTokens)
	}
	if after.Usage.LastInputTokens >= before.Usage.LastInputTokens {
		t.Errorf("LastInputTokens did not drop: before=%d after=%d", before.Usage.LastInputTokens, after.Usage.LastInputTokens)
	}

	if got := fake.requestCount(); got != 4 {
		t.Fatalf("provider requests = %d, want 4 (2 worker turns + 1 compaction summary + 1 more worker turn)", got)
	}

	// The durable journal shows the summary message BEFORE history.compacted,
	// and history.compacted names the fold.
	events := p1.eventReplay()
	assertContiguousSeqs(t, events)
	var summaryID string
	var sawSummaryMessage, sawCompacted bool
	for _, ev := range events {
		if ev.SessionID != id {
			continue
		}
		switch ev.Type {
		case "message":
			if ev.Message != nil && ev.Message.ID == final[0].ID {
				sawSummaryMessage = true
				summaryID = ev.Message.ID
			}
		case "history.compacted":
			if !sawSummaryMessage {
				t.Fatal("history.compacted event/record arrived before the summary's message event")
			}
			sawCompacted = true
			if ev.CompactTurnsFolded != 1 {
				t.Errorf("history.compacted turns_folded = %d, want 1", ev.CompactTurnsFolded)
			}
			if ev.CompactSummaryID != summaryID {
				t.Errorf("history.compacted summary id = %q, want %q", ev.CompactSummaryID, summaryID)
			}
		}
	}
	if !sawSummaryMessage {
		t.Fatal("never saw the summary's message event in the journal replay")
	}
	if !sawCompacted {
		t.Fatal("never saw a history.compacted event in the journal replay")
	}

	// --- restart: SIGKILL, fresh process, same session dir -------------
	p1.kill()
	p2 := startServe(t, sessDir, cfgPath)

	restarted := p2.getSession(id)
	if restarted.CompactionCount != 1 {
		t.Errorf("CompactionCount after restart = %d, want 1 (compact record must replay)", restarted.CompactionCount)
	}
	if restarted.LastCompactedAt.IsZero() {
		t.Error("LastCompactedAt after restart is zero, want the compaction's timestamp to survive")
	}
	if restarted.Messages != 5 {
		t.Fatalf("Messages after restart = %d, want 5 (trimmed history must survive)", restarted.Messages)
	}

	msgs := p2.messages(id)
	assertUniqueMessageIDs(t, msgs)
	if len(msgs) != 5 {
		t.Fatalf("messages after restart = %d, want 5", len(msgs))
	}
	if msgs[0].ID != final[0].ID || msgs[0].Role != "user" {
		t.Fatalf("messages[0] after restart = %+v, want the same summary message", msgs[0])
	}

	// The journal replay is still contiguous and reconciled after restart.
	assertContiguousSeqs(t, p2.eventReplay())

	// A post-compaction session must restart cleanly and keep working: a
	// further prompt succeeds end to end.
	p2.prompt(id, "go4")
	final2 := p2.waitMessages(id, 7) // + user4, assistant4
	assertUniqueMessageIDs(t, final2)
	if got := final2[len(final2)-1]; got.Role != "assistant" || textOf(got) != "reply four" {
		t.Fatalf("final message after restart+prompt = %+v, want assistant \"reply four\"", got)
	}
}

