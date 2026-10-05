package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractSessionFailedTurns(t *testing.T) {
	failing := func(name, user string, rep harnesstest.Reply) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: rep}
	}
	slowDown := harnesstest.Reply{HTTPStatus: 429, RetryAfter: "1", ErrorMessage: "slow down"}
	badRequest := harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "bad request"}
	text := func(user string) harnesstest.Step {
		return harnesstest.Step{Name: user, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: "re " + user}, Repeat: true}
	}
	runScenarios(t, []scenario{
		{
			name: "provider_usage_limit_fails_the_turn_and_holds_the_queue",
			model: []harnesstest.Step{
				failing("limited", "first", slowDown),
				failing("wall", "first", harnesstest.Reply{HTTPStatus: 429, ErrorMessage: usageLimitMessage}),
				text("two"), text("three"),
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueueNext{as: "a", text: "two"},
				waitIdle{as: "a"},
				getSession{as: "a"},
				restart{},
				getSession{as: "a"},
				submit{as: "a", text: "three"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name: "a_failed_turn_runs_the_next_queued_input",
			model: []harnesstest.Step{
				failing("limited", "first", slowDown), failing("bad", "first", badRequest), text("two"),
				failing("limited_again", "again", slowDown), failing("bad_again", "again", badRequest), text("last"),
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueueNext{as: "a", text: "two"},
				waitIdle{as: "a"},
				getSession{as: "a"},
				submit{as: "a", text: "again"},
				awaitRequests{n: 4},
				enqueueNext{as: "a", text: "last"},
				restart{},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractSessionUsage(t *testing.T) {
	used := func(name, user string, in int) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: "re " + user, Usage: harnesstest.Usage{Input: in, Output: in}}}
	}
	runScenarios(t, []scenario{{
		name:   "session_usage_counts_every_model_call_but_the_evaluation",
		config: map[string]any{"compaction_keep_turns": 1},
		model: []harnesstest.Step{
			used("one", "one", 1), used("two", "two", 1),
			{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist", Usage: harnesstest.Usage{Input: 20, Output: 20}}},
			used("goal", "say done", 1),
			{Name: "judge", Match: isEvaluator, Reply: harnesstest.Reply{Text: "MET: ok", Usage: harnesstest.Usage{Input: 300, Output: 300}}},
		},
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "one"}, waitIdle{as: "a"},
			submit{as: "a", text: "two"}, waitIdle{as: "a"},
			compact{as: "a"},
			setGoal{as: "a", condition: "say done"},
			waitIdle{as: "a"},
			getSession{as: "a"},
		},
	}, {
		name:   "a_failed_summary_keeps_its_usage_in_the_session",
		config: map[string]any{"context_window_tokens": 1000, "compaction_keep_turns": 1},
		model: []harnesstest.Step{
			used("one", "one", 1), used("two", "two", 900),
			{Name: "empty_summary", Match: summaryRequest, Reply: harnesstest.Reply{Usage: harnesstest.Usage{Input: 20, Output: 20}}},
			used("three", "three", 1),
			used("alpha", "alpha", 1), used("bravo", "bravo", 1),
			{Name: "overflow", Match: harnesstest.LastUserText("charlie"), Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}},
			{Name: "empty_summary_in_turn", Match: summaryRequest, Reply: harnesstest.Reply{Usage: harnesstest.Usage{Input: 7000, Output: 7000}}, Repeat: true},
		},
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "one"}, waitIdle{as: "a"},
			submit{as: "a", text: "two"}, waitIdle{as: "a"},
			submit{as: "a", text: "three"}, waitIdle{as: "a"},
			getSession{as: "a"},
			create{as: "b"},
			submit{as: "b", text: "alpha"}, waitIdle{as: "b"},
			submit{as: "b", text: "bravo"}, waitIdle{as: "b"},
			submit{as: "b", text: "charlie"}, waitIdle{as: "b"},
			getSession{as: "b"},
		},
	}})
}
