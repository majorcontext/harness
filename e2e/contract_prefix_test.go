package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// expectToolDescription checks that the description of tool in request req holds has, and its input schema holds schema.
type expectToolDescription struct {
	req               int
	tool, has, schema string
}

func (a expectToolDescription) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) {
		t.Fatalf("expectToolDescription: request %d of %d", a.req, len(reqs))
	}
	spec, ok := reqs[a.req-1].ToolSpecs[a.tool]
	if !ok || !strings.Contains(spec.Description, a.has) || !strings.Contains(string(spec.InputSchema), a.schema) {
		t.Errorf("request %d of %s = %q %s (sent: %v), want %q and %q", a.req, a.tool, spec.Description, spec.InputSchema, ok, a.has, a.schema)
	}
}

// weatherTools is the tool set of a weather server whose forecast has the description desc, followed by extra.
func weatherTools(desc string, extra ...harnesstest.MCPTool) []harnesstest.MCPTool {
	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	return append([]harnesstest.MCPTool{{Name: "forecast", Description: desc, InputSchema: schema, Result: mcpText("Oslo: 3C, snow")}}, extra...)
}

var weatherWarnings = harnesstest.MCPTool{Name: "warnings", Description: "List warnings of a region", InputSchema: json.RawMessage(`{"type":"object","properties":{"region":{"type":"string"}},"required":["region"]}`), Result: mcpText("no warnings")}

var weatherSnow = harnesstest.MCPTool{Name: "snow", Description: "Report the snow depth", Result: mcpText("30 cm")}

var weatherAlerts = harnesstest.MCPTool{Name: "alerts", Description: "List active weather alerts", Result: mcpText("no active alerts")}

// declaredPrefixRows is the disposition of the rows of the declared prefix.
var declaredPrefixRows = map[string]runtimeRow{
	"declared_tools_stay_the_same_after_a_restart_with_a_changed_description":  sameAsServe(),
	"a_tool_added_after_the_declaration_is_deferred_after_a_restart":           sameAsServe(),
	"a_declared_tool_that_is_gone_reads_no_such_tool_after_a_restart":          sameAsServe(),
	"a_plugin_tool_that_the_declaration_lacks_is_searched_selected_and_called": sameAsServe(),
	"a_first_server_after_a_declaration_with_no_mcp_tool_gives_no_notice":      sameAsServe(),
	"a_removed_last_server_leaves_the_next_declaration_with_no_mcp_tool":       sameAsServe(),
	"a_declared_mcp_tool_works_after_the_last_plugin_is_removed":               sameAsServe(),
	"a_tool_added_to_a_server_with_a_cut_catalog_is_named_by_the_added_notice": sameAsServe(),
	"a_plugin_added_in_a_running_process_is_declared_at_the_next_compaction":   sameAsServe(),
	"a_tool_added_to_a_lazy_server_is_named_by_the_added_notice":               sameAsServe(),
	"a_backend_that_owns_the_loop_declares_nothing":                            sameAsServe(),
	"a_tool_that_the_declaration_lacks_is_selected_with_its_schema":            sameAsServe(),
	"a_compaction_between_turns_declares_the_live_tools":                       sameAsServe(),
	"a_compaction_between_turns_declares_a_changed_description":                sameAsServe(),
	"a_session_that_does_not_compact_keeps_its_tools_when_another_declares":    sameAsServe(),
	"a_compaction_with_no_change_keeps_the_tools":                              sameAsServe(),
	"a_tool_change_inside_a_turn_waits_for_a_compaction":                       sameAsServe(),
	"a_model_change_declares_the_tools_of_the_new_model":                       sameAsServe(),
	"a_changed_input_schema_declares_after_a_restart":                          sameAsServe(),
}

const forecastFirst = "Get the weather forecast for a city"

var (
	declaredWeather = mcpServerDef{name: "weather", spec: harnesstest.MCPSpec{Name: "weather", Tools: weatherTools(forecastFirst)}}
	liveWeather     = mcpServerDef{name: "weather", spec: harnesstest.MCPSpec{Name: "weather", Tools: weatherTools(forecastFirst)}, sse: true}
	keepOneTurn     = map[string]any{"compaction_keep_turns": 1}
	gistSummary     = harnesstest.Step{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}}
)

// bigTools is the tool set of a server with more tools than the prompt catalog lists.
func bigTools() []harnesstest.MCPTool {
	var tools []harnesstest.MCPTool
	for i := range 205 {
		tools = append(tools, harnesstest.MCPTool{Name: fmt.Sprintf("t%03d", i), Description: "Tool number " + strconv.Itoa(i), Result: mcpText("ok")})
	}
	return tools
}

var bigWeather = mcpServerDef{name: "big", spec: harnesstest.MCPSpec{Name: "big", Tools: bigTools()}, sse: true, toolLoading: "lazy"}

func replyTo(name, user, reply string) harnesstest.Step {
	return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: reply}}
}

func TestContractDeclaredPrefix(t *testing.T) {
	first, weather, text := forecastFirst, declaredWeather, replyTo
	forecast := mcpTool("weather", "forecast", "city", "Oslo")
	runScenarios(t, []scenario{
		{
			name:  "declared_tools_stay_the_same_after_a_restart_with_a_changed_description",
			setup: mcpSetup(nil, weather),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				setMCPTools{server: "weather", tools: weatherTools("A changed description", weatherAlerts)},
				restart{},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectTools{req: 2, sameAs: 1},
				expectSystem{req: 2, sameAs: 1},
				expectToolDescription{req: 2, tool: "mcp__weather__forecast", has: first},
			},
		},
		{
			name:  "a_tool_added_after_the_declaration_is_deferred_after_a_restart",
			setup: mcpSetup(nil, weather),
			model: afterFirstTurn(mcpAction("action", "call", "tool", "mcp__weather__alerts")),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"}, waitIdle{as: "a"},
				setMCPTools{server: "weather", tools: weatherTools(first, weatherAlerts)},
				restart{},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				expectTools{req: 2, lacks: []string{"mcp__weather__alerts"}},
				expectTools{req: 3, lacks: []string{"mcp__weather__alerts"}, text: []string{"no active alerts"}},
			},
		},
		{
			name:  "a_tool_that_the_declaration_lacks_is_selected_with_its_schema",
			setup: mcpSetup(nil, weather),
			model: afterFirstTurn(
				mcpAction("action", "select", "tools", []string{"mcp__weather__warnings"}),
				mcpAction("action", "call", "tool", "mcp__weather__warnings", "arguments", map[string]any{"region": "north"}),
			),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"}, waitIdle{as: "a"},
				setMCPTools{server: "weather", tools: weatherTools(first, weatherWarnings)},
				restart{},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				expectTools{req: 2, lacks: []string{"mcp__weather__warnings"}},
				expectTools{req: 3, text: []string{`"required":["region"]`, `"already":[]`}},
				expectTools{req: 4, text: []string{"no warnings"}},
			},
		},
		{
			name:  "a_declared_tool_that_is_gone_reads_no_such_tool_after_a_restart",
			setup: mcpSetup(nil, weather),
			model: afterFirstTurn(forecast),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"}, waitIdle{as: "a"},
				stopMCPServer{server: "weather"},
				restart{},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				expectTools{req: 2, has: []string{"mcp__weather__forecast"}},
				expectTools{req: 3, has: []string{"mcp__weather__forecast"}, text: []string{"no such tool available: mcp__weather__forecast"}},
			},
		},
		{
			name:    "a_backend_that_owns_the_loop_declares_nothing",
			driver:  claudeLaneDriver("normal"),
			actions: withActions(claudeOneTurn, claudeJournalEvents{as: "a", prefix: "prefix."}),
		},
	})
}

// callForecast scripts a model call that runs the forecast tool when the last user text is user, then replies with reply.
func callForecast(user, id, reply string) []harnesstest.Step {
	forecast := withID(id, mcpTool("weather", "forecast", "city", "Oslo"))
	return []harnesstest.Step{
		{Name: "call " + user, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{forecast}}},
		{Name: "done " + user, Match: harnesstest.LastToolResult("mcp__weather__forecast"), Reply: harnesstest.Reply{Text: reply}},
	}
}

func TestContractDeclaredPrefixAfterACompaction(t *testing.T) {
	first, weather, live, text := forecastFirst, declaredWeather, liveWeather, replyTo
	keepOne, summary := keepOneTurn, gistSummary
	toolsChange := compactionBreak()
	runScenarios(t, []scenario{
		{
			name: "a_compaction_between_turns_declares_the_live_tools", prefixBreaks: toolsChange,
			config: keepOne, setup: mcpSetup(nil, live),
			model: append(append([]harnesstest.Step{text("one", "one", "1")}, callForecast("two", "toolu_2", "2")...), summary, text("three", "three", "3")),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				changeMCPTools{server: "weather", tools: weatherTools(first, weatherAlerts)},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectTools{req: 3, sameAs: 1, lacks: []string{"mcp__weather__alerts"}},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectTools{req: 5, has: []string{"mcp__weather__alerts"}},
				journalPage{as: "a", limit: 500},
			},
		},
		{
			name: "a_compaction_with_no_change_keeps_the_tools", prefixBreaks: compactionBreak(),
			config: keepOne, setup: mcpSetup(nil, weather),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectTools{req: 4, sameAs: 2},
				expectSystem{req: 4, sameAs: 2},
				journalPage{as: "a", limit: 500},
			},
		},
		{
			name: "a_tool_change_inside_a_turn_waits_for_a_compaction", prefixBreaks: toolsChange,
			config: keepOne, setup: mcpSetup(nil, live),
			model: append(append(append([]harnesstest.Step{text("one", "one", "1")}, callForecast("two", "toolu_2", "2")...), summary),
				callForecast("three", "toolu_3", "3")...),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				changeMCPTools{server: "weather", tools: weatherTools(first, weatherAlerts)},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				compact{as: "a"},
				changeMCPTools{server: "weather", tools: weatherTools("A changed description", weatherAlerts, weatherSnow)},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectTools{req: 5, has: []string{"mcp__weather__alerts"}, lacks: []string{"mcp__weather__snow"}},
				expectTools{req: 6, sameAs: 5},
				expectToolDescription{req: 6, tool: "mcp__weather__forecast", has: first},
			},
		},
	})
}

func TestContractDeclaredPrefixLiveState(t *testing.T) {
	first, live, text := forecastFirst, liveWeather, replyTo
	keepOne, summary, toolsChange := keepOneTurn, gistSummary, compactionBreak()
	add := putMCP{servers: []string{"weather"}}
	runScenarios(t, []scenario{
		{
			name: "a_removed_last_server_leaves_the_next_declaration_with_no_mcp_tool", prefixBreaks: toolsChange,
			config: keepOne, setup: mcpSetup(nil, live),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3"), text("four", "four", "4")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				putMCP{servers: nil},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectTools{req: 2, sameAs: 1, has: []string{"mcp", "mcp__weather__forecast"}, text: []string{"[mcp: removed"}},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectTools{req: 4, lacks: []string{"mcp", "mcp__weather__forecast"}},
				add, awaitMCPConnected{put: add},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				expectTools{req: 5, sameAs: 4},
				expectNoText{req: 5, text: "mcp__weather__forecast"},
			},
		},
		{
			name: "a_compaction_between_turns_declares_a_changed_description", prefixBreaks: toolsChange,
			config: keepOne, setup: mcpSetup(nil, live),
			model: append(append([]harnesstest.Step{text("one", "one", "1")}, callForecast("two", "toolu_2", "2")...), summary, text("three", "three", "3")),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				changeMCPTools{server: "weather", tools: weatherTools("A changed description")},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectToolDescription{req: 3, tool: "mcp__weather__forecast", has: first},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectToolDescription{req: 5, tool: "mcp__weather__forecast", has: "A changed description"},
			},
		},
		{
			name: "a_session_that_does_not_compact_keeps_its_tools_when_another_declares", prefixBreaks: toolsChange,
			config: keepOne, setup: mcpSetup(nil, live),
			model: append(append([]harnesstest.Step{text("a one", "a one", "1"), text("b one", "b one", "1")}, callForecast("a two", "toolu_2", "2")...),
				summary, text("a three", "a three", "3"), text("b two", "b two", "2")),
			actions: []action{
				create{as: "a"}, create{as: "b"},
				submit{as: "a", text: "a one"}, waitIdle{as: "a"},
				submit{as: "b", text: "b one"}, waitIdle{as: "b"},
				changeMCPTools{server: "weather", tools: weatherTools("A changed description", weatherAlerts)},
				submit{as: "a", text: "a two"}, waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "a three"}, waitIdle{as: "a"},
				expectTools{req: 6, has: []string{"mcp__weather__alerts"}},
				submit{as: "b", text: "b two"}, waitIdle{as: "b"},
				expectTools{req: 7, sameAs: 2, lacks: []string{"mcp__weather__alerts"}},
				expectToolDescription{req: 7, tool: "mcp__weather__forecast", has: first},
			},
		},
	})
}

func TestContractDeclaredPrefixAtOtherChanges(t *testing.T) {
	first, weather, text := forecastFirst, declaredWeather, replyTo
	runScenarios(t, []scenario{
		{
			name: "a_model_change_declares_the_tools_of_the_new_model", prefixBreaks: prefixBreak("a model change declares the live tools", breakTools),
			setup: mcpSetup(nil, liveWeather),
			model: append(append([]harnesstest.Step{text("one", "one", "1")}, callForecast("two", "toolu_2", "2")...), text("three", "three", "3")),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				changeMCPTools{server: "weather", tools: weatherTools(first, weatherAlerts)},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectTools{req: 3, lacks: []string{"mcp__weather__alerts"}},
				setModel{as: "a", model: "anthropic/claude-opus-5"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectTools{req: 4, has: []string{"mcp__weather__alerts"}},
				journalPage{as: "a", limit: 500},
			},
		},
		{
			name: "a_changed_input_schema_declares_after_a_restart", prefixBreaks: prefixBreak("a changed input schema declares the live tools", breakTools),
			setup: mcpSetup(nil, weather),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				setMCPTools{server: "weather", tools: []harnesstest.MCPTool{{Name: "forecast", Description: first, InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"days":{"type":"integer"}}}`), Result: mcpText("Oslo: 3C, snow")}}},
				restart{},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectToolDescription{req: 2, tool: "mcp__weather__forecast", has: first, schema: `"days"`},
				journalPage{as: "a", limit: 500},
			},
		},
	})
}

func TestContractDeclaredPrefixPlugin(t *testing.T) {
	guarded := map[string]any{"plugins": []any{pluginEntry(t, "guard", nil)}, "compaction_keep_turns": 1}
	withFixture := putPlugins{plugins: []map[string]any{pluginEntry(t, "guard", nil), pluginEntry(t, "fixture", nil)}}
	fixtureTools := []string{"fixture_echo"}
	late, err := json.Marshal(map[string]any{"plugins": []any{pluginEntry(t, "late", map[string]any{"late_tool": "late_report"})}})
	if err != nil {
		t.Fatal(err)
	}
	lateProject := string(late)
	runScenarios(t, []scenario{
		{
			name:   "a_declared_mcp_tool_works_after_the_last_plugin_is_removed",
			config: pluginConfig(t, nil),
			model:  afterFirstTurn(mcpAction("action", "status")),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"}, waitIdle{as: "a"},
				putPlugins{plugins: []map[string]any{}},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				expectTools{req: 2, has: []string{"mcp"}},
				expectTools{req: 3, text: []string{`"servers"`}},
			},
		},
		{
			name: "a_plugin_added_in_a_running_process_is_declared_at_the_next_compaction", prefixBreaks: compactionBreak(),
			config: guarded,
			model:  []harnesstest.Step{replyTo("one", "one", "1"), replyTo("two", "two", "2"), gistSummary, replyTo("three", "three", "3")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				withFixture,
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectTools{req: 2, sameAs: 1, lacks: fixtureTools},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectTools{req: 4, has: fixtureTools},
			},
		},
		{
			name:   "a_plugin_tool_that_the_declaration_lacks_is_searched_selected_and_called",
			driver: inTreeWith(nil, "", pluginConfig(t, nil)),
			model: afterFirstTurn(
				mcpAction("action", "search", "query", "restart adds"),
				mcpAction("action", "select", "tools", []string{"late_report"}),
				mcpAction("action", "call", "tool", "late_report", "arguments", map[string]any{"region": "north"}),
			),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"}, waitIdle{as: "a"},
				writeFile{path: ".harness.json", body: lateProject},
				restart{},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				expectTools{req: 2, lacks: []string{"late_report"}},
				expectTools{req: 3, text: []string{"late_report"}},
				expectTools{req: 4, text: []string{`"required":["region"]`}},
				expectTools{req: 5, text: []string{"late tool ran"}},
			},
		},
	})
}

// expectNoText checks that the messages of request req lack text.
type expectNoText struct {
	req  int
	text string
}

func (a expectNoText) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) {
		t.Fatalf("expectNoText: request %d of %d", a.req, len(reqs))
	}
	if all := strings.Join(flat(reqs[a.req-1].Messages), "\n"); strings.Contains(all, a.text) {
		t.Errorf("request %d messages hold %q:\n%s", a.req, a.text, all)
	}
}

func TestContractDeclaredPrefixNotices(t *testing.T) {
	first, text := forecastFirst, replyTo
	unlisted := declaredWeather
	unlisted.unlisted = true
	lazy := declaredWeather
	lazy.toolLoading = "lazy"
	add := putMCP{servers: []string{"weather"}}
	runScenarios(t, []scenario{
		{
			name: "a_first_server_after_a_declaration_with_no_mcp_tool_gives_no_notice", prefixBreaks: compactionBreak(),
			setup:  mcpSetup(nil, unlisted),
			config: keepOneTurn,
			model:  []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), gistSummary, text("three", "three", "3")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				add, awaitMCPConnected{put: add},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectTools{req: 2, sameAs: 1, lacks: []string{"mcp"}},
				expectNoText{req: 2, text: "[mcp:"},
				expectNoText{req: 2, text: "mcp__weather__forecast"},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectTools{req: 4, has: []string{"mcp", "mcp__weather__forecast"}},
			},
		},
		{
			name:  "a_tool_added_to_a_server_with_a_cut_catalog_is_named_by_the_added_notice",
			setup: mcpSetup(nil, bigWeather),
			model: afterFirstTurn(mcpAction("action", "call", "tool", "mcp__big__t000")),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"}, waitIdle{as: "a"},
				changeMCPTools{server: "big", tools: append(bigTools(), harnesstest.MCPTool{Name: "newtool", Description: "A tool that arrives later", Result: mcpText("ok")})},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				expectTools{req: 1, lacks: []string{"mcp__big__newtool"}},
				expectTools{req: 3, text: []string{"mcp__big__newtool"}},
			},
		},
		{
			name:  "a_tool_added_to_a_lazy_server_is_named_by_the_added_notice",
			setup: mcpSetup(nil, lazy),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				setMCPTools{server: "weather", tools: weatherTools(first, weatherAlerts)},
				restart{},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectTools{req: 2, sameAs: 1, text: []string{"mcp__weather__alerts"}},
			},
		},
	})
}

// blobless hides the declaration blobs of a store.
type blobless struct{ harness.Store }

func (b blobless) GetBlob(ctx context.Context, session, key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "prefix-") {
		return nil, fs.ErrNotExist
	}
	return b.Store.GetBlob(ctx, session, key)
}

// newOwnerTools runs one turn on a first owner that syncs to a receiver, changes the tools of the MCP server, and runs one
// turn of a second owner on the store of the receiver. It returns the tools of each request and the records of the
// receiver after the second turn.
func newOwnerTools(t *testing.T, hideBlobs, syncSecond bool) (before, after json.RawMessage, records [][]byte, replicated func() (head, want uint64)) {
	t.Helper()
	srv := harnesstest.NewMCPServer(t, harnesstest.MCPSpec{Name: "weather", Tools: weatherTools("Get the weather forecast for a city")})
	srv.RequireAuthorization(mcpToken)
	fake := harnesstest.NewChat(t,
		harnesstest.Step{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: "1"}},
		harnesstest.Step{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "2"}})
	rcv := newSyncReceiver(t, nil)
	servers := map[string]any{"weather": map[string]any{"url": srv.URL(), "headers": map[string]string{"Authorization": mcpToken}}}
	cfg := syncConfig(t, 1, rcv, fake.URL(), map[string]any{"mcp_servers": servers})
	r1, err := newWired(t, harness.Options{Store: store.NewMemStore(), Config: cfg, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s1, err := r1.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Prompt(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "one"}}}); err != nil {
		t.Fatal(err)
	}
	awaitTurns(t, s1, 1)
	if err := r1.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	srv.SetTools(weatherTools("A changed description", weatherAlerts))
	st := rcv.store
	if hideBlobs {
		st = blobless{st}
	}
	cfg.Sync, cfg.OwnerEpoch = nil, 2
	rcv2 := newSyncReceiver(t, nil)
	if syncSecond {
		cfg = syncConfig(t, 2, rcv2, fake.URL(), map[string]any{"mcp_servers": servers})
	}
	r2, err := newWired(t, harness.Options{Store: st, Config: cfg, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close(context.Background()) })
	s2, err := r2.Open(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Prompt(t.Context(), protocol.Input{ID: "b", Parts: []protocol.Part{{Type: protocol.PartText, Text: "two"}}}); err != nil {
		t.Fatal(err)
	}
	awaitTurns(t, s2, 2)
	var tools []json.RawMessage
	for _, raw := range fake.RawRequests() {
		if got := first(raw, "tools"); len(got) > 0 && string(got) != "null" {
			tools = append(tools, got)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("model requests with tools = %d, want 2", len(tools))
	}
	replicated = func() (uint64, uint64) {
		ctx, cancel := context.WithTimeout(t.Context(), waitBound)
		defer cancel()
		closed := make(chan error, 1)
		go func() { closed <- r2.Close(ctx) }()
		select {
		case err := <-closed:
			if err != nil {
				t.Errorf("Close = %v, want the final sync to complete", err)
			}
		case <-ctx.Done():
			t.Error("Close did not finish: replication stalls")
		}
		head, _ := rcv2.store.Head(t.Context(), "s1")
		want, _ := rcv.store.Head(t.Context(), "s1")
		return head, want
	}
	return tools[0], tools[1], readAll(t, rcv.store, "s1"), replicated
}

func rowANewOwnerSendsTheDeclaredTools(t *testing.T) {
	before, after, records, _ := newOwnerTools(t, false, false)
	if !bytes.Equal(before, after) {
		t.Errorf("the tools of the new owner differ from the first owner:\n got %s\nwant %s", after, before)
	}
	if n := declarations(records); n != 1 {
		t.Errorf("the log holds %d prefix.declared records, want 1", n)
	}
}

func rowANewOwnerWithoutTheBlobDeclaresAgain(t *testing.T) {
	before, after, records, _ := newOwnerTools(t, true, false)
	if bytes.Equal(before, after) || !bytes.Contains(after, []byte("mcp__weather__alerts")) {
		t.Errorf("the new owner without the blob sent the declared tools, want the live tools:\n got %s", after)
	}
	if n := declarations(records); n != 2 {
		t.Errorf("the log holds %d prefix.declared records, want 2", n)
	}
}

func declarations(records [][]byte) int {
	n := 0
	for _, r := range records {
		if bytes.Contains(r, []byte(`"k":"prefix.declared"`)) {
			n++
		}
	}
	return n
}

func rowANewOwnerWithoutTheBlobStillReplicates(t *testing.T) {
	_, _, _, replicated := newOwnerTools(t, true, true)
	if head, want := replicated(); want == 0 || head != want {
		t.Errorf("the receiver of the new owner has head %d, want %d", head, want)
	}
}
