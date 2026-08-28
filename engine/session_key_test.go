package engine

import (
	"context"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestPromptSetsSessionKeyOnRequest is the production-path proof for the
// main turn stream: Session.Prompt (via streamTurn) must set
// provider.Request.SessionKey to the session's own ID on every request it
// builds, so an adapter that forwards it (openaicompat's "user" field) can
// pin a session's requests to one provider replica for prompt-cache
// affinity. This drives the same Session.Prompt entry point production
// calls, not a hand-built Request (see the root AGENTS.md "Testing"
// section).
func TestPromptSetsSessionKeyOnRequest(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(prov.requests))
	}
	if got := prov.requests[0].SessionKey; got != s.ID {
		t.Errorf("SessionKey = %q, want session ID %q", got, s.ID)
	}
}

// TestGoalEvaluatorSetsSessionKeyOnRequest proves the goal evaluator call
// (runEvaluator, engine/goal.go) carries the session's ID too, alongside
// the worker turn request. Both requests land in goalProvider.requests
// (see goal_test.go's Stream, which appends every request regardless of
// worker/evaluator side).
func TestGoalEvaluatorSetsSessionKeyOnRequest(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProvider{
		worker: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "done"})},
		eval:   [][]provider.Event{evalTurn("MET: complete")},
	}
	s := goalSession(t, prov, dir)

	if _, err := s.PursueGoal(context.Background(), "finish the task", GoalOptions{Evaluator: evalModel}); err != nil {
		t.Fatal(err)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (worker + evaluator)", len(prov.requests))
	}
	for i, req := range prov.requests {
		if req.SessionKey != s.ID {
			t.Errorf("request %d: SessionKey = %q, want session ID %q", i, req.SessionKey, s.ID)
		}
	}
}

// TestCompactionSummarizerSetsSessionKeyOnRequest proves the compaction
// summarizer call (runCompactionSummary, engine/compact.go) carries the
// session's ID too.
func TestCompactionSummarizerSetsSessionKeyOnRequest(t *testing.T) {
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

	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatal(err)
	}
	if len(prov.requests) != 4 {
		t.Fatalf("requests = %d, want 4 (3 turns + 1 summary)", len(prov.requests))
	}
	// The last request is the summarizer call.
	summaryReq := prov.requests[len(prov.requests)-1]
	if summaryReq.SessionKey != s.ID {
		t.Errorf("summarizer SessionKey = %q, want session ID %q", summaryReq.SessionKey, s.ID)
	}
}
