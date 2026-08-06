package engine

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestMaybeAutoCompactFallsBackToSizeEstimateOnZeroUsage is the red-first
// regression test for the 2026-08-06 nimble-pizza field incident: a
// Bedrock-via-gateway route reported InputTokens=0, CacheReadTokens=0, and
// CacheWriteTokens=0 on EVERY turn of a 631-message session (OutputTokens
// was correct throughout — only the input side of that route's usage
// accounting was broken). maybeAutoCompact's threshold check sums exactly
// those three fields, so `promptTokens` was permanently 0 and `over` could
// never become true no matter how large history actually grew: automatic
// compaction could never fire on that route, and the session ran to a hard
// context overflow, which clears an active goal instead of parking it —
// unrecoverable.
//
// Pre-fix, this test fails: a completed turn with all-zero input usage
// never trips the threshold regardless of history size, so CompactionCount
// stays 0. Post-fix, estimatePromptTokensFromHistory's size-derived
// fallback (see compact.go) stands in for the missing accounting and the
// trigger fires exactly as it would have on a route with working usage
// accounting.
func TestMaybeAutoCompactFallsBackToSizeEstimateOnZeroUsage(t *testing.T) {
	zeroInputUsage := provider.Usage{OutputTokens: 5} // all input components zero
	bigText := strings.Repeat("x", 4000)              // 4000 bytes / 4 = 1000 estimated tokens alone

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", zeroInputUsage),                                           // call 1: no lastUsage yet, no trigger possible
		compactTurn(bigText, zeroInputUsage),                                        // call 2: lastUsage(t1) has zero usage, no trigger (history still small)
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5, OutputTokens: 5}), // the fallback-triggered compaction's own summarization call
		compactTurn("t3", zeroInputUsage),                                           // call 3: post-compaction, proceeds
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})
	runTurns(t, s, 3)

	if got := s.CompactionCount(); got != 1 {
		t.Fatalf("CompactionCount = %d, want 1: an all-zero-usage turn with a large enough history must still trigger automatic compaction via the size-derived fallback estimate", got)
	}

	// Real usage accounting must stay untouched by the estimate: it is used
	// for the threshold comparison only (see maybeAutoCompact's comment).
	last, ok := s.LastUsage()
	if !ok {
		t.Fatal("LastUsage: ok = false, want true")
	}
	if last.InputTokens != 0 || last.CacheReadTokens != 0 || last.CacheWriteTokens != 0 {
		t.Errorf("LastUsage = %+v, want all-zero input components preserved verbatim (the estimate must never be written into real accounting)", last)
	}
}

// TestMaybeAutoCompactZeroUsageBelowThresholdDoesNotFire is the inverse
// guard: an all-zero-usage turn whose actual history content is small must
// NOT trigger automatic compaction just because usage reporting is broken —
// the size-derived fallback estimate (see estimatePromptTokensFromHistory)
// must still respect the configured threshold, not fire unconditionally
// whenever usage is zero.
func TestMaybeAutoCompactZeroUsageBelowThresholdDoesNotFire(t *testing.T) {
	zeroInputUsage := provider.Usage{OutputTokens: 5} // all input components zero

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", zeroInputUsage),
		compactTurn("t2", zeroInputUsage),
		compactTurn("t3", zeroInputUsage),
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})
	runTurns(t, s, 3)

	if got := s.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount = %d, want 0: history is far below the configured threshold, so the size estimate must not fire just because usage reporting happens to be zero", got)
	}
	if len(prov.requests) != 3 {
		t.Errorf("provider calls = %d, want 3 (no compaction summary call — nothing should have triggered)", len(prov.requests))
	}
}
