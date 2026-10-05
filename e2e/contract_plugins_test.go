package e2e

import (
	"fmt"
	"maps"
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

func pluginConfig(t *testing.T, extra map[string]any) map[string]any {
	t.Helper()
	skipShort(t)
	bin, err := pluginFixtureBin()
	if err != nil {
		t.Fatal(err)
	}
	conf := map[string]any{"segment": fixtureSegment}
	maps.Copy(conf, extra)
	return map[string]any{"plugins": []any{map[string]any{
		"name":    "fixture",
		"command": []string{bin},
		"config":  conf,
	}}}
}

// pluginBoxesConfig installs the fixture the way boxinit installs a plugin:
// an interpreter-style command with a script file, a working directory, and
// no config block.
func pluginBoxesConfig(t *testing.T) map[string]any {
	t.Helper()
	skipShort(t)
	bin, err := pluginFixtureBin()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "plugin-cwd")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fixture.ts")
	if err := os.WriteFile(script, []byte("// script file named in the command\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"plugins": []any{map[string]any{
		"name":    "fixture",
		"command": []string{bin, script},
		"dir":     dir,
	}}}
}

// attachmentsSegment is the segment that the fixture builds from the blob
// parts of rowAttachments.
func attachmentsSegment() string {
	var sizes []string
	for _, a := range rowAttachments() {
		sizes = append(sizes, fmt.Sprintf("%s:%d", a.mediaType, len(a.data)))
	}
	return "BLOBS: " + strings.Join(sizes, " ")
}

func requestHasSegment(want bool) harnesstest.Matcher {
	return func(r harnesstest.Request) bool { return strings.Contains(r.System, fixtureSegment) == want }
}

func pluginMatchAll(ms ...harnesstest.Matcher) harnesstest.Matcher {
	return func(r harnesstest.Request) bool {
		for _, m := range ms {
			if !m(r) {
				return false
			}
		}
		return true
	}
}

func pluginToolReply(id, name string, in map[string]any) harnesstest.Reply {
	return harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: id, Name: name, Input: in}}}
}

func TestContractPluginTools(t *testing.T) {
	cfg := pluginConfig(t, nil)
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
				{Name: "call", Match: pluginMatchAll(assistantTurns(0), requestHasSegment(true)), Reply: pluginToolReply("toolu_1", "ls", ftArgs("path", "."))},
				{Name: "done", Match: pluginMatchAll(assistantTurns(1), requestHasSegment(true)), Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: oneTurn,
		},
		{
			name:    "session_info_reports_the_plugin_and_its_system_segment",
			config:  cfg,
			model:   toolChain(ftTool("session_info", map[string]any{})),
			actions: oneTurn,
		},
		{
			name:   "plugin_system_transform_reads_session_messages",
			config: pluginConfig(t, map[string]any{"recall": true}),
			model: []harnesstest.Step{
				{Name: "first", Match: pluginMatchAll(harnesstest.LastUserText("first"), harnesstest.SystemContains("LAST-USER: first")), Reply: harnesstest.Reply{Text: "ok1"}},
				{Name: "second", Match: pluginMatchAll(harnesstest.LastUserText("second"), harnesstest.SystemContains("LAST-USER: second")), Reply: harnesstest.Reply{Text: "ok2"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				waitIdle{as: "a"},
				submit{as: "a", text: "second"},
				waitIdle{as: "a"},
			},
		},
		{
			name:   "plugin_session_messages_carry_attachments",
			config: pluginConfig(t, map[string]any{"recall_blobs": true}),
			model:  []harnesstest.Step{{Name: "seen", Match: harnesstest.SystemContains(attachmentsSegment()), Reply: harnesstest.Reply{Text: "ok"}}},
			actions: []action{
				create{as: "a"},
				submitAttachments{as: "a", text: "look"},
				waitIdle{as: "a"},
			},
		},
		{
			name:    "plugin_boxes_style_command_and_dir",
			config:  pluginBoxesConfig(t),
			model:   toolChain(ftTool("fixture_report", ftArgs("process", true))),
			actions: oneTurn,
		},
	})
}

func TestContractPluginSession(t *testing.T) {
	cfg := pluginConfig(t, nil)
	runScenarios(t, []scenario{
		{
			name:   "plugin_sees_the_model_of_each_call",
			config: pluginConfig(t, map[string]any{"model": true}),
			model: []harnesstest.Step{
				{Name: "first", Match: pluginMatchAll(harnesstest.LastUserText("first"), harnesstest.SystemContains("MODEL: anthropic/claude-fable-5")), Reply: harnesstest.Reply{Text: "ok1"}},
				{Name: "second", Match: pluginMatchAll(harnesstest.LastUserText("second"), harnesstest.SystemContains("MODEL: anthropic/claude-haiku-4-5")), Reply: harnesstest.Reply{Text: "ok2"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				waitIdle{as: "a"},
				setModel{as: "a", model: "anthropic/claude-haiku-4-5"},
				submit{as: "a", text: "second"},
				waitIdle{as: "a"},
			},
		},
		{
			name:   "plugin_inventory_reports_not_spawned_then_running",
			config: cfg,
			model:  toolChain(ftTool("fixture_echo", ftArgs("text", "hi"))),
			actions: []action{
				create{as: "a"},
				getSession{as: "a"},
				submit{as: "a", text: "go"},
				waitIdle{as: "a"},
				getSession{as: "a"},
				listSessions{},
			},
		},
	})
}

func TestContractPluginHooks(t *testing.T) {
	cfg := pluginConfig(t, nil)
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
		{
			name:   "plugin_event_and_after_hook_payloads",
			config: cfg,
			model: toolChain(
				ftBash("echo rewrite-me"),
				ftWrite("edited.txt", "x"),
				ftTool("fixture_report", ftArgs("edits", 1)),
			),
			actions: oneTurn,
		},
	})
}

func TestContractPluginCrash(t *testing.T) {
	cfg := pluginConfig(t, nil)
	runScenarios(t, []scenario{
		{
			// Defect: a dead plugin's tool returns the raw pipe error "write |1: broken pipe".
			name:   "plugin_crash_mid_call_session_continues",
			config: cfg,
			model: []harnesstest.Step{
				{Name: "crash", Match: pluginMatchAll(harnesstest.LastUserText("crash"), requestHasSegment(true)), Reply: pluginToolReply("toolu_1", "fixture_crash", ftArgs())},
				{Name: "reported", Match: pluginMatchAll(harnesstest.LastToolResult("fixture_crash"), requestHasSegment(false)), Reply: harnesstest.Reply{Text: "plugin died"}},
				{Name: "again", Match: harnesstest.LastUserText("again"), Reply: pluginToolReply("toolu_2", "bash", ftArgs("command", "echo rewrite-me"))},
				{Name: "echo", Match: harnesstest.LastToolResult("bash"), Reply: pluginToolReply("toolu_3", "fixture_echo", ftArgs("text", "hi"))},
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
