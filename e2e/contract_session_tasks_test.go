package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractTaskProfileNarrowing(t *testing.T) {
	profile := func(name, tools string) action {
		return writeFile{path: ".agents/" + name + ".md", body: "---\nname: " + name + "\ndescription: Narrows.\ntools: " + tools + "\n---\n\nWork as " + name + ".\n"}
	}
	// midTurn spawns one grandchild and holds its turn until the scenario
	// releases the step, so the grandchildren start in a fixed order.
	midTurn := func(name string, spawned int, agent, prompt string) harnesstest.Step {
		match := matchAll(rootStarts("child work"), harnesstest.LastToolResult("task"), assistantTurns(spawned))
		return harnesstest.Step{Name: name, Match: match, Reply: harnesstest.Reply{Text: "spawning " + agent, Block: true, ToolCalls: []harnesstest.ToolCall{
			{ID: "toolu_" + agent, Name: "task", Input: spawn(agent, prompt)},
		}}}
	}
	grandchild := func(name, prompt string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: userStarts(prompt), Reply: childRuns}
	}
	first := harnesstest.ToolCall{ID: "toolu_leaf", Name: "task", Input: spawn("leaf", "leaf work")}
	runScenarios(t, []scenario{{
		name:       "task_profile_of_a_grandchild_keeps_the_tools_its_parent_allows",
		concurrent: true,
		model: delegation("mid", harnesstest.Reply{Text: "spawning leaf", Block: true, ToolCalls: []harnesstest.ToolCall{first}},
			grandchild("leaf", "leaf work"), grandchild("open", "open work"), grandchild("bare", "bare work"),
			midTurn("mid_open", 1, "general-purpose", "open work"), midTurn("mid_bare", 2, "bare", "bare work"),
			harnesstest.Step{Name: "mid_ack", Match: matchAll(rootStarts("child work"), harnesstest.LastToolResult("task"), assistantTurns(3)), Reply: harnesstest.Reply{Text: "waiting"}}),
		actions: []action{
			profile("mid", "task, ls, grep"), profile("leaf", "ls, bash"), profile("bare", "bash"),
			create{as: "a"},
			submit{as: "a", text: "delegate"},
			awaitRequests{n: 3},
			release{step: "child"},
			awaitRequests{n: 5},
			release{step: "mid_open"},
			awaitRequests{n: 7},
			release{step: "mid_bare"},
			awaitRequests{n: 10},
			waitIdle{as: "a"},
		},
	}})
}
