package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

// awaitFile waits for a file of the work dir that a tool creates, which marks
// the tool as running.
type awaitFile struct{ path string }

func (a awaitFile) run(t *testing.T, r *run) {
	t.Helper()
	path := filepath.Join(r.drv.Workdir(), a.path)
	testpoll.Until(t, waitBound, "tool never created "+a.path, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, 15*time.Millisecond)
}

func TestContractQueue(t *testing.T) {
	slow := func(text string) harnesstest.Step {
		return harnesstest.Step{
			Name:  "slow",
			Match: harnesstest.LastUserText("first"),
			Reply: harnesstest.Reply{Text: text, Block: true},
		}
	}
	second := harnesstest.Step{
		Name:  "second",
		Match: harnesstest.LastUserText("second"),
		Reply: harnesstest.Reply{Text: "2"},
	}
	runScenarios(t, []scenario{
		{
			name:  "enqueue_while_busy_runs_after",
			model: []harnesstest.Step{slow("1"), second},
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
			model: []harnesstest.Step{slow("unfinished"), second},
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
			name: "interrupt_cuts_a_running_tool_then_queue_continues",
			model: []harnesstest.Step{
				{Name: "call", Match: harnesstest.LastUserText("first"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_wait", Name: "bash", Input: map[string]any{"command": "touch tool-running; sleep 30"}},
				}}},
				{Name: "second", Match: harnesstest.LastUserText("second"), Reply: harnesstest.Reply{Text: "2"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitFile{path: "tool-running"},
				enqueue{as: "a", text: "second"},
				interrupt{as: "a"},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "input_receipts_and_conflicts",
			model: []harnesstest.Step{slow("1"), second},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				postInput{as: "a", text: "second"},
				repeatInput{as: "a", text: "second"},
				repeatInput{as: "a", text: "other"},
				steerOtherTurn{as: "a", text: "late"},
				release{step: "slow"},
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

func TestContractToolBoundaryDelivery(t *testing.T) {
	row := func(name, text string, send func(as, text string) action) scenario {
		return scenario{
			name: name,
			model: []harnesstest.Step{
				{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_" + text, Name: "bash", Input: map[string]any{"command": "echo tool"}},
				}}},
				{Name: text, Match: harnesstest.LastUserText(text), Reply: harnesstest.Reply{Text: "done"}},
				{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "ran"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run"},
				awaitRequests{n: 1},
				send("a", text),
				release{step: "call"},
				waitIdle{as: "a"},
			},
		}
	}
	runScenarios(t, []scenario{
		row("steer_joins_the_turn_at_the_tool_boundary", "steer", func(as, text string) action { return submit{as: as, text: text} }),
		row("enqueue_joins_the_turn_at_the_tool_boundary", "enqueued", func(as, text string) action { return enqueue{as: as, text: text} }),
	})
}
