package e2e

import (
	"slices"
	"strings"
)

func argvFacts(argv []string) map[string]any {
	value := func(flag string) any {
		if i := slices.Index(argv, flag); i >= 0 && i+1 < len(argv) {
			return argv[i+1]
		}
		return nil
	}
	appended, _ := value("--append-system-prompt").(string)
	facts := map[string]any{
		"model":             value("--model"),
		"resume":            value("--resume"),
		"history_directive": strings.Contains(appended, "get_conversation_history"),
		"mcp_config":        slices.Contains(argv, "--mcp-config"),
		"tools":             value("--tools"),
		"thinking_display":  value("--thinking-display"),
		"disallowed_tools":  value("--disallowedTools"),
		"strict_mcp_config": slices.Contains(argv, "--strict-mcp-config"),
	}
	if slices.Contains(argv, "--no-session-persistence") {
		facts["no_session_persistence"] = true
		facts["settings"] = value("--settings")
		facts["system_prompt_replaced"] = slices.Contains(argv, "--system-prompt") && !slices.Contains(argv, "--append-system-prompt")
	}
	if n := value("--max-turns"); n != nil {
		facts["max_turns"] = n
	}
	if e := value("--effort"); e != nil {
		facts["effort"] = e
	}
	tools, _ := value("--tools").(string)
	allowed, _ := value("--allowedTools").(string)
	if slices.Contains(strings.Split(tools, ","), "WebSearch") {
		facts["web_search_offered"] = slices.Contains(strings.Split(allowed, ","), "WebSearch")
	}
	return facts
}
