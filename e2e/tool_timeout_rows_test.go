package e2e

import "maps"

// toolTimeoutRows are the runtime dispositions of the tool time limit rows.
var toolTimeoutRows = map[string]runtimeRow{
	"mcp_tool_call_past_tool_timeout_s_is_an_error_result_and_the_turn_continues":    sameAsServe(),
	"plugin_tool_call_past_tool_timeout_s_is_an_error_result_and_the_turn_continues": sameAsServe(),
	"plugin_that_starts_slower_than_tool_timeout_s_is_not_left_failed":               sameAsServe(),
}

// allRowDispositions returns runtimeRows and toolTimeoutRows as one map.
func allRowDispositions() map[string]runtimeRow {
	all := maps.Clone(runtimeRows)
	maps.Copy(all, toolTimeoutRows)
	return all
}

// rowDisposition returns the disposition of the row name.
func rowDisposition(name string) (runtimeRow, bool) {
	if row, ok := runtimeRows[name]; ok {
		return row, true
	}
	row, ok := toolTimeoutRows[name]
	return row, ok
}
