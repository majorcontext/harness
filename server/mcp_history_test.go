package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/message"
)

// seedMessages builds a small, readable history: a user question, an
// assistant reply that calls a tool, the tool's own result, a reasoning
// block, and a final assistant text reply — enough shapes to exercise
// every branch of writeFlattenedMessage in one seed.
func seedMessages() []message.Message {
	return []message.Message{
		{ID: "1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "what is in the repo root?"}}},
		{ID: "2", Role: message.RoleAssistant, Parts: message.Parts{
			&message.Reasoning{Text: "I should list the directory."},
			&message.ToolCall{CallID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
		}},
		{ID: "3", Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "README.md\nmain.go"}}},
		}},
		{ID: "4", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "The repo root has README.md and main.go."}}},
	}
}

// TestFlattenHistoryRendersRolesReadably proves every message shape
// (user text, assistant reasoning, assistant tool call, tool result,
// final assistant text) renders into a readable, role-labeled line.
func TestFlattenHistoryRendersRolesReadably(t *testing.T) {
	page := flattenHistory(seedMessages(), 0, 0)
	if page.Total != 4 || page.Returned != 4 || page.HasMore {
		t.Fatalf("page = %+v, want Total=4 Returned=4 HasMore=false", page)
	}

	wantSubstrings := []string{
		"User: what is in the repo root?",
		"Assistant: [thinking]",
		`Assistant called tool bash({"command":"ls"})`,
		"Tool result (bash, ok): README.md\nmain.go",
		"Assistant: The repo root has README.md and main.go.",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(page.Text, want) {
			t.Errorf("flattened text missing %q\ngot:\n%s", want, page.Text)
		}
	}
}

// TestFlattenHistoryToolResultErrorStatus proves an IsError ToolResult is
// labeled "error", not "ok".
func TestFlattenHistoryToolResultErrorStatus(t *testing.T) {
	msgs := []message.Message{
		{ID: "1", Role: message.RoleAssistant, Parts: message.Parts{&message.ToolCall{CallID: "c1", Name: "bash", Arguments: json.RawMessage(`{}`)}}},
		{ID: "2", Role: message.RoleTool, Parts: message.Parts{&message.ToolResult{CallID: "c1", Content: message.Parts{&message.Text{Text: "command not found"}}, IsError: true}}},
	}
	page := flattenHistory(msgs, 0, 0)
	if !strings.Contains(page.Text, "Tool result (bash, error): command not found") {
		t.Errorf("flattened text = %q, want an (bash, error) tool result line", page.Text)
	}
}

// TestFlattenHistoryToolNameResolvedAcrossPageBoundary proves a
// ToolResult's tool name is still resolved correctly even when its
// matching ToolCall fell on an EARLIER page than the one being rendered —
// a caller paging through a long history one chunk at a time must still
// see readable names on every page.
func TestFlattenHistoryToolNameResolvedAcrossPageBoundary(t *testing.T) {
	msgs := []message.Message{
		{ID: "1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "q1"}}},
		{ID: "2", Role: message.RoleAssistant, Parts: message.Parts{&message.ToolCall{CallID: "call_1", Name: "grep", Arguments: json.RawMessage(`{}`)}}},
		{ID: "3", Role: message.RoleTool, Parts: message.Parts{&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "match"}}}}},
	}
	// Page 2 starts AFTER the ToolCall message (offset 2), so only the
	// ToolResult message itself is on this page.
	page := flattenHistory(msgs, 2, 10)
	if page.Returned != 1 {
		t.Fatalf("page.Returned = %d, want 1", page.Returned)
	}
	if !strings.Contains(page.Text, "Tool result (grep, ok): match") {
		t.Errorf("flattened text = %q, want the tool name \"grep\" resolved from an earlier page", page.Text)
	}
}

// TestFlattenHistoryPagination drives offset/limit across a 10-message
// history (5 user/assistant pairs) and checks each page's own bookkeeping.
func TestFlattenHistoryPagination(t *testing.T) {
	var msgs []message.Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs,
			message.Message{ID: fmt.Sprintf("u%d", i), Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: fmt.Sprintf("question %d", i)}}},
			message.Message{ID: fmt.Sprintf("a%d", i), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: fmt.Sprintf("answer %d", i)}}},
		)
	}

	p1 := flattenHistory(msgs, 0, 4)
	if p1.Offset != 0 || p1.Returned != 4 || p1.NextOffset != 4 || !p1.HasMore {
		t.Errorf("page 1 = %+v, want Offset=0 Returned=4 NextOffset=4 HasMore=true", p1)
	}
	p2 := flattenHistory(msgs, p1.NextOffset, 4)
	if p2.Offset != 4 || p2.Returned != 4 || p2.NextOffset != 8 || !p2.HasMore {
		t.Errorf("page 2 = %+v, want Offset=4 Returned=4 NextOffset=8 HasMore=true", p2)
	}
	p3 := flattenHistory(msgs, p2.NextOffset, 4)
	if p3.Offset != 8 || p3.Returned != 2 || p3.NextOffset != 10 || p3.HasMore {
		t.Errorf("page 3 = %+v, want Offset=8 Returned=2 NextOffset=10 HasMore=false", p3)
	}
	if p1.Total != 10 || p2.Total != 10 || p3.Total != 10 {
		t.Errorf("Total = %d/%d/%d, want 10 on every page", p1.Total, p2.Total, p3.Total)
	}
}

// TestFlattenHistoryDefaultsAndClampsLimit proves a non-positive or
// oversized limit falls back to historyDefaultLimit, and a negative offset
// clamps to 0, rather than an unbounded or empty read.
func TestFlattenHistoryDefaultsAndClampsLimit(t *testing.T) {
	msgs := seedMessages()
	for _, limit := range []int{0, -1, historyMaxLimit + 1} {
		page := flattenHistory(msgs, 0, limit)
		if page.Returned != len(msgs) {
			t.Errorf("limit=%d: Returned = %d, want %d (all of a short history)", limit, page.Returned, len(msgs))
		}
	}
	page := flattenHistory(msgs, -5, 0)
	if page.Offset != 0 {
		t.Errorf("negative offset clamped to %d, want 0", page.Offset)
	}
}

// TestFlattenHistoryTruncatesLargeToolResult proves an oversized tool
// result is bounded rather than inlined in full, with a byte-count note.
func TestFlattenHistoryTruncatesLargeToolResult(t *testing.T) {
	big := strings.Repeat("x", historyContentTruncateBytes+500)
	msgs := []message.Message{
		{ID: "1", Role: message.RoleAssistant, Parts: message.Parts{&message.ToolCall{CallID: "c1", Name: "cat", Arguments: json.RawMessage(`{}`)}}},
		{ID: "2", Role: message.RoleTool, Parts: message.Parts{&message.ToolResult{CallID: "c1", Content: message.Parts{&message.Text{Text: big}}}}},
	}
	page := flattenHistory(msgs, 0, 0)
	if strings.Contains(page.Text, big) {
		t.Error("flattened text inlines the full oversized tool result, want it truncated")
	}
	if !strings.Contains(page.Text, "truncated") {
		t.Errorf("flattened text = %q, want a truncation note", page.Text)
	}
}

// TestHistoryResultTextLabelsPriorContextAndHints proves the wrapped tool
// result text clearly labels itself as prior/already-happened context and
// names the next_offset to continue with when more history remains.
func TestHistoryResultTextLabelsPriorContextAndHints(t *testing.T) {
	msgs := seedMessages()
	page := flattenHistory(msgs, 0, 2)
	text := historyResultText(page)
	if !strings.Contains(strings.ToLower(text), "prior") {
		t.Errorf("result text = %q, want it labeled as PRIOR context", text)
	}
	if !strings.Contains(text, "Showing messages 1-2 of 4") {
		t.Errorf("result text = %q, want a \"Showing messages 1-2 of 4\" line", text)
	}
	if !strings.Contains(text, fmt.Sprintf("offset=%d", page.NextOffset)) {
		t.Errorf("result text = %q, want a hint naming offset=%d", text, page.NextOffset)
	}
}

// TestHistoryResultTextEmptyHistory proves a session with no history at
// all gets an explicit "no history" message rather than an empty or
// confusingly-numbered "Showing messages 1-0 of 0" line.
func TestHistoryResultTextEmptyHistory(t *testing.T) {
	page := flattenHistory(nil, 0, 0)
	text := historyResultText(page)
	if !strings.Contains(text, "No prior conversation history") {
		t.Errorf("result text = %q, want an explicit no-history message", text)
	}
}

// TestHistoryResultTextClampedOffsetOmitsNonsensicalShowingLine proves an
// offset clamped to (or past) the end of history — flattenHistory's own
// clamp, e.g. an offset a prior call's next_offset hint no longer covers
// after the history shrank, or one a model simply got wrong — renders as
// an explicit "no messages" line, never a "Showing messages X-Y of N"
// line with X > Y (Returned == 0 makes page.Offset+1 > page.Offset+0).
func TestHistoryResultTextClampedOffsetOmitsNonsensicalShowingLine(t *testing.T) {
	msgs := seedMessages() // 4 messages total
	page := flattenHistory(msgs, 10, 5)
	if page.Returned != 0 || page.Total != 4 {
		t.Fatalf("page = %+v, want Returned=0 Total=4 (offset clamped past the end)", page)
	}
	text := historyResultText(page)
	if strings.Contains(text, "Showing messages") {
		t.Errorf("result text = %q, want no \"Showing messages\" line when Returned is 0", text)
	}
	if !strings.Contains(text, "no messages") {
		t.Errorf("result text = %q, want an explicit \"no messages\" line", text)
	}
	if !strings.Contains(text, "4") {
		t.Errorf("result text = %q, want the total (4) still mentioned somewhere", text)
	}
}

// --- HTTP wiring: POST /session/{id}/mcp ---

// TestHandleSessionMCPFullLifecycle drives initialize, tools/list, and
// tools/call against a REAL session (built cold, on disk, the same way
// TestMessagesUnparameterizedIsUnchanged's coldMessages helper does) over
// the actual HTTP route, proving handleSessionMCP's session lookup and
// mcpserver.Registry wiring end to end.
func TestHandleSessionMCPFullLifecycle(t *testing.T) {
	dir := t.TempDir()
	sess := coldMessages(t, dir, 2) // 4 messages: ask 0/reply 0, ask 1/reply 1
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	initResp, initData := h.do("POST", "/session/"+sess.ID+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-11-25"},
	})
	if initResp.StatusCode != 200 {
		t.Fatalf("initialize status = %d: %s", initResp.StatusCode, initData)
	}
	var initMsg struct {
		Result mcp.InitializeResult `json:"result"`
	}
	if err := json.Unmarshal(initData, &initMsg); err != nil {
		t.Fatalf("decoding initialize response: %v (%s)", err, initData)
	}
	if initMsg.Result.Capabilities.Tools == nil {
		t.Error("initialize response has no tools capability")
	}

	listResp, listData := h.do("POST", "/session/"+sess.ID+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "2", "method": "tools/list",
	})
	if listResp.StatusCode != 200 {
		t.Fatalf("tools/list status = %d: %s", listResp.StatusCode, listData)
	}
	var listMsg struct {
		Result mcp.ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal(listData, &listMsg); err != nil {
		t.Fatalf("decoding tools/list response: %v (%s)", err, listData)
	}
	if len(listMsg.Result.Tools) != 1 || listMsg.Result.Tools[0].Name != historyToolName {
		t.Fatalf("tools/list Tools = %+v, want exactly [%s]", listMsg.Result.Tools, historyToolName)
	}

	callResp, callData := h.do("POST", "/session/"+sess.ID+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "3", "method": "tools/call",
		"params": map[string]any{"name": historyToolName},
	})
	if callResp.StatusCode != 200 {
		t.Fatalf("tools/call status = %d: %s", callResp.StatusCode, callData)
	}
	var callMsg struct {
		Result mcp.CallToolResult `json:"result"`
		Error  *mcp.RPCError      `json:"error"`
	}
	if err := json.Unmarshal(callData, &callMsg); err != nil {
		t.Fatalf("decoding tools/call response: %v (%s)", err, callData)
	}
	if callMsg.Error != nil {
		t.Fatalf("tools/call returned an error: %+v", callMsg.Error)
	}
	if len(callMsg.Result.Content) != 1 {
		t.Fatalf("tools/call Content = %+v, want exactly one text item", callMsg.Result.Content)
	}
	got := callMsg.Result.Content[0].Text
	for _, want := range []string{"ask 0", "reply 0", "ask 1", "reply 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools/call text missing %q from the session's real history:\n%s", want, got)
		}
	}
}

// TestHandleSessionMCPUnknownSessionNotFound proves the route 404s for a
// session id that does not exist, mirroring every other {id}-keyed route.
func TestHandleSessionMCPUnknownSessionNotFound(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session/ses_0000000000000000/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "initialize",
	})
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404: %s", resp.StatusCode, data)
	}
}

// TestHandleSessionMCPRequiresAuth proves the route carries the same
// bearer-token gate as every other session route — unlike GET /health, it
// is never publicly reachable.
func TestHandleSessionMCPRequiresAuth(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("")

	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "1", "method": "initialize"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", h.ts.URL+"/session/"+id+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately no Authorization header — see s.auth/s.authorized in
	// server.go, the same gate every other {id}-keyed route sits behind.
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with no Authorization header", resp.StatusCode)
	}
}
