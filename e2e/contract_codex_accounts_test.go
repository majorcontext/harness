package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

type requireCodexPrewarms struct{ minimum int }

func (requireCodexPrewarms) run(t *testing.T, _ *run) {
	t.Fatal("Codex prewarm check requires the Codex lane")
}
func (a requireCodexPrewarms) runWire(t *testing.T, _ *run, o *harnesstest.OpenAI) {
	count := 0
	for _, event := range o.WireEvents() {
		if event.Event == "prewarm" {
			count++
		}
	}
	if count < a.minimum {
		t.Fatalf("prewarm events = %d, want at least %d", count, a.minimum)
	}
}

func TestContractCodexSessionAccounts(t *testing.T) {
	spawnChild := spawn("general-purpose", "codex child work")
	spawnChild["accounts"] = map[string]string{"codex": "acct_child"}
	runCodexScenarios(t, []codexScenario{{
		scenario: scenario{
			name:       "codex_cold_send_applies_account_before_warm",
			concurrent: true,
			model: []harnesstest.Step{
				{Name: "spawn", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_spawn", Name: "task", Input: spawnChild}}}},
				{Name: "child", Match: harnesstest.LastUserText("codex child work"), Reply: harnesstest.Reply{Text: "codex child done"}},
				taskStep("send", harnesstest.LastUserText("resume"), onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("send", kid, "prompt", "codex child resume", "accounts", map[string]string{"codex": "acct_resumed"})}
				})),
				{Name: "child_resumed", Match: harnesstest.LastUserText("codex child resume"), Reply: harnesstest.Reply{Text: "codex child resumed"}},
				{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "delegate"}, waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a"}, waitIdle{as: "kid"}, waitIdle{as: "a"}, getSessionUsage{as: "kid", account: "acct_child"},
				restart{kill: true}, submit{as: "a", text: "resume"}, waitIdle{as: "a"}, waitIdle{as: "kid"}, waitIdle{as: "a"},
				getSessionUsage{as: "kid", account: "acct_resumed"}, requireCodexPrewarms{minimum: 4},
			},
		},
		opts: harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{
			"child":         {RateLimits: &harnesstest.RateLimits{Plan: "plus", Primary: &harnesstest.RateWindow{UsedPercent: 21, WindowMinutes: 300, ResetAt: 1900000000}}},
			"child_resumed": {RateLimits: &harnesstest.RateLimits{Plan: "plus", Primary: &harnesstest.RateWindow{UsedPercent: 22, WindowMinutes: 300, ResetAt: 1900000000}}},
		}},
		accountProxy:          true,
		websocket:             true,
		expectedProxyAccounts: []string{"acct_child", "acct_resumed"},
		expectedProxyCounts:   map[string]int{"acct_child": 1, "acct_resumed": 1},
	}})
}
