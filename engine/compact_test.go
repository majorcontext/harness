package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// compactTurnSeq gives each compactTurn call in a test a distinct assistant
// message ID: asstTurn's shared "msg_a" constant is fine for tests that
// never compare IDs across turns, but compaction's FirstID/LastID and its
// ID-based splice (see spliceCompact) need turns to be distinguishable by
// ID, exactly as production message IDs always are (every message gets a
// fresh newID()).
var compactTurnSeq int

// compactTurn builds a scripted worker-turn assistant reply carrying usage,
// with a fresh, unique message ID (see compactTurnSeq).
func compactTurn(text string, usage provider.Usage) []provider.Event {
	compactTurnSeq++
	msg := &message.Message{ID: fmt.Sprintf("msg_asst_%d", compactTurnSeq), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}}
	ev := provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn, Usage: usage}
	return []provider.Event{ev}
}

// compactSummaryTurn builds a scripted reply for the tool-less summarization
// call Session.Compact issues: a plain text assistant message, no tool
// calls, ending the stream via EventDone.
func compactSummaryTurn(text string, usage provider.Usage) []provider.Event {
	return compactTurn(text, usage)
}

// runTurns drives n ordinary Prompt calls against s, failing the test on any
// error.
func runTurns(t *testing.T, s *Session, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.Prompt(context.Background(), "go"); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
}

// TestCompactFoldsOldestPrefixKeepsRecentTurns is the red-first behavior
// test for the core mechanism (docs/design/context-compaction.md §2): a
// contiguous prefix of whole turns folds into one summary message, the most
// recent keep_turns turns survive verbatim, and FirstID/LastID name exactly
// the folded range's boundary messages.
func TestCompactFoldsOldestPrefixKeepsRecentTurns(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10, OutputTokens: 5}),
		compactTurn("two", provider.Usage{InputTokens: 20, OutputTokens: 5}),
		compactTurn("three", provider.Usage{InputTokens: 30, OutputTokens: 5}),
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 40, OutputTokens: 8}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 3)

	before := s.History()
	if len(before) != 6 {
		t.Fatalf("history before compact = %d messages, want 6 (3 turns x 2)", len(before))
	}
	wantFirstID := before[0].ID // turn 1's leading RoleUser message
	wantLastID := before[3].ID  // last message before turn 3's leading RoleUser message

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.TurnsFolded != 2 {
		t.Fatalf("TurnsFolded = %d, want 2", res.TurnsFolded)
	}
	if res.FirstID != wantFirstID {
		t.Errorf("FirstID = %q, want %q", res.FirstID, wantFirstID)
	}
	if res.LastID != wantLastID {
		t.Errorf("LastID = %q, want %q", res.LastID, wantLastID)
	}

	after := s.History()
	if len(after) != 3 {
		t.Fatalf("history after compact = %d messages, want 3 (summary + kept turn 3)", len(after))
	}
	if after[0].Role != message.RoleUser {
		t.Errorf("after[0].Role = %s, want RoleUser (summary)", after[0].Role)
	}
	if after[0].ID != res.Summary.ID {
		t.Errorf("after[0].ID = %q, want summary id %q", after[0].ID, res.Summary.ID)
	}
	if got := after[0].Parts.Text(); got == "" {
		t.Error("summary message has no text")
	}
	// Turn 3 survives verbatim.
	if after[1].Parts.Text() != "go" && after[1].Role != message.RoleUser {
		t.Errorf("after[1] = %+v, want turn 3's user message", after[1])
	}
	if after[2].Parts.Text() != "three" {
		t.Errorf("after[2] text = %q, want %q (turn 3's assistant reply)", after[2].Parts.Text(), "three")
	}
}

// TestCompactSummaryBannerMarksSyntheticOrigin asserts the summary text
// carries the visible synthesized-and-marked banner, mirroring
// message.SyntheticOrphanResultText's spirit — a transcript reader can never
// mistake it for something the human actually typed.
func TestCompactSummaryBannerMarksSyntheticOrigin(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("the gist", provider.Usage{InputTokens: 5}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Summary.Parts.Text()
	if got != CompactionSummaryBanner+"the gist" {
		t.Errorf("summary text = %q, want banner-prefixed", got)
	}
}

// TestCompactNoopWhenNotEnoughTurns is the red-first test for §2's minimum-
// fold rule: fewer than keep_turns complete turns exist yet, so compaction
// is a no-op (turns_folded: 0, not an error) — and, crucially, it never
// calls the provider at all (nothing to summarize).
func TestCompactNoopWhenNotEnoughTurns(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 1)

	before := s.History()
	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 2})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.TurnsFolded != 0 {
		t.Errorf("TurnsFolded = %d, want 0", res.TurnsFolded)
	}
	if len(prov.requests) != 1 {
		t.Errorf("provider calls = %d, want 1 (only the worker turn — no summarization call)", len(prov.requests))
	}
	after := s.History()
	if len(after) != len(before) {
		t.Errorf("history mutated on a no-op compaction: before=%d after=%d", len(before), len(after))
	}
}

// TestCompactKeepTurnsFloor is the red-first test for the hard floor on
// keep_turns (docs/design/context-compaction.md §1): the most recent turn
// is never foldable, so even an aggressive KeepTurns request always leaves
// at least one whole turn verbatim.
func TestCompactKeepTurnsFloor(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)

	// KeepTurns: 1 is the minimum valid value (server-side validation
	// rejects <= 0 before ever reaching here — see server/handlers.go); the
	// engine must honor it exactly, never defaulting it away.
	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsFolded != 1 {
		t.Fatalf("TurnsFolded = %d, want 1", res.TurnsFolded)
	}
	after := s.History()
	if len(after) != 3 { // summary + kept turn's user+assistant
		t.Fatalf("history after compact = %d messages, want 3", len(after))
	}
}

// TestCompactUsageAccountingCumulativeOnlyNotLastUsage is the red-first test
// for §2's "Usage accounting": the summarization call's tokens are real
// spend and must be added to cumulative Usage(), but must NEVER overwrite
// LastUsage() — the automatic trigger reads LastUsage as "how large is the
// next worker request", and a small summarization call would mask the very
// pressure that triggered compaction.
func TestCompactUsageAccountingCumulativeOnlyNotLastUsage(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 100, OutputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 200, OutputTokens: 10}),
		compactSummaryTurn("gist", provider.Usage{InputTokens: 7, OutputTokens: 3}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)

	wantUsageBefore := provider.Usage{InputTokens: 300, OutputTokens: 20}
	if got := s.Usage(); got != wantUsageBefore {
		t.Fatalf("Usage before compact = %+v, want %+v", got, wantUsageBefore)
	}
	lastBefore, _ := s.LastUsage()

	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatal(err)
	}

	wantUsageAfter := provider.Usage{InputTokens: 307, OutputTokens: 23}
	if got := s.Usage(); got != wantUsageAfter {
		t.Errorf("Usage after compact = %+v, want %+v (summarization spend added)", got, wantUsageAfter)
	}
	lastAfter, ok := s.LastUsage()
	if !ok {
		t.Fatal("LastUsage not ok after compact")
	}
	if lastAfter != lastBefore {
		t.Errorf("LastUsage changed by compaction: before=%+v after=%+v, want unchanged", lastBefore, lastAfter)
	}
}

// TestCompactFailureNoJournalNoMutation is the red-first test for §2's
// "Failure handling": when the summarization call itself errors, compaction
// aborts cleanly — no history mutation, no journal write, and an emitted
// EventCompactionFailed.
func TestCompactFailureNoJournalNoMutation(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		// No third scripted turn: the summarization call (the 3rd
		// prov.Stream call) exhausts p.turns and returns io.ErrUnexpectedEOF
		// (see scriptedProvider.Stream).
	}}
	dir := t.TempDir()
	var evs []Event
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
		OnEvent:    func(ev Event) { evs = append(evs, ev) },
	})
	runTurns(t, s, 2)
	before := s.History()
	beforeCompactCount := s.CompactionCount()

	_, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err == nil {
		t.Fatal("Compact succeeded, want an error (provider call exhausted)")
	}

	after := s.History()
	if len(after) != len(before) {
		t.Errorf("history mutated on a failed compaction: before=%d after=%d", len(before), len(after))
	}
	if got := s.CompactionCount(); got != beforeCompactCount {
		t.Errorf("CompactionCount = %d, want unchanged at %d", got, beforeCompactCount)
	}

	var failed int
	for _, ev := range evs {
		if ev.Type == EventCompactionFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("EventCompactionFailed count = %d, want 1", failed)
	}

	// Reload: the log must show no compact record — a torn/aborted
	// compaction is indistinguishable from "never started" (§3 "Crash
	// discipline").
	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.CompactionCount(); got != 0 {
		t.Errorf("reloaded CompactionCount = %d, want 0 (failed compaction never journaled)", got)
	}
	if len(loaded.History()) != len(before) {
		t.Errorf("reloaded history = %d messages, want %d (unchanged)", len(loaded.History()), len(before))
	}
}

// TestCompactSummaryFlowsThroughEventMessageBeforeHistoryCompacted is the
// red-first test for §4's "Live event surface": a successful compaction
// emits the summary via the ordinary EventMessage path FIRST, then
// EventHistoryCompacted — never the other order, or an events.jsonl tailer
// would hold a dangling summary_id it never received a message for.
func TestCompactSummaryFlowsThroughEventMessageBeforeHistoryCompacted(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5}),
	}}
	var evs []Event
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		OnEvent:   func(ev Event) { evs = append(evs, ev) },
	})
	runTurns(t, s, 2)
	evs = nil // discard the two ordinary turns' events

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}

	var messageIdx, compactedIdx = -1, -1
	for i, ev := range evs {
		switch ev.Type {
		case EventMessage:
			if ev.Message != nil && ev.Message.ID == res.Summary.ID {
				messageIdx = i
			}
		case EventHistoryCompacted:
			compactedIdx = i
		}
	}
	if messageIdx == -1 {
		t.Fatal("no EventMessage carrying the summary was emitted")
	}
	if compactedIdx == -1 {
		t.Fatal("no EventHistoryCompacted was emitted")
	}
	if messageIdx >= compactedIdx {
		t.Errorf("EventMessage(summary) at %d, EventHistoryCompacted at %d; want message strictly before", messageIdx, compactedIdx)
	}
	last := evs[compactedIdx]
	if last.CompactFirstID != res.FirstID || last.CompactLastID != res.LastID ||
		last.CompactTurnsFolded != res.TurnsFolded || last.CompactSummaryID != res.Summary.ID {
		t.Errorf("EventHistoryCompacted = %+v, want it to carry the compact result", last)
	}
}

// TestCompactSurvivesReload is the red-first restart test for §2's
// "LoadSession replay": a reloaded session replays the compact record and
// the trimmed history — the summary lands exactly where it did live, and
// cumulative usage (including the summarization spend) survives, but
// LastUsage does not pick up the summarization call's tiny usage.
func TestCompactSurvivesReload(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 100, OutputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 200, OutputTokens: 10}),
		compactTurn("three", provider.Usage{InputTokens: 300, OutputTokens: 10}),
		compactSummaryTurn("the gist of turns one and two", provider.Usage{InputTokens: 9, OutputTokens: 4}),
	}}
	dir := t.TempDir()
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	runTurns(t, s, 3)

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	wantHistory := s.History()
	wantUsage := s.Usage()
	wantLast, _ := s.LastUsage()
	wantCount := s.CompactionCount()
	wantLastCompactedAt := s.LastCompactedAt()

	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotHistory := loaded.History()
	if len(gotHistory) != len(wantHistory) {
		t.Fatalf("reloaded history = %d messages, want %d", len(gotHistory), len(wantHistory))
	}
	for i := range wantHistory {
		if gotHistory[i].ID != wantHistory[i].ID || gotHistory[i].Role != wantHistory[i].Role ||
			gotHistory[i].Parts.Text() != wantHistory[i].Parts.Text() {
			t.Errorf("reloaded history[%d] = %+v, want %+v", i, gotHistory[i], wantHistory[i])
		}
	}
	if got := loaded.Usage(); got != wantUsage {
		t.Errorf("reloaded Usage = %+v, want %+v", got, wantUsage)
	}
	last, ok := loaded.LastUsage()
	if !ok || last != wantLast {
		t.Errorf("reloaded LastUsage = %+v (ok=%v), want %+v", last, ok, wantLast)
	}
	if got := loaded.CompactionCount(); got != wantCount {
		t.Errorf("reloaded CompactionCount = %d, want %d", got, wantCount)
	}
	if !loaded.LastCompactedAt().Equal(wantLastCompactedAt) {
		t.Errorf("reloaded LastCompactedAt = %v, want %v", loaded.LastCompactedAt(), wantLastCompactedAt)
	}
	if res.TurnsFolded != 2 {
		t.Fatalf("sanity: TurnsFolded = %d, want 2", res.TurnsFolded)
	}

	// A post-compaction session must restart cleanly and keep working: a
	// further Prompt on the reloaded session must succeed.
	prov.turns = append(prov.turns, compactTurn("four", provider.Usage{InputTokens: 50, OutputTokens: 5}))
	if _, err := loaded.Prompt(context.Background(), "keep going"); err != nil {
		t.Fatalf("Prompt on reloaded post-compaction session: %v", err)
	}
}

// TestCompactCorruptRangeIsLoadError is the red-first test for §2's "Not
// found is treated as corruption" rule: a compact record naming a
// first_id/last_id pair that is not present in the accumulated history is
// an explicit LoadSession error, never a silent best-effort guess.
func TestCompactCorruptRangeIsLoadError(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	cfg := Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.persistCompactLocked("msg_does_not_exist", "msg_also_missing", 1, message.Message{
		ID: newID("msg"), Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "x"}},
	}, provider.Usage{})
	s.mu.Unlock()

	if _, err := LoadSession(cfg, s.ID); err == nil {
		t.Fatal("LoadSession succeeded on a corrupt compact record range, want an error")
	}
}

// nep5292FixtureLines is the exact reproduction journal from NEP-5292: three
// turns, the first turn's assistant message carrying a tool_call ("A") with
// no matching tool_result — the orphan message.ResolveOrphanToolCalls
// repairs at every LoadSession, in memory only. With keepTurns=2 the fold
// boundary lands exactly on that in-memory-only synthetic message.
const nep5292FixtureLines = `{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"task 1"}]}}
{"type":"message","message":{"id":"msg_2","role":"assistant","parts":[{"type":"tool_call","call_id":"A","name":"bash","arguments":{}}]}}
{"type":"message","message":{"id":"msg_3","role":"user","parts":[{"type":"text","text":"task 2"}]}}
{"type":"message","message":{"id":"msg_4","role":"assistant","parts":[{"type":"text","text":"done"}]}}
{"type":"message","message":{"id":"msg_5","role":"user","parts":[{"type":"text","text":"task 3"}]}}
{"type":"message","message":{"id":"msg_6","role":"assistant","parts":[{"type":"text","text":"done"}]}}
`

// writeNEP5292Fixture writes the reproduction journal above under id, with a
// session header line so it satisfies every other reader's expectations too.
func writeNEP5292Fixture(t *testing.T, dir, id string) {
	t.Helper()
	data := `{"type":"session","id":"` + id + `","created_at":"2025-01-02T03:04:05Z"}
` + nep5292FixtureLines
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// nep5292RawHistory is the exact message.Message values nep5292FixtureLines
// encodes, built directly (not by parsing JSON) for tests that need to feed
// them to spliceCompact without going through LoadSession at all — this is
// what ANY binary's scan loop, old or new, sees before
// message.ResolveOrphanToolCalls ever runs.
func nep5292RawHistory() []message.Message {
	return []message.Message{
		{ID: "msg_1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "task 1"}}},
		{ID: "msg_2", Role: message.RoleAssistant, Parts: message.Parts{&message.ToolCall{CallID: "A", Name: "bash", Arguments: json.RawMessage("{}")}}},
		{ID: "msg_3", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "task 2"}}},
		{ID: "msg_4", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}},
		{ID: "msg_5", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "task 3"}}},
		{ID: "msg_6", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}},
	}
}

// TestCompactNeverJournalsSyntheticOrphanID is the red-first test for Part A
// of NEP-5292's fix: Session.Compact must never persist a fold boundary ID
// that names a message.ResolveOrphanToolCalls synthetic repair message —
// that message exists only in this process's live memory (see
// engine/store.go's LoadSession, which applies the repair AFTER replay) and
// is never itself persisted, so a journal record naming one is corrupt on
// arrival: no future LoadSession will ever find it.
//
// It reproduces the exact mechanism from the issue: loading
// nep5292FixtureLines leaves an orphaned tool_call at msg_2, which
// LoadSession's ResolveOrphanToolCalls repair turns into a synthetic
// RoleTool message at live history index 2. A keepTurns=2 compact folds
// exactly turn 1 (indices 0-2), so the naive fold-end id would be that
// synthetic message's — this test asserts the FIXED Compact instead
// journals msg_2 (the nearest real, persisted message before it), and that
// the result reloads cleanly to the exact same kept history the live
// process already has.
func TestCompactNeverJournalsSyntheticOrphanID(t *testing.T) {
	dir := t.TempDir()
	id := "ses_5292000000000001"
	writeNEP5292Fixture(t, dir, id)

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	cfg := Config{
		SessionDir: dir,
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
	}

	s, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatalf("LoadSession = %v", err)
	}

	before := s.History()
	if len(before) != 7 {
		t.Fatalf("history length = %d, want 7 (6 raw messages + 1 synthetic repair)", len(before))
	}
	if !message.IsSyntheticOrphanID(before[2].ID) {
		t.Fatalf("history[2].ID = %q, want a synthetic orphan-repair id (the mechanism this test guards)", before[2].ID)
	}

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 2})
	if err != nil {
		t.Fatalf("Compact = %v", err)
	}
	if res.TurnsFolded != 1 {
		t.Fatalf("TurnsFolded = %d, want 1 (matches the issue's reproduction)", res.TurnsFolded)
	}
	if message.IsSyntheticOrphanID(res.LastID) {
		t.Fatalf("Compact result names a synthetic LastID: %q, want a real, persisted id", res.LastID)
	}
	if res.LastID != "msg_2" {
		t.Errorf("LastID = %q, want %q (the nearest real message before the synthetic one)", res.LastID, "msg_2")
	}
	liveAfter := s.History()

	// The journaled record itself must be clean, not merely what
	// LoadSession happens to recover afterward: read the raw on-disk line
	// directly, bypassing LoadSession's own heal path entirely.
	raw, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var last struct {
		Type    string `json:"type"`
		Compact *struct {
			FirstID string `json:"first_id"`
			LastID  string `json:"last_id"`
		} `json:"compact"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("unmarshal last journal line: %v", err)
	}
	if last.Type != recCompact || last.Compact == nil {
		t.Fatalf("last journal line = %q, want a %q record", lines[len(lines)-1], recCompact)
	}
	if message.IsSyntheticOrphanID(last.Compact.LastID) {
		t.Fatalf("on-disk compact record last_id is synthetic: %q", last.Compact.LastID)
	}

	// Equivalence (the issue's core claim, verified): the synthetic message
	// never existed in raw replayed history, so folding it live while
	// journaling the last real id before it produces IDENTICAL kept
	// history on a fresh reload.
	reloaded, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatalf("reload after compact: %v", err)
	}
	reloadedHistory := reloaded.History()
	if len(reloadedHistory) != len(liveAfter) {
		t.Fatalf("reloaded history = %d messages, want %d (live post-compact)", len(reloadedHistory), len(liveAfter))
	}
	for i := range liveAfter {
		if reloadedHistory[i].ID != liveAfter[i].ID ||
			reloadedHistory[i].Role != liveAfter[i].Role ||
			reloadedHistory[i].Parts.Text() != liveAfter[i].Parts.Text() {
			t.Errorf("reloaded history[%d] = %+v, want %+v", i, reloadedHistory[i], liveAfter[i])
		}
	}
}

// TestCompactNewRecordReplaysIdenticallyWithoutHealPath is the version-skew
// half of NEP-5292's fix: an OLD binary — one with no heal path at all,
// calling spliceCompact directly and never message.IsSyntheticOrphanID —
// must still replay a compact record written by the FIXED Compact
// correctly. This is what makes downgrading to an old binary after this fix
// safe: Part A never introduces a new record field or record type (the
// compactRecord shape is untouched), it only changes WHICH real, already-
// persisted message id LastID names. This test proves that value is always
// resolvable by the bare, unhealed spliceCompact function — simulating the
// old binary directly, never through LoadSession's own (new) heal path —
// and that doing so lands on the exact same kept history the live process
// already has.
//
// The named claim is "an old binary replays the JOURNALED record" — so this
// test must drive spliceCompact from the bytes persistCompactLocked actually
// wrote, not from CompactResult. CompactResult is populated independently
// (compact.go's return statement, not its persistCompactLocked call), so a
// bug that journals the wrong IDs while still returning the right
// CompactResult would slip past a version read from res. Read the raw
// on-disk line directly, the same way TestCompactNeverJournalsSyntheticOrphanID
// does, and use ITS FirstID/LastID/Summary.
func TestCompactNewRecordReplaysIdenticallyWithoutHealPath(t *testing.T) {
	dir := t.TempDir()
	id := "ses_5292000000000003"
	writeNEP5292Fixture(t, dir, id)

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	cfg := Config{
		SessionDir: dir,
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
	}
	s, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatalf("LoadSession = %v", err)
	}
	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 2}); err != nil {
		t.Fatalf("Compact = %v", err)
	}
	liveAfter := s.History()

	// Read the raw on-disk line directly, bypassing both CompactResult and
	// LoadSession's own heal path entirely — this is the exact record an old
	// binary would read from disk.
	raw, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var last record
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("unmarshal last journal line: %v", err)
	}
	if last.Type != recCompact || last.Compact == nil {
		t.Fatalf("last journal line = %q, want a %q record", lines[len(lines)-1], recCompact)
	}

	// The exact old-binary code path: plain spliceCompact against raw
	// pre-compact history, using the journaled ids/summary verbatim (read
	// from disk above, not from CompactResult), no heal function ever
	// called or even in scope.
	oldSpliced, err := spliceCompact(nep5292RawHistory(), last.Compact.FirstID, last.Compact.LastID, last.Compact.Summary)
	if err != nil {
		t.Fatalf("old-binary-equivalent spliceCompact = %v, want success (on-disk LastID must be a real, persisted id an old binary can find)", err)
	}
	oldFinal := message.ResolveOrphanToolCalls(oldSpliced)

	if len(oldFinal) != len(liveAfter) {
		t.Fatalf("old-binary-equivalent history = %d messages, want %d (live post-compact)", len(oldFinal), len(liveAfter))
	}
	for i := range liveAfter {
		if oldFinal[i].ID != liveAfter[i].ID ||
			oldFinal[i].Role != liveAfter[i].Role ||
			oldFinal[i].Parts.Text() != liveAfter[i].Parts.Text() {
			t.Errorf("old-binary-equivalent history[%d] = %+v, want %+v", i, oldFinal[i], liveAfter[i])
		}
	}
}

// TestLoadSessionHealsPhantomSyntheticCompactLastID is the red-first test
// for Part B of NEP-5292's fix: a journal ALREADY containing a phantom
// synthetic LastID (written by an unpatched build, before Part A existed)
// must still load — LoadSession re-derives the fold end from FirstID plus
// the record's own turns_folded count instead of failing outright. The
// journal here is written by hand, not produced by Session.Compact, to
// guarantee it exercises the phantom-id shape rather than whatever the
// (already fixed) live path would now produce.
func TestLoadSessionHealsPhantomSyntheticCompactLastID(t *testing.T) {
	dir := t.TempDir()
	id := "ses_5292000000000002"
	data := `{"type":"session","id":"` + id + `","created_at":"2025-01-02T03:04:05Z"}
` + nep5292FixtureLines +
		`{"type":"compact","compact":{"first_id":"msg_1","last_id":"synthetic-orphan-tool-result-1-A","turns_folded":1,"summary":{"id":"msg_summary","role":"user","parts":[{"type":"text","text":"[compacted summary of earlier conversation]\n\nthe gist"}]}}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("LoadSession = %v, want the phantom synthetic last_id healed instead of a hard error", err)
	}
	got := s.History()
	if len(got) != 5 {
		t.Fatalf("history length = %d, want 5 (summary + 4 kept messages: msg_3..msg_6)", len(got))
	}
	if got[0].ID != "msg_summary" {
		t.Errorf("history[0].ID = %q, want %q (the summary)", got[0].ID, "msg_summary")
	}
	if got[0].Role != message.RoleUser {
		t.Errorf("history[0].Role = %s, want RoleUser", got[0].Role)
	}
	wantKeptIDs := []string{"msg_3", "msg_4", "msg_5", "msg_6"}
	for i, want := range wantKeptIDs {
		if got[i+1].ID != want {
			t.Errorf("history[%d].ID = %q, want %q", i+1, got[i+1].ID, want)
		}
	}
}

// TestLoadSessionCompactPhantomLastIDFailsLoudlyWhenUnhealable is the
// explicit-error half of Part B: if the heal itself is impossible (here,
// first_id also does not name a real message), LoadSession must still fail
// loudly — never silently drop history — exactly as an un-healable corrupt
// range already does (see TestCompactCorruptRangeIsLoadError).
func TestLoadSessionCompactPhantomLastIDFailsLoudlyWhenUnhealable(t *testing.T) {
	dir := t.TempDir()
	id := "ses_5292000000000004"
	data := `{"type":"session","id":"` + id + `","created_at":"2025-01-02T03:04:05Z"}
` + nep5292FixtureLines +
		`{"type":"compact","compact":{"first_id":"msg_does_not_exist","last_id":"synthetic-orphan-tool-result-1-A","turns_folded":1,"summary":{"id":"msg_summary","role":"user","parts":[{"type":"text","text":"x"}]}}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadSession(Config{SessionDir: dir}, id); err == nil {
		t.Fatal("LoadSession succeeded on an unhealable phantom last_id (first_id also missing), want an error")
	}
}

// TestMaybeAutoCompactTriggersAndHysteresisPreventsThrash is the red-first
// test for §1's automatic trigger and §2's churn-guard hysteresis: crossing
// the threshold fires exactly one compaction; a second consecutive
// over-threshold turn (no intervening dip) does NOT re-fire; a dip below
// the threshold clears the guard so a later crossing can fire again.
func TestMaybeAutoCompactTriggersAndHysteresisPreventsThrash(t *testing.T) {
	over := provider.Usage{InputTokens: 900}
	under := provider.Usage{InputTokens: 100}

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", under), // call 1: no lastUsage yet, no auto-compact possible
		compactTurn("t2", over),  // call 2: lastUsage(t1)=under, no trigger
		compactSummaryTurn("gist-1", provider.Usage{InputTokens: 5}), // triggered before call 3 (lastUsage(t2)=over)
		compactTurn("t3", over),  // call 3's own turn (post first compaction)
		compactTurn("t4", under), // call 4: lastUsage(t3)=over but on cooldown, no trigger
		compactTurn("t5", over),  // call 5: lastUsage(t4)=under, cooldown clears, no trigger (not over)
		compactSummaryTurn("gist-2", provider.Usage{InputTokens: 5}), // triggered before call 6 (lastUsage(t5)=over)
		compactTurn("t6", under),                                     // call 6's own turn (post second compaction)
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})
	runTurns(t, s, 6)

	if got := s.CompactionCount(); got != 2 {
		t.Fatalf("CompactionCount = %d, want exactly 2 (hysteresis must have suppressed a third)", got)
	}
	if len(prov.requests) != 8 {
		t.Fatalf("provider calls = %d, want 8 (6 worker turns + 2 compaction summaries)", len(prov.requests))
	}
}

// TestMaybeAutoCompactDisabledByDefault is the red-first test for the
// opt-in gate: Config.ContextWindowTokens's zero value (a fresh Config)
// disables automatic compaction entirely, so a huge LastUsage never
// triggers it — no existing deployment changes behavior by upgrading (§5
// "Non-goals").
func TestMaybeAutoCompactDisabledByDefault(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", provider.Usage{InputTokens: 999_999}),
		compactTurn("t2", provider.Usage{InputTokens: 999_999}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)
	if got := s.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount = %d, want 0 (ContextWindowTokens unset)", got)
	}
	if len(prov.requests) != 2 {
		t.Errorf("provider calls = %d, want 2 (no compaction summary calls)", len(prov.requests))
	}
}

// TestIncidentRecoverableByCompaction is the red-first regression test for
// the production incident: a goal session died at 205102 tokens > 200000
// maximum ("invalid_request_error: prompt is too long") and was
// unrecoverable afterward. With ContextWindowTokens configured, the
// automatic trigger must fold history BEFORE the next request would repeat
// that identical, deterministic failure — turning the incident's shape into
// a recoverable one instead of a dead session.
func TestIncidentRecoverableByCompaction(t *testing.T) {
	// Three prior worker turns, the last one landing at the incident's exact
	// input-token count, followed by the automatic compaction's own
	// summarization call, then a worker turn that must now succeed instead
	// of repeating the "prompt is too long" failure.
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", provider.Usage{InputTokens: 50_000, OutputTokens: 500}),
		compactTurn("t2", provider.Usage{InputTokens: 120_000, OutputTokens: 500}),
		compactTurn("t3", provider.Usage{InputTokens: 205_102, OutputTokens: 500}), // the incident's exact figure
		compactSummaryTurn("summary of the first two turns", provider.Usage{InputTokens: 4_000, OutputTokens: 200}),
		compactTurn("t4", provider.Usage{InputTokens: 30_000, OutputTokens: 500}), // succeeds: history was trimmed first
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		ContextWindowTokens: 200_000, // the incident's exact maximum
		CompactionKeepTurns: 1,
	})
	runTurns(t, s, 3)

	last, ok := s.LastUsage()
	if !ok || last.InputTokens != 205_102 {
		t.Fatalf("LastUsage = %+v (ok=%v), want the incident's 205102 input tokens", last, ok)
	}
	if got := s.CompactionCount(); got != 0 {
		t.Fatalf("CompactionCount = %d before the 4th call, want 0", got)
	}

	// Pre-fix, this 4th call would resend the full, now-over-limit history
	// and die identically ("prompt is too long"). Post-fix, maybeAutoCompact
	// folds the oldest turns first, so the request this turn actually sends
	// is far smaller — the incident's exact failure mode never recurs.
	if _, err := s.Prompt(context.Background(), "keep going"); err != nil {
		t.Fatalf("Prompt on a session over the context-window threshold: %v (must be recoverable by compaction, not fatal)", err)
	}
	if got := s.CompactionCount(); got != 1 {
		t.Fatalf("CompactionCount after the 4th call = %d, want 1 (automatic compaction must have fired)", got)
	}
	finalReq := prov.requests[len(prov.requests)-1]
	if len(finalReq.Messages) >= 6 { // pre-compaction full history would have been >= 6 messages (3 turns)
		t.Errorf("final request carried %d messages, want a trimmed history (compaction folded the old turns)", len(finalReq.Messages))
	}
}

// goalCompactProvider serves the goal loop's worker turns, its independent
// tool-less evaluator, AND compaction's own tool-less summarization call —
// classifying by System content (the compaction system prompt is a unique
// marker no evaluator request ever carries) and, failing that, by whether
// Tools is empty (the evaluator's request, per goalProvider in
// goal_test.go). This is what "no goal.go changes needed" (docs/design/
// context-compaction.md §1) means in practice: PursueGoal drives everything
// through Prompt, so the automatic trigger fires mid-goal-loop with no
// special-casing at all — this test proves it end to end.
type goalCompactProvider struct {
	worker, eval, summary [][]provider.Event
	wi, ei, si            int
	requests              []*provider.Request
}

func (p *goalCompactProvider) Name() string { return "test" }

func (p *goalCompactProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.requests = append(p.requests, req)
	if len(req.System) == 1 && req.System[0] == compactionSystemPrompt {
		if p.si >= len(p.summary) {
			return nil, io.ErrUnexpectedEOF
		}
		ev := p.summary[p.si]
		p.si++
		return &scriptedStream{events: ev}, nil
	}
	if len(req.Tools) == 0 {
		if p.ei >= len(p.eval) {
			return &scriptedStream{}, nil
		}
		ev := p.eval[p.ei]
		p.ei++
		return &scriptedStream{events: ev}, nil
	}
	if p.wi >= len(p.worker) {
		return &scriptedStream{}, nil
	}
	ev := p.worker[p.wi]
	p.wi++
	return &scriptedStream{events: ev}, nil
}

// TestPursueGoalAutoCompactsMidLoop is the red-first test for §1's "no
// separate scheduler, no goal.go changes needed": a goal loop, driven
// entirely through the ordinary Prompt path, auto-compacts mid-loop exactly
// like a bare prompt_async session would, and still reaches its goal
// afterward.
func TestPursueGoalAutoCompactsMidLoop(t *testing.T) {
	prov := &goalCompactProvider{
		worker: [][]provider.Event{
			compactTurn("working turn 1", provider.Usage{InputTokens: 100}),
			compactTurn("working turn 2", provider.Usage{InputTokens: 900}), // over threshold
			compactTurn("working turn 3", provider.Usage{InputTokens: 100}), // proceeds post-compaction
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: keep going"),
			evalTurn("NOT MET: still going"),
			evalTurn("MET: done"),
		},
		summary: [][]provider.Event{
			compactSummaryTurn("gist of turn 1", provider.Usage{InputTokens: 20}),
		},
	}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		Instructions:        &InstructionsConfig{Disabled: true},
		SkillsDirs:          []string{},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})

	res, err := s.PursueGoal(context.Background(), "finish the thing", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if !res.Achieved {
		t.Fatalf("PursueGoal result = %+v, want Achieved", res)
	}
	if got := s.CompactionCount(); got != 1 {
		t.Fatalf("CompactionCount = %d, want exactly 1 (mid-loop automatic compaction)", got)
	}
}
