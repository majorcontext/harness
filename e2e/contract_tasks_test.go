package e2e

import (
	"slices"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

const readerProfile = "---\nname: reader\ndescription: Reads.\ntools: ls\nmodel: anthropic/claude-haiku-4-5\n---\n\nOnly read files.\n"

func TestContractTaskProfiles(t *testing.T) {
	lead := "---\nname: lead\ndescription: Leads.\n---\n\nLead the team.\n"
	runScenarios(t, []scenario{
		{
			name:       "task_profile_sets_the_tools_model_and_prompt_of_the_child",
			concurrent: true,
			model:      delegation("reader", childDone),
			actions: slices.Concat([]action{writeFile{path: ".agents/reader.md", body: readerProfile}}, spawned,
				[]action{recordSystemLine{user: "child work", contains: "Only read files."}}),
		},
		{
			name:       "agent_defs_dirs_replace_the_default_profile_dir",
			concurrent: true,
			config:     map[string]any{"agent_defs_dirs": []string{"team"}},
			model:      delegation("lead", childDone),
			actions: slices.Concat([]action{
				writeFile{path: ".agents/reader.md", body: readerProfile},
				writeFile{path: "team/lead.md", body: lead},
			}, spawned, []action{recordSystemLine{user: "child work", contains: "Lead the team."}}),
		},
	})
}

func TestContractTaskRepeatedProfileNames(t *testing.T) {
	other := "---\nname: reader\ndescription: Reads again.\n---\n\nRead more.\n"
	attempt := []action{create{as: "a"}, submit{as: "a", text: "refuse it"}, waitIdle{as: "a"}}
	model := []harnesstest.Step{
		taskStep("refuse", userStarts("refuse"), fixed(spawn("reader", "x"))),
		{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
	}
	runScenarios(t, []scenario{
		{
			name:  "task_spawn_fails_on_an_agent_name_repeated_in_one_dir",
			model: model,
			actions: slices.Concat([]action{
				writeFile{path: ".agents/reader.md", body: readerProfile},
				writeFile{path: ".agents/reader_again.md", body: other},
			}, attempt),
		},
		{
			name:   "task_spawn_fails_on_an_agent_name_repeated_across_dirs",
			config: map[string]any{"agent_defs_dirs": []string{"team", ".agents"}},
			model:  model,
			actions: slices.Concat([]action{
				writeFile{path: ".agents/reader.md", body: readerProfile},
				writeFile{path: "team/reader.md", body: other},
			}, attempt),
		},
	})
}

func TestContractTaskReadOnlyProfiles(t *testing.T) {
	runScenarios(t, []scenario{{
		name:       "task_explore_and_plan_children_get_read_only_tools",
		concurrent: true,
		model: delegation("explore", childDone,
			taskStep("plan", userStarts("plan it"), fixed(spawn("plan", "plan work"))),
			harnesstest.Step{Name: "plan_child", Match: userStarts("plan work"), Reply: harnesstest.Reply{Text: "plan done", Block: true}},
			harnesstest.Step{Name: "plan_ack", Match: matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting"}}),
		actions: slices.Concat(spawned, []action{
			submit{as: "a", text: "plan it"},
			awaitRequests{n: 7},
			release{step: "plan_child"},
			awaitRequests{n: 8},
			waitIdle{as: "a"},
			bindChild{as: "planner", parent: "a", nth: 1, record: true},
		}),
	}})
}

func TestContractTaskRefusals(t *testing.T) {
	grand := harnesstest.ToolCall{ID: "toolu_grand", Name: "task", Input: spawn("general-purpose", "grand work")}
	runScenarios(t, []scenario{
		{
			name: "task_refusals",
			model: []harnesstest.Step{
				taskStep("refuse", userStarts("refuse"), fixed(spawn("nope", "x"), ftArgs("prompt", 5))),
				{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "refuse it"}, waitIdle{as: "a"}},
		},
		{
			name:       "task_refusal_past_max_task_depth",
			concurrent: true,
			driver:     limited("max_task_depth", "HARNESS_MAX_TASK_DEPTH", 1),
			model: delegation("general-purpose", harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{grand}},
				harnesstest.Step{Name: "kid_after", Match: matchAll(rootStarts("child work"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "child done"}}),
			actions: spawnedAfter(5),
		},
		{
			name:       "task_refusal_past_max_concurrent_tasks",
			concurrent: true,
			driver:     limited("max_concurrent_tasks", "HARNESS_MAX_CONCURRENT_TASKS", 1),
			model:      delegation("general-purpose", childRuns, taskStep("again", userStarts("again"), fixed(spawn("general-purpose", "second work")))),
			actions:    slices.Concat(spawnedRunning, []action{submit{as: "a", text: "again"}, waitIdle{as: "a"}}),
		},
	})
}

func TestContractTaskPluginProfile(t *testing.T) {
	mixed := "---\nname: mixed\ndescription: Echoes and reads.\ntools: fixture_echo, ls\n---\n\nUse both tools.\n"
	runScenarios(t, []scenario{{
		name:       "task_profile_keeps_the_plugin_tools_of_its_list",
		concurrent: true,
		config:     pluginConfig(t, nil),
		model: []harnesstest.Step{
			busyTaskStep("delegate", userStarts("delegate"), fixed(spawn("mixed", "child work"))),
			{Name: "child", Match: userStarts("child work"), Reply: childRuns, Repeat: true},
			{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
		},
		actions: []action{
			writeFile{path: ".agents/mixed.md", body: mixed},
			create{as: "a"},
			submit{as: "a", text: "delegate"},
			awaitRequests{n: 2},
			waitIdle{as: "a"},
		},
	}})
}

func TestContractTaskClaudeChild(t *testing.T) {
	onClaude := "---\nname: reader\ndescription: Reads.\ntools: ls\nmodel: claude-code/sonnet\n---\n\nOnly read files.\n"
	runScenarios(t, []scenario{{
		name:   "task_child_on_claude_code_gets_no_runtime_builtin",
		driver: claudeLane{mode: "hang_after_listing", listTools: true, initTools: "[]"}.newDriver,
		model: []harnesstest.Step{
			taskStep("delegate", userStarts("delegate"), fixed(spawn("reader", "child work"))),
			{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
			{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
		},
		actions: []action{
			writeFile{path: ".agents/reader.md", body: onClaude},
			create{as: "a", model: "anthropic/claude-fable-5"},
			submit{as: "a", text: "delegate"},
			awaitRequests{n: 2},
			waitIdle{as: "a"},
			bindChild{as: "kid", parent: "a", record: true},
			claudeAwaitText{as: "kid", text: "Working on it."},
			claudeInvocations{as: "kid"},
			claudeOfferedTools{as: "kid"},
			interrupt{as: "kid"},
			awaitRequests{n: 3},
			waitIdle{as: "a"},
		},
	}})
}
