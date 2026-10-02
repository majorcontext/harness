package e2e

import (
	"testing"

	"github.com/majorcontext/harness/internal/fakemodel"
)

func TestContractDurability(t *testing.T) {
	text := func(name, user, reply string) fakemodel.Step {
		return fakemodel.Step{Name: name, Match: fakemodel.LastUserText(user), Reply: fakemodel.Reply{Text: reply}}
	}
	slow := fakemodel.Step{Name: "slow", Match: fakemodel.LastUserText("first"), Reply: fakemodel.Reply{Text: "partial", Block: true}}
	runScenarios(t, []scenario{
		{
			name:  "kill_mid_turn_then_continue",
			model: []fakemodel.Step{slow, text("again", "again", "ok")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				restart{kill: true},
				submit{as: "a", text: "again"},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "history_survives_clean_restart",
			model: []fakemodel.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"},
				waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "two"},
				waitIdle{as: "a"},
			},
		},
		{
			// Known defect, pinned on purpose: after a SIGKILL restart the refolded
			// queue entry is not dispatched.
			name:  "queued_input_survives_kill",
			model: []fakemodel.Step{slow},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				restart{kill: true},
				expectQueued{as: "a", texts: []string{"second"}},
			},
		},
	})
}
