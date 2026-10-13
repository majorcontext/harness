package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func matchAll(ms ...harnesstest.Matcher) harnesstest.Matcher {
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
				{Name: "call", Match: matchAll(assistantTurns(0), requestHasSegment(true)), Reply: pluginToolReply("toolu_1", "ls", ftArgs("path", "."))},
				{Name: "done", Match: matchAll(assistantTurns(1), requestHasSegment(true)), Reply: harnesstest.Reply{Text: "done"}},
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
				{Name: "first", Match: matchAll(harnesstest.LastUserText("first"), harnesstest.SystemContains("LAST-USER: first")), Reply: harnesstest.Reply{Text: "ok1"}},
				{Name: "second", Match: matchAll(harnesstest.LastUserText("second"), harnesstest.SystemContains("LAST-USER: second")), Reply: harnesstest.Reply{Text: "ok2"}},
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
				{Name: "first", Match: matchAll(harnesstest.LastUserText("first"), harnesstest.SystemContains("MODEL: anthropic/claude-fable-5")), Reply: harnesstest.Reply{Text: "ok1"}},
				{Name: "second", Match: matchAll(harnesstest.LastUserText("second"), harnesstest.SystemContains("MODEL: anthropic/claude-haiku-4-5")), Reply: harnesstest.Reply{Text: "ok2"}},
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
			name:    "plugin_shell_env_hook_sets_the_environment_of_a_bash_command",
			config:  pluginConfig(t, map[string]any{"shell_env": "FIXTURE_ENV=from-plugin"}),
			model:   toolChain(ftBash("echo $FIXTURE_ENV")),
			actions: oneTurn,
		},
		{
			name:   "plugin_chat_params_hook_sets_the_cap_and_sampling_of_a_model_call",
			config: pluginConfig(t, map[string]any{"chat_params": map[string]any{"max_tokens": 77, "temperature": 0.25, "top_p": 0.5}}),
			model: []harnesstest.Step{{Name: "tuned", Match: func(r harnesstest.Request) bool {
				return r.MaxTokens == 77 && r.Temperature != nil && *r.Temperature == 0.25 && r.TopP != nil && *r.TopP == 0.5
			}, Reply: harnesstest.Reply{Text: "tuned"}}},
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
				{Name: "crash", Match: matchAll(harnesstest.LastUserText("crash"), requestHasSegment(true)), Reply: pluginToolReply("toolu_1", "fixture_crash", ftArgs())},
				{Name: "reported", Match: matchAll(harnesstest.LastToolResult("fixture_crash"), requestHasSegment(false)), Reply: harnesstest.Reply{Text: "plugin died"}},
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

// pluginEntry is one fixture plugin entry under name, with conf as its config block.
func pluginEntry(t *testing.T, name string, conf map[string]any) map[string]any {
	t.Helper()
	skipShort(t)
	bin, err := pluginFixtureBin()
	if err != nil {
		t.Fatal(err)
	}
	conf = maps.Clone(conf)
	if conf == nil {
		conf = map[string]any{}
	}
	conf["name"] = name
	return map[string]any{"name": name, "command": []string{bin}, "config": conf}
}

func TestContractPluginConfigMerge(t *testing.T) {
	userGuard := pluginEntry(t, "user-guard", map[string]any{"deny": "--force"})
	projectRewrite := pluginEntry(t, "project-rw", map[string]any{"rewrite": map[string]any{"from": "echo push", "to": "git push --force"}})
	projectFile := func(t *testing.T, plugins ...any) map[string]string {
		t.Helper()
		body, err := json.Marshal(map[string]any{"plugins": plugins})
		if err != nil {
			t.Fatal(err)
		}
		return map[string]string{".harness.json": string(body)}
	}
	runScenarios(t, []scenario{
		{
			name:    "plugin_user_guard_sees_the_project_rewrite",
			model:   toolChain(ftBash("echo push")),
			driver:  inTreeWith(projectFile(t, projectRewrite), "", map[string]any{"plugins": []any{userGuard}}),
			actions: oneTurn,
		},
		{
			name:    "plugin_project_plugins_alone_keep_their_chain",
			model:   toolChain(ftBash("echo rewrite-me"), ftBash("echo block-me")),
			driver:  inTreeWith(projectFile(t, pluginEntry(t, "fixture", nil)), "", nil),
			actions: oneTurn,
		},
	})
}

func TestContractPluginDenyOnFailure(t *testing.T) {
	flagged := func(fail string) map[string]any {
		return pluginConfig(t, map[string]any{"deny_on_failure": true, "before_fail": fail})
	}
	runScenarios(t, []scenario{
		{
			name:    "plugin_flagged_before_hook_denies_when_the_plugin_exits",
			config:  flagged("exit"),
			model:   toolChain(ftBash("echo should-not-run")),
			actions: oneTurn,
		},
		{
			name:    "plugin_flagged_before_hook_denies_on_an_error",
			config:  flagged("error"),
			model:   toolChain(ftBash("echo should-not-run")),
			actions: oneTurn,
		},
		{
			name:    "plugin_flagged_before_hook_denies_at_the_deadline",
			config:  pluginConfig(t, map[string]any{"deny_on_failure": true, "before_fail": "hang", "hook_timeouts": map[string]any{"tool.execute.before": 200}}),
			model:   toolChain(ftBash("echo should-not-run")),
			actions: oneTurn,
		},
		{
			name:    "plugin_unflagged_before_hook_fails_open",
			config:  pluginConfig(t, map[string]any{"before_fail": "exit"}),
			model:   toolChain(ftBash("echo should-run")),
			actions: oneTurn,
		},
	})
}

func TestContractPluginInvalidDenyOnFailureFailsStart(t *testing.T) {
	skipShort(t)
	cases := map[string]struct {
		conf map[string]any
		want string
	}{
		"another_hook":      {map[string]any{"deny_hook": "tool.execute.after"}, "plugin fixture: deny_on_failure is valid only for tool.execute.before"},
		"unsubscribed_hook": {map[string]any{"deny_unsubscribed": true}, "plugin fixture: deny_on_failure names tool.execute.before, which hooks does not list"},
	}
	onHosts(t, func(t *testing.T, h host) {
		for name, tc := range cases {
			cfg := pluginConfig(t, tc.conf)
			t.Run(name, func(t *testing.T) {
				d, _ := startOn(t, h, runtimeWorkdir(t, nil), cfg, replyText("ok"))
				res := d.CreateModel(t, "")
				if res.Status/100 == 2 {
					t.Fatalf("create = %d %v, want a failure: %s", res.Status, res.Body, tc.want)
				}
				if !h.runtime {
					awaitLog(t, d, tc.want)
				}
			})
		}
	})
}

func TestContractPluginHookTimeouts(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "plugin_before_hook_timeout_over_default_is_honored",
			config: pluginConfig(t, map[string]any{
				"hook_timeouts":   map[string]any{"tool.execute.before": 10000},
				"before_delay_ms": 5500,
				"rewrite":         map[string]any{"from": "echo slow", "to": "echo rewritten-late"},
			}),
			model:   toolChain(ftBash("echo slow")),
			actions: oneTurn,
		},
	})
}

func TestContractPluginInvalidHookTimeoutsFailStart(t *testing.T) {
	skipShort(t)
	cases := map[string]struct {
		conf map[string]any
		want string
	}{
		"unsubscribed_hook": {map[string]any{"hook_timeouts": map[string]any{"chat.params": 1000}}, "plugin fixture: hook_timeouts names chat.params, which hooks does not list"},
		"event":             {map[string]any{"hook_timeouts": map[string]any{"event": 1000}}, "plugin fixture: hook_timeouts is not valid for event"},
		"over_the_limit":    {map[string]any{"hook_timeouts": map[string]any{"tool.execute.before": 600001}}, "plugin fixture: hook_timeouts for tool.execute.before must be from 1 to 600000 ms"},
		"zero":              {map[string]any{"hook_timeouts": map[string]any{"tool.execute.before": 0}}, "plugin fixture: hook_timeouts for tool.execute.before must be from 1 to 600000 ms"},
	}
	onHosts(t, func(t *testing.T, h host) {
		for name, tc := range cases {
			cfg := pluginConfig(t, tc.conf)
			t.Run(name, func(t *testing.T) {
				d, _ := startOn(t, h, runtimeWorkdir(t, nil), cfg, replyText("ok"))
				res := d.CreateModel(t, "")
				if res.Status/100 == 2 {
					t.Fatalf("create = %d %v, want a failure: %s", res.Status, res.Body, tc.want)
				}
				if !h.runtime {
					awaitLog(t, d, tc.want)
				}
			})
		}
	})
}

const generateSystem = "GENERATE-SYSTEM"

func isGenerate(r harnesstest.Request) bool { return r.System == generateSystem }

func isToollessGenerate(r harnesstest.Request) bool { return isGenerate(r) && len(r.Tools) == 0 }

func notGenerate(r harnesstest.Request) bool { return !isGenerate(r) }

// ownsLoopConfig adds a claude-code provider, whose models run their own agent loop.
func ownsLoopConfig(cfg map[string]any) func(*testing.T, host, string) driver {
	return func(t *testing.T, h host, modelURL string) driver {
		t.Helper()
		c := maps.Clone(cfg)
		c["providers"] = map[string]any{
			"anthropic":   map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": modelURL},
			"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": fakeClaudePath()},
		}
		return h.newDriver(t, modelURL, c)
	}
}

func TestContractPluginHostCalls(t *testing.T) {
	cfg := pluginConfig(t, map[string]any{"host_calls": true})
	weather := mcpServerDef{name: "weather", spec: mcpWeather("")}
	runScenarios(t, []scenario{
		{
			name:   "plugin_tool_calls_an_mcp_server_through_the_host",
			config: cfg,
			setup:  mcpSetup(nil, weather),
			model:  toolChain(ftTool("fixture_mcp", ftArgs("server", "weather", "tool", "forecast", "mcp_args", ftArgs("city", "Oslo")))),
			actions: append(append([]action{}, oneTurn...), expectMCPCalls{server: "weather", want: []harnesstest.MCPCall{
				{Method: "tools/call", Name: "forecast", Args: map[string]any{"city": "Oslo"}, Authorization: mcpToken},
			}}),
		},
		{
			name:    "plugin_mcp_call_to_an_unknown_server_fails",
			config:  cfg,
			setup:   mcpSetup(nil, weather),
			model:   toolChain(ftTool("fixture_mcp", ftArgs("server", "nowhere", "tool", "forecast"))),
			actions: oneTurn,
		},
		{
			name:    "plugin_mcp_call_reports_a_tool_error",
			config:  cfg,
			setup:   mcpSetup(nil, weather),
			model:   toolChain(ftTool("fixture_mcp", ftArgs("server", "weather", "tool", "flaky"))),
			actions: oneTurn,
		},
	})
}

func blockedGenerate() []harnesstest.Step {
	return []harnesstest.Step{
		{Name: "generate", Match: isGenerate, Reply: harnesstest.Reply{Text: "never read", Block: true}},
		{Name: "call", Match: matchAll(assistantTurns(0), notGenerate),
			Reply: pluginToolReply("toolu_1", "fixture_generate", ftArgs("system", generateSystem, "text", "hi"))},
		{Name: "done", Match: assistantTurns(1), Reply: harnesstest.Reply{Text: "done"}},
	}
}

var pluginPidFiles sync.Map

// pidFileConfig is the driver of a host whose fixture plugin writes its process ID to a file.
func pidFileConfig(t *testing.T) func(*testing.T, host, string) driver {
	cfg := pluginConfig(t, map[string]any{"host_calls": true})
	return func(t *testing.T, h host, modelURL string) driver {
		t.Helper()
		path := filepath.Join(t.TempDir(), "plugin.pid")
		pluginPidFiles.Store(t.Name(), path)
		t.Cleanup(func() { pluginPidFiles.Delete(t.Name()) })
		c := maps.Clone(cfg)
		plugins := c["plugins"].([]any)
		entry := maps.Clone(plugins[0].(map[string]any))
		conf := maps.Clone(entry["config"].(map[string]any))
		conf["pid_file"] = path
		entry["config"] = conf
		c["plugins"] = []any{entry}
		return h.newDriver(t, modelURL, c)
	}
}

type awaitBlocked struct{ step string }

func (a awaitBlocked) run(t *testing.T, r *run) {
	t.Helper()
	if !r.fake.AwaitBlocked(a.step, waitBound) {
		t.Fatalf("model request for step %q was not waiting within %s\nserve stderr:\n%s", a.step, waitBound, r.drv.Stderr())
	}
}

// killPlugin kills the fixture plugin process.
type killPlugin struct{}

func (killPlugin) run(t *testing.T, _ *run) {
	t.Helper()
	path, _ := pluginPidFiles.Load(t.Name())
	b, err := os.ReadFile(path.(string))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

func TestContractPluginToolBlob(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "plugin_tool_blob_output_reaches_the_model",
			config:  pluginConfig(t, map[string]any{"extra_tool": "fixture_blob"}),
			model:   toolChain(ftTool("fixture_blob", ftArgs())),
			actions: oneTurn,
		},
	})
}

func TestContractPluginGenerate(t *testing.T) {
	cfg := pluginConfig(t, map[string]any{"host_calls": true})
	runScenarios(t, []scenario{
		{
			name:   "plugin_tool_generates_through_the_host",
			config: cfg,
			model: []harnesstest.Step{
				{Name: "generate", Match: isToollessGenerate, Reply: harnesstest.Reply{Text: "generated reply", Usage: harnesstest.Usage{Input: 11, Output: 7}}},
				{Name: "call", Match: matchAll(assistantTurns(0), notGenerate),
					Reply: pluginToolReply("toolu_1", "fixture_generate", ftArgs("system", generateSystem, "text", "say hi"))},
				{Name: "done", Match: assistantTurns(1), Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: oneTurn,
		},
		{
			name:   "plugin_generate_resolves_an_alias",
			config: pluginConfig(t, map[string]any{"host_calls": true}),
			setup: func(*testing.T, map[string]any) map[string]any {
				return map[string]any{"aliases": map[string]any{"quick": "anthropic/alias-model"}}
			},
			model: []harnesstest.Step{
				{Name: "generate", Match: matchAll(isToollessGenerate, func(r harnesstest.Request) bool { return r.Model == "alias-model" }),
					Reply: harnesstest.Reply{Text: "from alias"}},
				{Name: "call", Match: matchAll(assistantTurns(0), notGenerate),
					Reply: pluginToolReply("toolu_1", "fixture_generate", ftArgs("model", "quick", "system", generateSystem, "text", "hi"))},
				{Name: "done", Match: assistantTurns(1), Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: oneTurn,
		},
		{
			name:    "plugin_generate_refuses_a_model_that_runs_its_own_agent_loop",
			config:  cfg,
			driver:  ownsLoopConfig(cfg),
			model:   toolChain(ftTool("fixture_generate", ftArgs("model", "claude-code/sonnet", "text", "hi"))),
			actions: oneTurn,
		},
		{
			name:   "plugin_exit_during_generate_ends_the_call",
			driver: pidFileConfig(t),
			model:  blockedGenerate(),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"},
				awaitBlocked{step: "generate"},
				killPlugin{},
				awaitCanceled{step: "generate"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name:    "plugin_generate_rejects_a_negative_max_tokens",
			config:  cfg,
			model:   toolChain(ftTool("fixture_generate", ftArgs("text", "hi", "max_tokens", -1))),
			actions: oneTurn,
		},
		{
			name:    "plugin_generate_with_an_unserved_model_fails",
			config:  cfg,
			model:   toolChain(ftTool("fixture_generate", ftArgs("model", "nosuch/model", "text", "hi"))),
			actions: oneTurn,
		},
	})
}
