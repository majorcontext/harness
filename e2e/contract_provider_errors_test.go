package e2e

import (
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

func TestContractProviderErrors(t *testing.T) {
	ok := harnesstest.Step{Name: "ok", Reply: harnesstest.Reply{Text: "recovered"}}
	oneTurn := func(extra ...action) []action {
		return append([]action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}, extra...)
	}
	runScenarios(t, []scenario{
		{
			// Possible defect: the retry waits a fixed 1s and ignores Retry-After.
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
		{
			name: "max_tokens_continuation",
			model: []harnesstest.Step{
				{Name: "cut", Reply: harnesstest.Reply{Text: "first half ", StopReason: "max_tokens"}},
				{Name: "rest", Reply: harnesstest.Reply{Text: "second half"}},
			},
			actions: oneTurn(getSession{as: "a"}),
		},
	})
}
