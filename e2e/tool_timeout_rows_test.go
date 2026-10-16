package e2e

import "maps"

// toolTimeoutRows are the runtime dispositions of the tool time limit rows.
var toolTimeoutRows = map[string]runtimeRow{
	"mcp_tool_call_past_tool_timeout_s_is_an_error_result_and_the_turn_continues":    sameAsServe(),
	"plugin_tool_call_past_tool_timeout_s_is_an_error_result_and_the_turn_continues": sameAsServe(),
	"plugin_that_starts_slower_than_tool_timeout_s_is_not_left_failed":               sameAsServe(),
}

func init() { maps.Copy(runtimeRows, toolTimeoutRows) }
