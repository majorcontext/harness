package e2e

import (
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/store"
)

type checkpointKinds struct {
	as    string
	kinds []string
}

func (a checkpointKinds) run(t *testing.T, r *run) {
	t.Helper()
	st := store.NewDiskStore(r.drv.(*runtimeDriver).store)
	seen := map[string]bool{}
	for event, err := range harness.ReadEvents(t.Context(), st, r.id(t, a.as), 0) {
		if err != nil {
			t.Fatal(err)
		}
		seen[event.Kind] = true
	}
	for _, kind := range a.kinds {
		if !seen[kind] {
			t.Errorf("checkpoint history lacks %s", kind)
		}
	}
}

func TestContractCheckpointHistory(t *testing.T) {
	turn := func(text string) harnesstest.Step {
		return harnesstest.Step{Name: text, Match: harnesstest.LastUserText(text), Reply: harnesstest.Reply{Text: "reply " + text}}
	}
	runScenarios(t, []scenario{{
		name: "checkpoint_compaction_and_command_history", prefixBreaks: compactionBreak(),
		config: map[string]any{"compaction_keep_turns": 1},
		model:  []harnesstest.Step{turn("one"), turn("two"), {Name: "summary", Reply: harnesstest.Reply{Text: "gist"}}, turn("three")},
		actions: []action{create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"},
			command{as: "a", text: "/thinking high"}, awaitCommands{as: "a"},
			submit{as: "a", text: "two"}, waitIdle{as: "a"}, command{as: "a", text: "/compact 1"}, awaitCommands{as: "a"},
			submit{as: "a", text: "three"}, waitIdle{as: "a"}, restart{}, checkpointKinds{as: "a", kinds: []string{"compaction.applied", "command.recorded"}}, checkpointView{as: "a"}},
	}, {
		name: "checkpoint_question_answer_history", driver: askLane(true), model: askSteps(),
		actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"},
			answerQuestion{as: "a", callID: "toolu_ask", answers: map[string]string{"Which database?": "SQLite"}},
			waitIdle{as: "a"}, restart{}, checkpointKinds{as: "a", kinds: []string{"request.opened", "request.resolved"}}, checkpointView{as: "a"}},
	}, {
		name: "checkpoint_question_dismiss_history", driver: askLane(true), model: askSteps(),
		actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"},
			dismissQuestion{as: "a", callID: "toolu_ask"}, submit{as: "a", text: "go on"}, waitIdle{as: "a"},
			restart{}, checkpointKinds{as: "a", kinds: []string{"request.opened", "request.resolved"}}, checkpointView{as: "a"}},
	}, {
		name: "checkpoint_promoted_inputs_history",
		model: []harnesstest.Step{
			{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{{ID: "toolu_queued", Name: "bash", Input: map[string]any{"command": "echo tool"}}}}},
			{Name: "after", Reply: harnesstest.Reply{Text: "done"}, Repeat: true},
		},
		actions: []action{create{as: "a"}, submit{as: "a", text: "run"}, awaitRequests{n: 1},
			enqueue{as: "a", text: "queued one"}, enqueue{as: "a", text: "queued two"}, release{step: "call"},
			waitIdle{as: "a"}, restart{}, checkpointKinds{as: "a", kinds: []string{"input.promoted"}}, checkpointView{as: "a"}},
	}})
}
