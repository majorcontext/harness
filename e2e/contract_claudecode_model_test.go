package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

var deferredModelRows = map[string]runtimeRow{
	"claudecode_deferred_model_applies_after_failure":       sameAsServe(),
	"claudecode_deferred_model_applies_after_cancellation":  sameAsServe(),
	"claudecode_deferred_model_runs_the_next_turn":          sameAsServe(),
	"claudecode_deferred_model_replaces_and_clears_pending": sameAsServe(),
	"claudecode_deferred_model_rejects_invalid_targets":     sameAsServe(),
}

type requireSessionModel struct{ as, model string }

func (a requireSessionModel) run(t *testing.T, r *run) {
	res := r.drv.GetSession(t, r.id(t, a.as))
	body, _ := res.Body.(map[string]any)
	if body["model"] != a.model {
		t.Fatalf("session model = %v, want %s", body["model"], a.model)
	}
	r.record(t, "session_model", a.as, callResult{Status: res.Status, Body: body["model"]})
}

func TestContractClaudeCodeDeferredModel(t *testing.T) {
	set := func(ref string) []turnCall {
		return []turnCall{{"model", map[string]any{"action": "set", "model": ref}}}
	}
	status := []turnCall{{"model", map[string]any{"action": "status"}}}
	runScenarios(t, []scenario{{
		name:    "claudecode_deferred_model_applies_after_failure",
		driver:  claudeLane{mode: "turn_calls_error", turnCalls: [][]turnCall{set("anthropic/claude-fable-5")}}.newDriver,
		actions: withActions(claudeOneTurn, requireSessionModel{as: "a", model: "anthropic/claude-fable-5"}, claudeTurnEnds{as: "a"}),
	}, {
		name:   "claudecode_deferred_model_applies_after_cancellation",
		driver: claudeLane{mode: "turn_calls_hang", turnCalls: [][]turnCall{set("anthropic/claude-fable-5")}}.newDriver,
		actions: []action{create{as: "a"}, submit{as: "a", text: "run it"}, claudeAwaitText{as: "a", text: "Pending model set."},
			requireSessionModel{as: "a", model: "claude-code/sonnet"}, interrupt{as: "a"}, waitIdle{as: "a"},
			requireSessionModel{as: "a", model: "anthropic/claude-fable-5"}, claudeTurnEnds{as: "a"}},
	}, {
		name: "claudecode_deferred_model_runs_the_next_turn",
		driver: claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{set("quick"), status},
			extra: map[string]any{"aliases": map[string]any{"quick": "anthropic/claude-fable-5"}}}.newDriver,
		model: []harnesstest.Step{{Name: "new model", Match: harnesstest.LastUserText("next"), Reply: harnesstest.Reply{Text: "new model reply"}}},
		actions: withActions(claudeOneTurn, requireSessionModel{as: "a", model: "anthropic/claude-fable-5"},
			submit{as: "a", text: "next"}, waitIdle{as: "a"}, claudeInvocations{as: "a"}, claudeJournalEvents{as: "a", prefix: "settings."}),
	}, {
		name: "claudecode_deferred_model_replaces_and_clears_pending",
		driver: claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{
			set("anthropic/claude-fable-5"), set("claude-code/opus"), status, set("claude-code/sonnet"), status,
		}}.newDriver,
		actions: withActions(claudeOneTurn, requireSessionModel{as: "a", model: "claude-code/sonnet"}, claudeJournalEvents{as: "a", prefix: "settings."}),
	}, {
		name: "claudecode_deferred_model_rejects_invalid_targets",
		driver: claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{
			set("bare"), set("nope/model"), status, set("claude-code/opus"), set("nope/model"), status,
		}}.newDriver,
		actions: withActions(claudeOneTurn, requireSessionModel{as: "a", model: "claude-code/opus"}, claudeJournalEvents{as: "a", prefix: "settings."}),
	}})
}
