package claudecode

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

func TestUserLineSendsAnEngineContextPartAsRawText(t *testing.T) {
	m := protocol.LogMessage{Parts: []protocol.LogPart{
		{Type: protocol.LogPartText, Text: "OPERATOR MESSAGES (address these, then continue the task):\n1. go on\n"},
		{Type: protocol.LogPartEngineContext, Text: "[tasks:\n- kid (agent=explore) done: found it (usage: 1 in / 2 out)\n]"},
	}}
	line, err := userLine(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"user","message":{"role":"user","content":"OPERATOR MESSAGES (address these, then continue the task):\n1. go on\n\n\n[tasks:\n- kid (agent=explore) done: found it (usage: 1 in / 2 out)\n]"}}`
	if got, _ := json.Marshal(line); string(got) != want {
		t.Errorf("line = %s, want %s", got, want)
	}
}

func TestSearchPartReadsTheLinksOfAWebSearchResult(t *testing.T) {
	text := "Web search results for query: \"go\"\n\nLinks: [{\"title\":\"Go\",\"url\":\"https://go.dev\"}]\n\nGo is current."
	for name, tc := range map[string]struct {
		res  protocol.LogPart
		want protocol.LogPart
	}{
		"links":    {protocol.LogPart{Text: text}, protocol.LogPart{Results: []protocol.SearchResult{{Title: "Go", URL: "https://go.dev"}}}},
		"cut list": {protocol.LogPart{Text: "Links: [{\"title\":\"Go\",\"ur..."}, protocol.LogPart{}},
		"no list":  {protocol.LogPart{Text: "nothing"}, protocol.LogPart{}},
		"an error": {protocol.LogPart{Text: "boom", IsError: true}, protocol.LogPart{Text: "boom", IsError: true}},
	} {
		tc.want.Type, tc.want.CallID, tc.want.Query = protocol.LogPartWebSearch, "toolu_1", "go"
		if got := searchPart("toolu_1", "go", tc.res, nil); !slices.Equal(got.Results, tc.want.Results) || got.Text != tc.want.Text || got.IsError != tc.want.IsError || got.Query != "go" || got.Type != tc.want.Type {
			t.Errorf("%s: part = %+v, want %+v", name, got, tc.want)
		}
	}
}

func TestAllowSearchAddsWebSearchToTheApprovedTools(t *testing.T) {
	for name, tc := range map[string]struct {
		args   []string
		search bool
		want   []string
	}{
		"off":            {[]string{"--mcp-config", "c"}, false, []string{"--mcp-config", "c"}},
		"no flag":        {nil, true, []string{"--allowedTools", "WebSearch"}},
		"beside the MCP": {[]string{"--mcp-config", "c", "--allowedTools", "mcp__harness"}, true, []string{"--mcp-config", "c", "--allowedTools", "mcp__harness,WebSearch"}},
	} {
		if got := allowSearch(tc.args, tc.search); !slices.Equal(got, tc.want) {
			t.Errorf("%s: args = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSearchPartPrefersTheStructuredResults(t *testing.T) {
	raw := json.RawMessage(`{"query":"go","results":[{"content":[{"title":"A","url":"https://a.example"}]},"a note"]}`)
	structured := structuredResults(raw)
	got := searchPart("toolu_1", "go", protocol.LogPart{Text: "Links: [{\"title\":\"B\",\"url\":\"https://b.example\"}]"}, structured)
	if len(got.Results) != 1 || got.Results[0].URL != "https://a.example" {
		t.Errorf("results = %+v, want the structured source", got.Results)
	}
}

func TestToolResultNamesTheToolsThatToolSearchLoaded(t *testing.T) {
	content := json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":"mcp__harness__tool_7"}]}]`)
	parts := toolResults(wireMessage{Content: content}, map[string]string{"toolu_1": "ToolSearch"})
	if len(parts) != 1 || parts[0].Text != "mcp__harness__tool_7" {
		t.Errorf("parts = %+v, want one result that names mcp__harness__tool_7", parts)
	}
}

func TestResultUsageReadsModelUsageWhenUsageIsZero(t *testing.T) {
	var env envelope
	line := `{"type":"result","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},` +
		`"modelUsage":{"claude-sonnet-5-5":{"inputTokens":2000,"outputTokens":723,"cacheReadInputTokens":24728,"cacheCreationInputTokens":66109}}}`
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatal(err)
	}
	r := &run{}
	want := protocol.Usage{InputTokens: 2000, OutputTokens: 723, CacheReadTokens: 24728, CacheWriteTokens: 66109}
	if got := r.resultUsage(env); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
	if got := r.resultUsage(env); got != (protocol.Usage{}) {
		t.Errorf("second result with the same running total = %+v, want zero", got)
	}
}
