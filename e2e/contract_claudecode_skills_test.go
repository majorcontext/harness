package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

// claudeSystemValue records, for each fakeclaude spawn in order, the value of
// --append-system-prompt with the work dir as <workdir>.
type claudeSystemValue struct{ as string }

func (a claudeSystemValue) run(t *testing.T, r *run) {
	data, err := os.ReadFile(claudeDriverOf(t, r).argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	out := []any{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("decode argv line %q: %v", line, err)
		}
		value := ""
		if i := slices.Index(argv, "--append-system-prompt"); i >= 0 && i+1 < len(argv) {
			value = argv[i+1]
		}
		for _, w := range workdirPaths(r.drv.Workdir()) {
			value = strings.ReplaceAll(value, w, "<workdir>")
		}
		out = append(out, value)
	}
	r.record(t, "claude_system_value", a.as, callResult{Status: http.StatusOK, Body: out})
}

// claudeSessionInfoSkills records the skills that the session_info tool
// reported to the CLI, with the work dir as <workdir>.
type claudeSessionInfoSkills struct{ as string }

func (a claudeSessionInfoSkills) run(t *testing.T, r *run) {
	body, _ := claudeDriverOf(t, r).lastToolCall(t).Body.(map[string]any)
	result, _ := body["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("session_info returned no content: %v", body)
	}
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	var info struct {
		Skills []map[string]string `json:"skills"`
	}
	if err := json.Unmarshal([]byte(text), &info); err != nil {
		t.Fatalf("decode session_info %q: %v", text, err)
	}
	for _, s := range info.Skills {
		for _, w := range workdirPaths(r.drv.Workdir()) {
			s["path"] = strings.ReplaceAll(s["path"], w, "<workdir>")
		}
	}
	r.record(t, "session_info_skills", a.as, callResult{Status: http.StatusOK, Body: info.Skills})
}

func TestContractClaudeCodeSkills(t *testing.T) {
	demo := writeFile{path: ".agents/skills/demo/SKILL.md", body: skillFile("demo", "Demonstrates discovery.", "The skill body.")}
	picked := writeFile{path: "custom/picked/SKILL.md", body: skillFile("picked", "Scanned.", "x")}
	withTurn := func(files []action, record action) []action {
		return append(append(files, claudeOneTurn...), record)
	}
	runScenarios(t, []scenario{{
		name:    "claudecode_system_prompt_names_the_harness_tools_and_lists_the_skills",
		driver:  claudeLane{mode: "normal", extra: map[string]any{"append_system_prompt": []string{"one"}}}.newDriver,
		actions: withTurn([]action{demo}, claudeSystemValue{as: "a"}),
	}, {
		name:    "claudecode_skills_dirs_config_changes_the_skill_list",
		driver:  claudeLane{mode: "normal", extra: map[string]any{"skills_dirs": []string{"custom"}}}.newDriver,
		actions: withTurn([]action{demo, picked}, claudeSystemValue{as: "a"}),
	}, {
		name:    "claudecode_session_info_lists_the_skills",
		driver:  claudeLane{mode: "normal", callTool: "session_info", callArgs: map[string]any{}}.newDriver,
		actions: withTurn([]action{demo}, claudeSessionInfoSkills{as: "a"}),
	}})
}
