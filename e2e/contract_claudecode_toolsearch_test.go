package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

// claudeToolSearch records whether the last CLI spawn got ToolSearch in
// --tools, and the ENABLE_TOOL_SEARCH value of its environment.
type claudeToolSearch struct{ as string }

func (a claudeToolSearch) run(t *testing.T, r *run) {
	d := claudeDriverOf(t, r)
	data, err := os.ReadFile(d.argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var argv []string
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &argv); err != nil {
		t.Fatalf("decode argv line: %v", err)
	}
	var tools string
	if i := slices.Index(argv, "--tools"); i >= 0 && i+1 < len(argv) {
		tools = argv[i+1]
	}
	raw, err := os.ReadFile(d.envLog)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	var env []string
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode env log: %v", err)
	}
	var enable any
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "ENABLE_TOOL_SEARCH="); ok {
			enable = v
		}
	}
	r.record(t, "tool_search_flags", a.as, callResult{Status: http.StatusOK, Body: map[string]any{
		"tool_search_offered": slices.Contains(strings.Split(tools, ","), "ToolSearch"), "enable_tool_search": enable}})
}

// claudeNoAPIKeyEnv records whether the last CLI spawn saw ANTHROPIC_API_KEY or
// ANTHROPIC_AUTH_TOKEN.
type claudeNoAPIKeyEnv struct{ as string }

func (a claudeNoAPIKeyEnv) run(t *testing.T, r *run) {
	raw, err := os.ReadFile(claudeDriverOf(t, r).envLog)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	var env []string
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode env log: %v", err)
	}
	seen := slices.ContainsFunc(env, func(e string) bool {
		return strings.HasPrefix(e, "ANTHROPIC_API_KEY=") || strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN=")
	})
	r.record(t, "cli_env_credentials", a.as, callResult{Status: http.StatusOK, Body: map[string]any{"cli_saw_anthropic_credentials": seen}})
}

func TestContractClaudeCodeToolSearch(t *testing.T) {
	loading := func(mode string) func(*testing.T, host, string) driver {
		var extra map[string]any
		if mode != "" {
			extra = map[string]any{"mcp_tool_loading": mode}
		}
		return claudeLane{mode: "normal", extra: extra}.newDriver
	}
	turn := withActions(claudeOneTurn, claudeToolSearch{as: "a"})
	withCredentials := claudeLane{mode: "normal", procEnv: map[string]string{"ANTHROPIC_API_KEY": "e2e-key", "ANTHROPIC_AUTH_TOKEN": "e2e-token"}}.newDriver
	runScenarios(t, []scenario{
		{name: "claudecode_cli_runs_without_the_anthropic_credential_variables", driver: withCredentials, actions: withActions(claudeOneTurn, claudeNoAPIKeyEnv{as: "a"})},
		{
			name:    "claudecode_tool_search_call_and_its_result_do_not_fail_the_turn",
			driver:  claudeLane{mode: "tool_search"}.newDriver,
			actions: withActions(claudeOneTurn, claudeSession{as: "a"}, claudeToolSearch{as: "a"}),
		},
		{name: "claudecode_mcp_tool_loading_eager_turns_the_cli_tool_search_off", driver: loading("eager"), actions: turn},
		{name: "claudecode_mcp_tool_loading_lazy_turns_the_cli_tool_search_on", driver: loading("lazy"), actions: turn},
		{name: "claudecode_mcp_tool_loading_auto_leaves_the_cli_tool_search_to_the_cli", driver: loading("auto"), actions: turn},
		{name: "claudecode_mcp_tool_loading_unset_leaves_the_cli_tool_search_to_the_cli", driver: loading(""), actions: turn},
	})
}
