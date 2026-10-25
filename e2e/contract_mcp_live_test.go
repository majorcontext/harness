package e2e

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

// putMCP sends PUT /mcp/servers. servers names the fixtures that the live set holds, and edit changes the entry of
// one. body, when set, is sent as it is.
type putMCP struct {
	servers []string
	edit    map[string]func(map[string]any)
	body    any
	status  int
}

func (a putMCP) request(t *testing.T, r *run) any {
	t.Helper()
	body := a.body
	if body == nil {
		entries := map[string]any{}
		for _, name := range a.servers {
			entry := map[string]any{"url": mcpFixture(t, r, name).URL(), "headers": map[string]string{"Authorization": mcpToken}}
			if edit := a.edit[name]; edit != nil {
				edit(entry)
			}
			entries[name] = entry
		}
		body = map[string]any{"mcp_servers": entries}
	}
	return body
}

func (a putMCP) run(t *testing.T, r *run) {
	t.Helper()
	res := r.drv.PutMCPServers(t, a.request(t, r))
	if want := cmpStatus(a.status); res.Status != want {
		t.Fatalf("PUT /mcp/servers = %d %v, want %d", res.Status, res.Body, want)
	}
	if res.Status != http.StatusOK {
		r.record(t, "put_mcp_servers", "", res)
		return
	}
	names := []string{}
	list, _ := res.Body.(map[string]any)["servers"].([]any)
	for _, e := range list {
		entry, _ := e.(map[string]any)
		for _, k := range []string{"name", "connected", "attempts", "parked"} {
			if _, ok := entry[k]; !ok {
				t.Fatalf("PUT /mcp/servers entry %v lacks %q", entry, k)
			}
		}
		names = append(names, fmt.Sprint(entry["name"]))
	}
	if !slices.IsSorted(names) {
		t.Fatalf("PUT /mcp/servers servers %v are not in name order", names)
	}
	r.record(t, "put_mcp_servers", "", callResult{Status: res.Status, Body: names})
}

// awaitMCPConnected waits until every server that put names is connected. It sends put again, which changes nothing
// because the set is the same, and reads the status in the answer: the serve host has no other view of the set.
type awaitMCPConnected struct{ put putMCP }

func (a awaitMCPConnected) run(t *testing.T, r *run) {
	t.Helper()
	body := a.put.request(t, r)
	testpoll.Until(t, waitBound, "the added MCP servers did not connect", func() bool {
		res := r.drv.PutMCPServers(t, body)
		list, _ := res.Body.(map[string]any)["servers"].([]any)
		for _, e := range list {
			if entry, _ := e.(map[string]any); entry["connected"] != true {
				return false
			}
		}
		return res.Status == http.StatusOK
	}, 20*time.Millisecond)
}

func cmpStatus(s int) int {
	if s == 0 {
		return http.StatusOK
	}
	return s
}

// changeMCPTools makes a fixture serve tools, and makes its next tools/call answer carry a list_changed notification.
type changeMCPTools struct {
	server string
	tools  []harnesstest.MCPTool
}

func (a changeMCPTools) run(t *testing.T, r *run) {
	t.Helper()
	srv := mcpFixture(t, r, a.server)
	srv.SetTools(a.tools)
	srv.NotifyToolsChanged()
}

type setMCPTools struct {
	server string
	tools  []harnesstest.MCPTool
}

func (a setMCPTools) run(t *testing.T, r *run) {
	t.Helper()
	mcpFixture(t, r, a.server).SetTools(a.tools)
}

type expectMCPInitializes struct {
	server string
	n      int
}

func (a expectMCPInitializes) run(t *testing.T, r *run) {
	t.Helper()
	if got := mcpFixture(t, r, a.server).Initializes(); got != a.n {
		t.Errorf("server %s answered %d initialize requests, want %d", a.server, got, a.n)
	}
}

// afterFirstTurn scripts a text reply for the first model request, and one tool call for each request after it.
func afterFirstTurn(calls ...harnesstest.ToolCall) []harnesstest.Step {
	steps := []harnesstest.Step{{Name: "first", Match: assistantTurns(0), Reply: harnesstest.Reply{Text: "hi"}}}
	for i, c := range calls {
		c.ID = fmt.Sprintf("toolu_%d", i+1)
		steps = append(steps, harnesstest.Step{Name: fmt.Sprintf("call%d", i+1), Match: assistantTurns(i + 1), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{c}}})
	}
	return append(steps, harnesstest.Step{Name: "done", Match: assistantTurns(len(calls) + 1), Reply: harnesstest.Reply{Text: "done"}})
}

func liveTurns(between ...action) []action {
	acts := []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}
	acts = append(acts, between...)
	return append(acts, submit{as: "a", text: "again"}, waitIdle{as: "a"})
}

// mcpLiveRows is the disposition of the rows of the live MCP server set.
var mcpLiveRows = map[string]runtimeRow{
	"mcp_http_sse_server_gets_the_work_dir_as_a_root":                     sameAsServe(),
	"mcp_stdio_server_gets_the_work_dir_as_a_root":                        sameAsServe(),
	"mcp_select_reads_the_live_set_when_it_runs":                          sameAsServe(),
	"mcp_session_that_began_with_an_empty_set_keeps_that_after_a_reopen":  sameAsServe(),
	"mcp_put_before_any_call_sets_the_tool_loading_of_the_new_spec":       sameAsServe(),
	"mcp_put_connects_a_new_server_before_any_model_call":                 sameAsServe(),
	"mcp_put_adds_a_server_whose_tools_arrive_deferred":                   sameAsServe(),
	"mcp_put_removes_a_server_and_a_sent_tool_stays_in_the_definitions":   sameAsServe(),
	"mcp_put_with_a_changed_spec_connects_again":                          sameAsServe(),
	"mcp_put_with_the_same_spec_keeps_the_client_and_the_call_in_flight":  sameAsServe(),
	"mcp_put_that_removes_a_server_ends_the_call_in_flight_with_an_error": sameAsServe(),
	"mcp_list_changed_adds_a_deferred_tool_that_the_notice_names":         sameAsServe(),
	"mcp_missing_tool_refreshes_a_server_without_list_changed":            sameAsServe(),
	"mcp_missing_tool_call_refreshes_a_server_without_list_changed":       sameAsServe(),
	"mcp_put_with_a_bad_body_is_invalid_and_keeps_the_set":                sameAsServe(),
	"mcp_session_that_began_with_an_empty_set_never_gets_mcp_tools":       sameAsServe(),
}

func withRows(parts ...map[string]runtimeRow) map[string]runtimeRow {
	out := map[string]runtimeRow{}
	for _, p := range parts {
		maps.Copy(out, p)
	}
	return out
}

func TestContractMCPLiveSet(t *testing.T) {
	weather := mcpServerDef{name: "weather", spec: mcpWeather("")}
	docs := mcpServerDef{name: "docs", spec: mcpDocs()}
	unlisted := docs
	unlisted.unlisted = true
	add := putMCP{servers: []string{"weather", "docs"}}
	changed := putMCP{servers: []string{"weather"}, edit: map[string]func(map[string]any){
		"weather": func(e map[string]any) { e["connect_timeout_s"] = 20 },
	}}
	runScenarios(t, []scenario{
		{
			name:  "mcp_put_adds_a_server_whose_tools_arrive_deferred",
			setup: mcpSetup(nil, weather, unlisted),
			model: afterFirstTurn(
				mcpAction("action", "connect", "server", "docs"),
				mcpAction("action", "select", "tools", []string{"mcp__docs__search"}),
				mcpAction("action", "call", "tool", "mcp__docs__search"),
				mcpAction("action", "status"),
			),
			actions: append(liveTurns(add, awaitMCPConnected{put: add}),
				expectSystem{req: 2, sameAs: 1, lacks: []string{"Deferred MCP tools", "Read doc://guide"}},
			),
		},
		{
			name:  "mcp_put_removes_a_server_and_a_sent_tool_stays_in_the_definitions",
			setup: mcpSetup(nil, weather, docs),
			model: afterFirstTurn(
				mcpTool("docs", "search"),
				mcpAction("action", "status"),
				mcpAction("action", "connect", "server", "docs"),
			),
			actions: append(liveTurns(putMCP{servers: []string{"weather"}}), expectSystem{req: 2, sameAs: 1}),
		},
		{
			name:  "mcp_put_with_a_changed_spec_connects_again",
			setup: mcpSetup(nil, weather),
			model: afterFirstTurn(
				mcpAction("action", "connect", "server", "weather"),
				mcpTool("weather", "forecast", "city", "Oslo"),
			),
			actions: append(liveTurns(changed, awaitMCPConnected{put: changed}), expectMCPInitializes{server: "weather", n: 2}),
		},
		{
			name:  "mcp_put_with_the_same_spec_keeps_the_client_and_the_call_in_flight",
			setup: mcpSetup(nil, gateServer),
			model: oneBatch(holdCall("a")),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"},
				awaitHeld{server: "gate", n: 1},
				putMCP{servers: []string{"gate"}},
				releaseCall{server: "gate", id: "a"},
				waitIdle{as: "a"},
				expectMCPInitializes{server: "gate", n: 1},
			},
		},
	})
}

func TestContractMCPLiveSetCalls(t *testing.T) {
	weather := mcpServerDef{name: "weather", spec: mcpWeather("")}
	docs := mcpServerDef{name: "docs", spec: mcpDocs()}
	unlisted := docs
	unlisted.unlisted = true
	sse := mcpServerDef{name: "weather", spec: mcpWeather(""), sse: true}
	snow := append(mcpWeather("").Tools, harnesstest.MCPTool{Name: "snow", Description: "Report the snowfall", Echo: true})
	runScenarios(t, []scenario{
		{
			name:  "mcp_put_that_removes_a_server_ends_the_call_in_flight_with_an_error",
			setup: mcpSetup(nil, gateServer),
			model: oneBatch(holdCall("a")),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"},
				awaitHeld{server: "gate", n: 1},
				putMCP{servers: nil},
				awaitAbandoned{server: "gate", n: 1},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "mcp_list_changed_adds_a_deferred_tool_that_the_notice_names",
			setup: mcpSetup(nil, sse),
			model: afterFirstTurn(
				mcpTool("weather", "echo", "city", "Oslo"),
				mcpAction("action", "select", "tools", []string{"mcp__weather__snow"}),
				mcpAction("action", "call", "tool", "mcp__weather__snow", "arguments", map[string]any{"city": "Oslo"}),
			),
			actions: append(liveTurns(changeMCPTools{server: "weather", tools: snow}), expectSystem{req: 2, sameAs: 1}),
		},
		{
			name:  "mcp_missing_tool_refreshes_a_server_without_list_changed",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather(""), noListChanged: true}),
			model: afterFirstTurn(
				mcpAction("action", "select", "tools", []string{"mcp__weather__snow"}),
				mcpAction("action", "call", "tool", "mcp__weather__snow", "arguments", map[string]any{"city": "Oslo"}),
			),
			actions: liveTurns(setMCPTools{server: "weather", tools: snow}),
		},
		{
			name:  "mcp_missing_tool_call_refreshes_a_server_without_list_changed",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather(""), noListChanged: true}),
			model: afterFirstTurn(
				mcpAction("action", "call", "tool", "mcp__weather__snow", "arguments", map[string]any{"city": "Oslo"}),
				ftTool("mcp__weather__snow", ftArgs("city", "Bergen")),
			),
			actions: liveTurns(setMCPTools{server: "weather", tools: snow}),
		},
		{
			name:  "mcp_put_with_a_bad_body_is_invalid_and_keeps_the_set",
			setup: mcpSetup(nil, weather, unlisted),
			model: afterFirstTurn(mcpAction("action", "status")),
			actions: liveTurns(
				putMCP{body: map[string]any{}, status: http.StatusBadRequest},
				putMCP{body: map[string]any{"mcp_servers": nil}, status: http.StatusBadRequest},
				putMCP{body: map[string]any{"mcp_servers": map[string]any{"bad": map[string]any{}}}, status: http.StatusBadRequest},
				putMCP{body: map[string]any{"mcp_servers": map[string]any{"a__b": map[string]any{"url": "http://127.0.0.1:1"}}}, status: http.StatusBadRequest},
				putMCP{body: map[string]any{"mcp_servers": map[string]any{"weather": map[string]any{"url": "http://127.0.0.1:1", "tool_loading": "auto"}}}, status: http.StatusBadRequest},
			),
		},
	})
}

func TestContractMCPPutBeforeAnyCall(t *testing.T) {
	docs := mcpServerDef{name: "docs", spec: mcpDocs(), unlisted: true}
	add := putMCP{servers: []string{"docs"}}
	lazy := putMCP{servers: []string{"weather"}, edit: map[string]func(map[string]any){"weather": func(e map[string]any) { e["tool_loading"] = "lazy" }}}
	runScenarios(t, []scenario{
		{
			name:    "mcp_put_before_any_call_sets_the_tool_loading_of_the_new_spec",
			setup:   mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather("")}),
			model:   textReply("hi"),
			actions: append([]action{lazy}, append(oneTurn, expectSystem{req: 1, has: []string{"Deferred MCP tools", "mcp__weather__forecast \u2014"}})...),
		},
		{
			name:    "mcp_put_connects_a_new_server_before_any_model_call",
			setup:   mcpSetup(nil, docs),
			actions: []action{add, awaitMCPConnected{put: add}, expectMCPInitializes{server: "docs", n: 1}},
		},
	})
}

func TestContractMCPLiveSetTiming(t *testing.T) {
	weather := mcpServerDef{name: "weather", spec: mcpWeather("")}
	docs := mcpServerDef{name: "docs", spec: mcpDocs(), unlisted: true}
	add := putMCP{servers: []string{"weather", "docs"}}
	sel := mcpAction("action", "select", "tools", []string{"mcp__docs__search"})
	sel.ID = "toolu_1"
	runScenarios(t, []scenario{
		{
			name:  "mcp_select_reads_the_live_set_when_it_runs",
			setup: mcpSetup(nil, weather, docs),
			model: []harnesstest.Step{
				{Name: "first", Match: assistantTurns(0), Reply: harnesstest.Reply{Text: "hi"}},
				{Name: "sel", Match: assistantTurns(1), Reply: harnesstest.Reply{Text: "loading", Block: true, ToolCalls: []harnesstest.ToolCall{sel}}},
				{Name: "done", Match: assistantTurns(2), Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
				submit{as: "a", text: "again"}, awaitRequests{n: 2},
				add, awaitMCPConnected{put: add},
				release{step: "sel"}, waitIdle{as: "a"},
			},
		},
	})
}

func TestContractMCPEmptySetSurface(t *testing.T) {
	docs := mcpServerDef{name: "docs", spec: mcpDocs(), unlisted: true}
	add := putMCP{servers: []string{"docs"}}
	runScenarios(t, []scenario{
		{
			name:  "mcp_session_that_began_with_an_empty_set_keeps_that_after_a_reopen",
			setup: mcpSetup(nil, docs),
			model: []harnesstest.Step{
				{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: "1"}},
				{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "2"}},
			},
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"},
				endSession{as: "a"}, add,
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
			},
		},
		{
			name:  "mcp_session_that_began_with_an_empty_set_never_gets_mcp_tools",
			setup: mcpSetup(nil, docs),
			model: []harnesstest.Step{
				{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: "1"}},
				{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "2"}},
				{Name: "three", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "3"}},
			},
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"},
				add,
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				create{as: "b"}, submit{as: "b", text: "three"}, waitIdle{as: "b"},
			},
		},
	})
}
