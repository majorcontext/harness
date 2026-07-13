package engine

import (
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestMaybeAutoCompactCountsCachedPromptTokens encodes the PR#74 review
// finding, shaped like the original 205,102-token incident UNDER PROMPT
// CACHING: harness injects cache_control by default, so on a warm session
// the Anthropic adapter reports the bulk of the prompt in CacheReadTokens
// (and new prefix growth in CacheWriteTokens) while InputTokens stays
// small. A threshold check reading InputTokens alone never fires in
// exactly the production shape auto-compaction exists for. The prompt
// size is the SUM of the three input components.
func TestMaybeAutoCompactCountsCachedPromptTokens(t *testing.T) {
	// The incident, cached: ~197k of prompt in cache reads/writes, a few
	// thousand uncached. Window 200k, default threshold — must trigger.
	warmOver := provider.Usage{InputTokens: 3_102, CacheReadTokens: 190_000, CacheWriteTokens: 12_000}
	small := provider.Usage{InputTokens: 100}

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", small),                                   // call 1: no lastUsage yet
		compactTurn("t2", warmOver),                                // call 2: lastUsage(t1)=small, no trigger
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5}), // must be consumed by the trigger before call 3
		compactTurn("t3", small),
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		ContextWindowTokens: 200_000,
		CompactionKeepTurns: 1,
	})
	runTurns(t, s, 3)

	if got := s.CompactionCount(); got != 1 {
		t.Fatalf("CompactionCount = %d, want 1: a warm-cached %d-token prompt (input %d + cache read %d + cache write %d) crossed the threshold but did not trigger",
			got, warmOver.InputTokens+warmOver.CacheReadTokens+warmOver.CacheWriteTokens,
			warmOver.InputTokens, warmOver.CacheReadTokens, warmOver.CacheWriteTokens)
	}
}
