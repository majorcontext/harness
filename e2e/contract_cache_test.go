package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// cacheControl matches the marker that a request puts on its newest block. It is the last field of a block.
var cacheControl = regexp.MustCompile(`,"cache_control":\{[^{}]*\}`)

// searchOffered checks that the model requests of the row are enough to prove the prefix gate: at least three, the first with tools and the web_search tool.
type searchOffered struct{}

func (searchOffered) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.RawRequests()
	if len(reqs) < 3 {
		t.Fatalf("model requests = %d, want at least 3", len(reqs))
	}
	var body struct {
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(reqs[0], &body); err != nil || len(body.Tools) < 100 {
		t.Error("request 1 has no tools to compare")
	}
	if !strings.Contains(string(reqs[0]), "web_search") {
		t.Error("request 1 does not offer web_search, so the row proves nothing")
	}
}

// claudePromptStable checks that every spawn of the CLI got the same system prompt text and the same tool list.
type claudePromptStable struct{}

func (claudePromptStable) run(t *testing.T, r *run) {
	t.Helper()
	f, err := os.Open(claudeDriverOf(t, r).argvLog)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var prompts, tools []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var argv []string
		if err := json.Unmarshal(sc.Bytes(), &argv); err != nil {
			t.Fatal(err)
		}
		for _, flag := range []struct {
			name string
			into *[]string
		}{{"--append-system-prompt", &prompts}, {"--tools", &tools}} {
			if i := slices.Index(argv, flag.name); i >= 0 && i+1 < len(argv) {
				*flag.into = append(*flag.into, argv[i+1])
			}
		}
	}
	if len(prompts) < 3 || len(tools) != len(prompts) {
		t.Fatalf("spawns = %d prompts and %d tool lists, want at least 3 of each", len(prompts), len(tools))
	}
	for i := 1; i < len(prompts); i++ {
		if prompts[i] != prompts[0] || tools[i] != tools[0] {
			t.Errorf("spawn %d got another system prompt or tool list than spawn 1", i+1)
		}
	}
}

func TestContractWebSearchKeepsTheCachedPrefixStable(t *testing.T) {
	bash := harnesstest.ToolCall{ID: "toolu_b", Name: "bash", Input: map[string]any{"command": "echo hi"}}
	steps := []harnesstest.Step{
		{Name: "search", Match: harnesstest.LastUserText("find the release"), Reply: harnesstest.Reply{Text: "Go 1.27.2 is current.", WebSearch: goSearch}},
		{Name: "tool", Match: harnesstest.LastUserText("which source"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{bash}}},
		{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "go.dev"}},
		{Name: "third", Match: harnesstest.LastUserText("and again"), Reply: harnesstest.Reply{Text: "done"}},
	}
	turns := []action{create{as: "a"}, submit{as: "a", text: "find the release"}, waitIdle{as: "a"},
		submit{as: "a", text: "which source"}, waitIdle{as: "a"}, submit{as: "a", text: "and again"}, waitIdle{as: "a"}, searchOffered{}}
	runScenarios(t, []scenario{
		{name: "web_search_keeps_the_cached_prefix_of_anthropic_stable_across_calls_and_turns", config: searchOn, model: steps, actions: turns},
		{
			name:   "claudecode_web_search_keeps_the_system_prompt_stable_across_turns",
			driver: claudeLane{mode: "web_search", extra: searchOn}.newDriver,
			actions: []action{create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"}, submit{as: "a", text: "two"}, waitIdle{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"}, claudePromptStable{}},
		},
	})
}

func codexCacheRows() []codexScenario {
	bash := harnesstest.ToolCall{ID: "call_b", Name: "bash", Input: map[string]any{"command": "echo hi"}}
	steps := []harnesstest.Step{
		{Name: "search", Match: harnesstest.LastUserText("find the release"), Reply: harnesstest.Reply{Text: "Go 1.27.2 is current.", WebSearch: goSearch}},
		{Name: "tool", Match: harnesstest.LastUserText("which source"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{bash}}},
		{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "go.dev"}},
		{Name: "third", Match: harnesstest.LastUserText("and again"), Reply: harnesstest.Reply{Text: "done"}},
	}
	return []codexScenario{{scenario: scenario{
		name: "codex_web_search_keeps_the_cached_prefix_stable_across_calls_and_turns", config: searchOn, model: steps,
		actions: codexSession([]action{submit{as: "a", text: "find the release"}, waitIdle{as: "a"}, submit{as: "a", text: "which source"}, waitIdle{as: "a"},
			submit{as: "a", text: "and again"}, waitIdle{as: "a"}, searchOffered{}}),
	}}}
}
