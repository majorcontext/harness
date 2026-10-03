package harness_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

func TestModelsListsTheConfiguredProviders(t *testing.T) {
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: config.Config{Providers: map[string]config.Provider{
		"codex":       {Type: config.TypeOpenAI},
		"claude-code": {Type: config.TypeClaudeCodeCLI},
		"work":        {Type: config.TypeOpenAI},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	var got []string
	for _, m := range r.Models() {
		got = append(got, fmt.Sprintf("%s %s %d", m.ID, m.Provider, m.ContextWindow))
		if _, err := r.Create(bg, protocol.CreateSession{Model: m.ID}); err != nil {
			t.Errorf("Create(%s): %v", m.ID, err)
		}
	}
	want := []string{"claude-code/fable claude-code 0", "claude-code/haiku claude-code 0", "claude-code/opus claude-code 0",
		"claude-code/sonnet claude-code 0", "codex/gpt-6-astra codex 1050000", "codex/gpt-6-luna codex 1050000", "codex/gpt-6-sol codex 1050000"}
	if !slices.Equal(got, want) {
		t.Errorf("Models =\n%q\nwant\n%q", got, want)
	}
}
