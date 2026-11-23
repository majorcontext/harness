package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// noticeCleared is the text that the MCP down notice turns into when every server is connected.
const noticeCleared = "[mcp: every server is connected again.]"

// durableNoticeRows is the disposition of the rows of the durable notices.
var durableNoticeRows = map[string]runtimeRow{
	"a_down_server_notice_is_sent_once_after_a_restart":                            sameAsServe(),
	"a_connected_server_turns_the_down_notice_into_one_cleared_notice":             runtimeOnly("mcp-retry-held"),
	"an_added_tools_notice_is_sent_once_after_a_restart":                           sameAsServe(),
	"a_notice_that_the_log_never_recorded_follows_the_history_of_a_new_call":       sameAsServe(),
	"a_compaction_keeps_the_down_notice_while_the_server_is_down":                  sameAsServe(),
	"a_compaction_drops_the_added_notice_that_the_new_declaration_states":          sameAsServe(),
	"a_compaction_drops_the_removed_server_notice_that_the_declaration_made_stale": sameAsServe(),
	"a_declaration_without_a_compaction_retires_the_added_notice_that_it_states":   sameAsServe(),
	"a_declaration_with_no_mcp_surface_retires_the_recorded_down_notice":           sameAsServe(),
	"a_server_that_is_removed_again_after_a_restart_is_named_again":                sameAsServe(),
	"a_server_that_is_removed_again_after_a_declaration_is_named_again":            sameAsServe(),
}

// expectNoticeBlocks checks that request req holds n engine-context blocks that hold marker.
type expectNoticeBlocks struct {
	req    int
	marker string
	n      int
}

func (a expectNoticeBlocks) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) {
		t.Fatalf("expectNoticeBlocks: request %d of %d", a.req, len(reqs))
	}
	n := 0
	for _, m := range reqs[a.req-1].Messages {
		for _, p := range m.Parts {
			if isEngineContext(p) && strings.Contains(p.Text, a.marker) {
				n++
			}
		}
	}
	if n != a.n {
		t.Errorf("request %d holds %d engine-context blocks with %q, want %d:\n%s", a.req, n, a.marker, a.n, strings.Join(flat(reqs[a.req-1].Messages), "\n"))
	}
}

// expectExtends checks that the messages of request prev are a prefix of the messages of request req.
type expectExtends struct{ req, prev int }

func (a expectExtends) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) || a.prev < 1 || a.prev > len(reqs) {
		t.Fatalf("expectExtends: requests %d and %d of %d", a.req, a.prev, len(reqs))
	}
	before, got := flat(reqs[a.prev-1].Messages), flat(reqs[a.req-1].Messages)
	if len(got) < len(before) || !slices.Equal(got[:len(before)], before) {
		t.Errorf("request %d does not begin with the messages of request %d:\n%q\n---\n%q", a.req, a.prev, before, got)
	}
}

const (
	noticeDown  = "[mcp: unavailable"
	noticeAdded = "tools added after the tool list"
)

var downWeather = mcpServerDef{name: "weather", spec: mcpWeather(""), down: true}

func TestContractDurableNoticesAfterARestart(t *testing.T) {
	text, weather, live := replyTo, declaredWeather, liveWeather
	runScenarios(t, []scenario{
		{
			name:  "a_down_server_notice_is_sent_once_after_a_restart",
			setup: mcpSetup(nil, downWeather),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 1, marker: noticeDown, n: 1},
				expectNoticeBlocks{req: 2, marker: noticeDown, n: 1},
				expectExtends{req: 2, prev: 1},
			},
		},
		{
			name:  "an_added_tools_notice_is_sent_once_after_a_restart",
			setup: mcpSetup(nil, live),
			model: append(append([]harnesstest.Step{text("one", "one", "1")}, callForecast("two", "toolu_2", "2")...), text("three", "three", "3")),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				changeMCPTools{server: "weather", tools: weatherTools(forecastFirst, weatherAlerts)},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 3, marker: noticeAdded, n: 1},
				expectNoticeBlocks{req: 4, marker: noticeAdded, n: 1},
				expectExtends{req: 4, prev: 3},
			},
		},
		{
			name:  "a_notice_that_the_log_never_recorded_follows_the_history_of_a_new_call",
			setup: mcpSetup(nil, weather),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), text("three", "three", "3")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				setMCPTools{server: "weather", tools: weatherTools(forecastFirst, weatherAlerts)},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 2, marker: noticeAdded, n: 0},
				expectNoticeBlocks{req: 3, marker: noticeAdded, n: 1},
				expectExtends{req: 3, prev: 2},
			},
		},
	})
}

func TestContractDurableNoticesCleared(t *testing.T) {
	text, keepOne, summary := replyTo, keepOneTurn, gistSummary
	connectModel := []harnesstest.Step{
		{Name: "connect", Match: unansweredPrompt("go"), Reply: harnesstest.Reply{ToolCalls: callWithID("toolu_connect", mcpAction("action", "connect", "server", "weather"))}},
		{Name: "connected", Match: answeredTool("mcp"), Reply: harnesstest.Reply{Text: "connected"}},
		text("again", "again", "ok"),
	}
	runScenarios(t, []scenario{
		{
			name:  "a_connected_server_turns_the_down_notice_into_one_cleared_notice",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather(""), failInit: 1}),
			model: connectModel,
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"}, waitIdle{as: "a"},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 1, marker: noticeDown, n: 1},
				expectNoticeBlocks{req: 1, marker: noticeCleared, n: 0},
				expectNoticeBlocks{req: 2, marker: noticeCleared, n: 1},
				expectNoticeBlocks{req: 3, marker: noticeCleared, n: 1},
				expectExtends{req: 3, prev: 2},
			},
		},
		{
			name: "a_compaction_keeps_the_down_notice_while_the_server_is_down", prefixBreaks: compactionBreak(),
			config: keepOne, setup: mcpSetup(nil, downWeather),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 2, marker: noticeDown, n: 1},
				expectNoticeBlocks{req: 4, marker: noticeDown, n: 1},
			},
		},
	})
}

func TestContractDurableNoticesAfterACompaction(t *testing.T) {
	text, keepOne, summary, live := replyTo, keepOneTurn, gistSummary, liveWeather
	removedLive := mcpServerDef{name: "docs", spec: mcpDocs(), sse: true}
	keepDocs := putMCP{servers: []string{"docs"}}
	runScenarios(t, []scenario{
		{
			name: "a_compaction_drops_the_added_notice_that_the_new_declaration_states", prefixBreaks: compactionBreak(),
			config: keepOne, setup: mcpSetup(nil, live),
			model: append(append([]harnesstest.Step{text("one", "one", "1")}, callForecast("two", "toolu_2", "2")...), summary, text("three", "three", "3"), text("four", "four", "4")),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				changeMCPTools{server: "weather", tools: weatherTools(forecastFirst, weatherAlerts)},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 3, marker: noticeAdded, n: 1},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				expectTools{req: 5, has: []string{"mcp__weather__alerts"}},
				expectNoticeBlocks{req: 5, marker: noticeAdded, n: 0},
				expectNoticeBlocks{req: 6, marker: noticeAdded, n: 0},
				expectExtends{req: 6, prev: 5},
			},
		},
		{
			name: "a_compaction_drops_the_removed_server_notice_that_the_declaration_made_stale", prefixBreaks: compactionBreak(),
			config: keepOne, setup: mcpSetup(nil, live, removedLive),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3"), text("four", "four", "4")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				keepDocs,
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 2, marker: "[mcp: removed", n: 1},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 4, marker: "[mcp: removed", n: 0},
				expectNoticeBlocks{req: 5, marker: "[mcp: removed", n: 0},
				expectExtends{req: 5, prev: 4},
			},
		},
	})
}

func TestContractDurableNoticesAfterARemoval(t *testing.T) {
	text, keepOne, summary, live := replyTo, keepOneTurn, gistSummary, liveWeather
	removedLive := mcpServerDef{name: "docs", spec: mcpDocs(), sse: true}
	keepDocs := putMCP{servers: []string{"docs"}}
	runScenarios(t, []scenario{
		{
			name: "a_server_that_is_removed_again_after_a_declaration_is_named_again", prefixBreaks: compactionBreak(),
			config: keepOne, setup: mcpSetup(nil, live, removedLive),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3"), text("four", "four", "4"), text("five", "five", "5")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				keepDocs,
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				putMCP{servers: []string{"weather", "docs"}}, awaitMCPConnected{put: putMCP{servers: []string{"weather", "docs"}}},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 5, marker: noticeAdded, n: 1},
				keepDocs,
				submit{as: "a", text: "five"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 6, marker: "[mcp: removed", n: 1},
			},
		},
		{
			name: "a_server_that_is_removed_again_after_a_restart_is_named_again", prefixBreaks: compactionBreak(),
			config: keepOne, setup: mcpSetup(nil, live, removedLive),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3"), text("four", "four", "4"), text("five", "five", "5")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				keepDocs,
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 5, marker: noticeAdded, n: 1},
				keepDocs,
				submit{as: "a", text: "five"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 6, marker: "[mcp: removed", n: 1},
			},
		},
		{
			name: "a_declaration_with_no_mcp_surface_retires_the_recorded_down_notice", prefixBreaks: compactionBreak(),
			config: keepOne, setup: mcpSetup(nil, downWeather),
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3"), text("four", "four", "4")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 1, marker: noticeDown, n: 1},
				putMCP{servers: nil},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 4, marker: "[mcp:", n: 0},
				restart{},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 5, marker: "[mcp:", n: 0},
			},
		},
	})
}

func TestContractDurableNoticesAfterADeclaration(t *testing.T) {
	text := replyTo
	changed := []harnesstest.MCPTool{{Name: "forecast", Description: forecastFirst, InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"days":{"type":"integer"}}}`), Result: mcpText("Oslo: 3C, snow")}, weatherAlerts}
	runScenarios(t, []scenario{
		{
			name: "a_declaration_without_a_compaction_retires_the_added_notice_that_it_states", prefixBreaks: prefixBreak("a changed input schema declares the live tools and retires the notice that the declaration states", breakTools, breakHistory),
			setup: mcpSetup(nil, liveWeather),
			model: append(append([]harnesstest.Step{text("one", "one", "1")}, callForecast("two", "toolu_2", "2")...), text("three", "three", "3")),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				changeMCPTools{server: "weather", tools: changed},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectNoticeBlocks{req: 3, marker: noticeAdded, n: 1},
				restart{},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectTools{req: 4, has: []string{"mcp__weather__alerts"}},
				expectNoticeBlocks{req: 4, marker: noticeAdded, n: 0},
			},
		},
	})
}
