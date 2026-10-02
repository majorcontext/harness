package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

const fixtureSegment = "FIXTURE-SYSTEM-SEGMENT"

// pluginFixtureBin builds harnesstest/pluginfixture once next to the harness
// binary, so the same cleanup removes both.
var pluginFixtureBin = sync.OnceValues(func() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(filepath.Dir(harnessBin), "pluginfixture")
	cmd := exec.Command("go", "build", "-o", bin, "./harnesstest/pluginfixture")
	cmd.Dir = filepath.Dir(wd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build pluginfixture: %v\n%s", err, out)
	}
	return bin, nil
})

func pluginConfig(t *testing.T) map[string]any {
	t.Helper()
	skipShort(t)
	bin, err := pluginFixtureBin()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"plugins": []any{map[string]any{
		"name":    "fixture",
		"command": []string{bin},
		"config":  map[string]any{"segment": fixtureSegment},
	}}}
}

func requestHasSegment(want bool) harnesstest.Matcher {
	return func(r harnesstest.Request) bool { return strings.Contains(r.System, fixtureSegment) == want }
}

func all(ms ...harnesstest.Matcher) harnesstest.Matcher {
	return func(r harnesstest.Request) bool {
		for _, m := range ms {
			if !m(r) {
				return false
			}
		}
		return true
	}
}

func toolReply(id, name string, in map[string]any) harnesstest.Reply {
	return harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: id, Name: name, Input: in}}}
}

func TestContractPluginTools(t *testing.T) {
	cfg := pluginConfig(t)
	runScenarios(t, []scenario{
		{
			name:   "plugin_tools_listed_and_run",
			config: cfg,
			model: toolChain(
				ftTool("fixture_echo", ftArgs("text", "hi")),
				ftTool("fixture_fail", ftArgs()),
				ftTool("fixture_config", ftArgs()),
			),
			actions: []action{
				create{as: "a"},
				getSession{as: "a"},
				submit{as: "a", text: "go"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name:   "plugin_system_segment_in_every_request",
			config: cfg,
			model: []harnesstest.Step{
				{Name: "call", Match: all(assistantTurns(0), requestHasSegment(true)), Reply: toolReply("toolu_1", "ls", ftArgs("path", "."))},
				{Name: "done", Match: all(assistantTurns(1), requestHasSegment(true)), Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: oneTurn,
		},
	})
}

func TestContractPluginHooks(t *testing.T) {
	cfg := pluginConfig(t)
	runScenarios(t, []scenario{
		{
			name:   "plugin_before_hook_rewrites_and_blocks",
			config: cfg,
			model: toolChain(
				ftBash("echo rewrite-me"),
				ftBash("echo block-me"),
				ftBash("echo plain"),
			),
			actions: oneTurn,
		},
		{
			name:   "plugin_after_hook_sees_output",
			config: cfg,
			model: toolChain(
				ftBash("echo hello"),
				ftBash("echo oops; exit 3"),
				ftTool("ls", ftArgs("path", ".")),
			),
			actions: oneTurn,
		},
	})
}

// A dead plugin is never respawned: its hooks are skipped and its tools fail
// with the raw pipe error, which pins a defect.
func TestContractPluginCrash(t *testing.T) {
	cfg := pluginConfig(t)
	runScenarios(t, []scenario{
		{
			name:   "plugin_crash_mid_call_session_continues",
			config: cfg,
			model: []harnesstest.Step{
				{Name: "crash", Match: all(harnesstest.LastUserText("crash"), requestHasSegment(true)), Reply: toolReply("toolu_1", "fixture_crash", ftArgs())},
				{Name: "reported", Match: all(harnesstest.LastToolResult("fixture_crash"), requestHasSegment(false)), Reply: harnesstest.Reply{Text: "plugin died"}},
				{Name: "again", Match: harnesstest.LastUserText("again"), Reply: toolReply("toolu_2", "bash", ftArgs("command", "echo rewrite-me"))},
				{Name: "echo", Match: harnesstest.LastToolResult("bash"), Reply: toolReply("toolu_3", "fixture_echo", ftArgs("text", "hi"))},
				{Name: "fin", Match: harnesstest.LastToolResult("fixture_echo"), Reply: harnesstest.Reply{Text: "still here"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "crash"},
				waitIdle{as: "a"},
				submit{as: "a", text: "again"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}
