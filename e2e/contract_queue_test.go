package e2e

import (
	"testing"

	"github.com/majorcontext/harness/internal/fakemodel"
)

func TestContractQueue(t *testing.T) {
	slow := func(text string) fakemodel.Step {
		return fakemodel.Step{
			Name:  "slow",
			Match: fakemodel.LastUserText("first"),
			Reply: fakemodel.Reply{Text: text, Block: true},
		}
	}
	second := fakemodel.Step{
		Name:  "second",
		Match: fakemodel.LastUserText("second"),
		Reply: fakemodel.Reply{Text: "2"},
	}
	runScenarios(t, []scenario{
		{
			name:  "enqueue_while_busy_runs_after",
			model: []fakemodel.Step{slow("1"), second},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				release{step: "slow"},
				waitIdle{as: "a"},
			},
		},
		{
			// Known defect, pinned on purpose: interrupt drops the partial assistant text.
			name:  "interrupt_drops_unfinished_text_then_queue_continues",
			model: []fakemodel.Step{slow("unfinished"), second},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				interrupt{as: "a"},
				waitIdle{as: "a"},
			},
		},
		{
			name: "interrupt_idle_is_noop",
			actions: []action{
				create{as: "a"},
				interrupt{as: "a"},
				waitIdle{as: "a"},
			},
		},
	})
}
