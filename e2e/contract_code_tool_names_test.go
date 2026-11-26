package e2e

import (
	"strings"
	"testing"
)

func TestContractCodeToolNames(t *testing.T) {
	weather := func(name string) mcpServerDef { return mcpServerDef{name: name, spec: mcpWeather("")} }
	rows := []struct {
		name, script, want, absent string
		servers                    []mcpServerDef
	}{
		{
			name:   "a_hyphenated_deferred_mcp_tool_is_callable_by_its_alias",
			script: `mcp__weather_station__forecast(city="Oslo")`,
			want:   "Oslo", absent: "undefined", servers: []mcpServerDef{weather("weather-station")},
		},
		{
			name:   "a_colliding_tool_alias_is_not_bound",
			script: `mcp__weather_station__forecast(city="Oslo")`,
			want:   "undefined: mcp__weather_station__forecast", absent: "Weather in",
			servers: []mcpServerDef{weather("weather-station"), weather("weather.station")},
		},
		{
			name: "call_stops_the_script_on_an_error_result",
			script: `call("read_file", path="missing.txt")
print("after")`,
			want: "missing.txt", absent: "after",
		},
		{
			name:   "call_refuses_a_not_callable_tool",
			script: `call("model", action="list")`,
			want:   "no such tool available: model", absent: "undefined: call",
		},
		{
			name:   "an_undefined_name_suggests_the_exact_tool_name",
			script: `mcp__weather_statio__forecast(city="Oslo")`,
			want:   `call("mcp__weather-station__forecast", ...)`, servers: []mcpServerDef{weather("weather-station")},
		},
		{
			name:   "an_unrelated_undefined_name_still_suggests_the_closest_tool",
			script: `zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz()`,
			want:   `; try call(`,
		},
		{
			name:   "search_tools_accepts_a_keyword_query",
			script: `[t["name"] for t in search_tools(query="forecast")]`,
			want:   `mcp__weather-station__forecast`, absent: "Error", servers: []mcpServerDef{weather("weather-station")},
		},
	}
	var scenarios []scenario
	for _, row := range rows {
		scenarios = append(scenarios, scenario{
			name: row.name, config: codeWith(map[string]any{"mcp_tool_loading": "lazy"}),
			setup: mcpSetup(nil, row.servers...), model: toolChain(codeCall(row.script)),
			actions: withActions(oneTurn,
				expectCodeResult{want: row.want, absent: row.absent}),
		})
	}
	runScenarios(t, scenarios)
}

type expectCodeResult struct{ want, absent string }

func (a expectCodeResult) run(t *testing.T, r *run) {
	t.Helper()
	for _, p := range toolResults(r.drv.Messages(t, r.id(t, "a"))) {
		if p.CallID == "toolu_1" {
			if !strings.Contains(p.Content, a.want) || a.absent != "" && strings.Contains(p.Content, a.absent) {
				t.Fatalf("result = %q, want %q and not %q", p.Content, a.want, a.absent)
			}
			return
		}
	}
	t.Fatal("no code result")
}

var codeToolNameRows = map[string]runtimeRow{
	"an_unrelated_undefined_name_still_suggests_the_closest_tool": sameAsServe(),
	"a_hyphenated_deferred_mcp_tool_is_callable_by_its_alias":     sameAsServe(),
	"a_colliding_tool_alias_is_not_bound":                         sameAsServe(),
	"call_stops_the_script_on_an_error_result":                    sameAsServe(),
	"call_refuses_a_not_callable_tool":                            sameAsServe(),
	"an_undefined_name_suggests_the_exact_tool_name":              sameAsServe(),
	"search_tools_accepts_a_keyword_query":                        sameAsServe(),
}
