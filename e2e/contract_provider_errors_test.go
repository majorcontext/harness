package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

type awaitCanceled struct{ step string }

func (a awaitCanceled) run(t *testing.T, r *run) {
	t.Helper()
	if !r.fake.AwaitCanceled(a.step, waitBound) {
		t.Fatalf("model request for step %q was not canceled within %s\nserve stderr:\n%s", a.step, waitBound, r.drv.Stderr())
	}
}

const usageLimitMessage = "You have reached your specified API usage limits. You will regain access on 2099-01-01 at 00:00 UTC."

func TestContractProviderErrors(t *testing.T) {
	ok := harnesstest.Step{Name: "ok", Reply: harnesstest.Reply{Text: "recovered"}}
	oneTurn := func(extra ...action) []action {
		return append([]action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}, extra...)
	}
	runScenarios(t, slices.Concat([]scenario{
		{
			// Retry-After 1 equals the fixed 1s backoff, so this row cannot tell them apart.
			name: "provider_429_then_ok",
			model: []harnesstest.Step{
				{Name: "limited", Reply: harnesstest.Reply{HTTPStatus: 429, RetryAfter: "1", ErrorMessage: "slow down"}},
				ok,
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name: "provider_5xx_then_ok",
			model: []harnesstest.Step{
				{Name: "broken", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke"}},
				ok,
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name: "context_overflow",
			model: []harnesstest.Step{
				{Name: "overflow", Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name: "provider_error_text_is_masked_and_bounded",
			model: []harnesstest.Step{
				{Name: "leak", Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "bad request, Authorization: Bearer sk-live-0123456789abcdef " + strings.Repeat("x", 400)}},
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 2},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name: "provider_usage_limit_fails_turn",
			model: []harnesstest.Step{
				{Name: "wall", Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: usageLimitMessage}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name:   "stream_stall",
			config: map[string]any{"stream_idle_timeout_s": 1},
			model: []harnesstest.Step{
				{Name: "stall", Reply: harnesstest.Reply{Text: "partial", Block: true}},
				ok,
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"},
				awaitCanceled{step: "stall"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	}, anthropicInjectedRows()))
}

func TestContractProviderMaxTokens(t *testing.T) {
	oneTurn := func(extra ...action) []action {
		return append([]action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}, extra...)
	}
	runScenarios(t, []scenario{
		{
			name: "max_tokens_cut_response_is_retried",
			model: []harnesstest.Step{
				{Name: "cut", Reply: harnesstest.Reply{Text: "first half ", StopReason: "max_tokens"}},
				{Name: "whole", Reply: harnesstest.Reply{Text: "whole answer"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name: "max_tokens_cut_tool_call_is_retried_without_running",
			model: []harnesstest.Step{
				{Name: "cut", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "write_file", Input: map[string]any{"path": "cut.txt", "content": "abc"}}}, StopReason: "max_tokens"}},
				{Name: "whole", Reply: harnesstest.Reply{Text: "whole answer"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name: "retried_call_with_a_tool_call_alone_completes_under_a_new_item",
			model: []harnesstest.Step{
				{Name: "cut", Reply: harnesstest.Reply{Text: "first half ", StopReason: "max_tokens"}},
				{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "ls", Input: map[string]any{"path": "."}}}}},
				{Name: "done", Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: []action{
				create{as: "a"},
				watchItems{as: "a"},
				submit{as: "a", text: "go"},
				waitIdle{as: "a"},
				itemFrames{as: "a"},
			},
		},
		{
			name:   "max_tokens_cut_response_fails_the_turn_after_stream_max_retries",
			config: map[string]any{"stream_max_retries": 1},
			model: []harnesstest.Step{
				{Name: "cut", Reply: harnesstest.Reply{Text: "first half ", StopReason: "max_tokens"}},
				{Name: "cut_again", Reply: harnesstest.Reply{Text: "first half ", StopReason: "max_tokens"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
	})
}

func TestContractProviderRetries(t *testing.T) {
	oneTurn := func(extra ...action) []action {
		return append([]action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}, extra...)
	}
	runScenarios(t, []scenario{
		{
			name:   "stream_retries_stop_after_stream_max_retries",
			config: map[string]any{"request_max_retries": 0, "stream_max_retries": 1},
			model: []harnesstest.Step{
				{Name: "broken", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke"}},
				{Name: "still_broken", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke again"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name: "interrupt_during_retry_backoff_ends_the_turn",
			model: []harnesstest.Step{
				{Name: "limited", Reply: harnesstest.Reply{HTTPStatus: 429, RetryAfter: "30", ErrorMessage: "slow down"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"},
				awaitRequests{n: 1},
				getSession{as: "a"},
				interrupt{as: "a"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractProviderRetryBudgets(t *testing.T) {
	limited := harnesstest.Reply{HTTPStatus: 429, RetryAfter: "0", ErrorMessage: "slow down"}
	oneTurn := func(extra ...action) []action {
		return append([]action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}, extra...)
	}
	runScenarios(t, []scenario{
		{
			name:   "provider_5xx_is_retried_by_the_request_before_the_turn_retries",
			config: map[string]any{"stream_max_retries": 0},
			model: []harnesstest.Step{
				{Name: "broken", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke"}},
				{Name: "broken_again", Reply: harnesstest.Reply{HTTPStatus: 502, ErrorMessage: "upstream broke again"}},
				{Name: "overloaded", Reply: harnesstest.Reply{HTTPStatus: 529, ErrorMessage: "overloaded"}},
				{Name: "ok", Reply: harnesstest.Reply{Text: "recovered"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name:   "request_and_stream_retries_multiply",
			config: map[string]any{"request_max_retries": 1, "stream_max_retries": 1},
			model: []harnesstest.Step{
				{Name: "a1", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke 1"}},
				{Name: "a2", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke 2"}},
				{Name: "b1", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke 3"}},
				{Name: "b2", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke 4"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name:    "provider_429_is_not_retried_by_the_request",
			config:  map[string]any{"request_max_retries": 4, "stream_max_retries": 0},
			model:   []harnesstest.Step{{Name: "limited", Reply: limited}},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name:    "provider_429_without_retry_after_is_not_retried",
			model:   []harnesstest.Step{{Name: "limited", Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: "slow down"}}},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name: "stream_retries_default_to_five",
			model: []harnesstest.Step{
				{Name: "l1", Reply: limited}, {Name: "l2", Reply: limited}, {Name: "l3", Reply: limited},
				{Name: "l4", Reply: limited}, {Name: "l5", Reply: limited},
				{Name: "ok", Reply: harnesstest.Reply{Text: "recovered"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name: "stream_retries_stop_after_the_default_of_five",
			model: []harnesstest.Step{
				{Name: "l1", Reply: limited}, {Name: "l2", Reply: limited}, {Name: "l3", Reply: limited},
				{Name: "l4", Reply: limited}, {Name: "l5", Reply: limited}, {Name: "l6", Reply: limited},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name: "provider_connection_failure_is_retried",
			model: []harnesstest.Step{
				{Name: "hangup", Reply: harnesstest.Reply{Hangup: true}},
				{Name: "ok", Reply: harnesstest.Reply{Text: "recovered"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
	})
}

func TestContractProviderRefusals(t *testing.T) {
	oneTurn := func(extra ...action) []action {
		return append([]action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}, extra...)
	}
	runScenarios(t, []scenario{
		{
			name: "provider_refusal_is_retried_as_a_content_filter",
			model: []harnesstest.Step{
				{Name: "refused", Reply: harnesstest.Reply{Text: "I cannot ", StopReason: "refusal"}},
				{Name: "ok", Reply: harnesstest.Reply{Text: "recovered"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
		{
			name:   "provider_refusal_fails_the_turn_after_stream_max_retries",
			config: map[string]any{"stream_max_retries": 1},
			model: []harnesstest.Step{
				{Name: "refused", Reply: harnesstest.Reply{Text: "I cannot ", StopReason: "refusal"}},
				{Name: "refused_again", Reply: harnesstest.Reply{Text: "I cannot ", StopReason: "refusal"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
	})
}
