package engine

import (
	"context"
	"fmt"
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
	wantFirstID := before[0].ID   // turn 1's leading RoleUser message
	wantLastID := before[3].ID    // last message before turn 3's leading RoleUser message

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
		compactTurn("t6", under), // call 6's own turn (post second compaction)
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
