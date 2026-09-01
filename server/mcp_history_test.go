package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/provider"
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

// TestHandleSessionMCPToolsListOmitsProcessToolWhenNotConfigured proves a
// session with no Config.Processes (the ordinary newHarness session, no
// process manager wired at all) advertises get_conversation_history and
// `task` but NEVER `process` — the same "process tool absent when
// unconfigured" rule the native loop already follows (engine's
// TestProcessToolAbsentWhenNoProcessesConfigured) applies identically to
// this MCP surface. `task` is present here (unlike `model`, gated purely on
// Config.ModelTool) because handleCreate's own h.createSession call path
// always runs SessionManager.AdoptRoot on a freshly created session (see
// server/handlers.go), which installs the native `task` tool unconditionally
// — see adoptRootLocked's own doc comment — independent of whatever
// Config.SessionManager the session was originally constructed with.
func TestHandleSessionMCPToolsListOmitsProcessToolWhenNotConfigured(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("")

	_, listData := h.do("POST", "/session/"+id+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "tools/list",
	})
	var listMsg struct {
		Result mcp.ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal(listData, &listMsg); err != nil {
		t.Fatalf("decoding tools/list response: %v (%s)", err, listData)
	}
	var names []string
	for _, tl := range listMsg.Result.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	want := []string{historyToolName, "task"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools/list Tools = %v, want exactly %v (no process, no model)", names, want)
	}
}

// TestHandleSessionMCPToolsListIncludesProcessToolWithAnnotations proves a
// session WITH Config.Processes configured advertises the native
// `process` tool alongside get_conversation_history (and `task`, always
// present on a handleCreate-adopted session — see
// TestHandleSessionMCPToolsListOmitsProcessToolWhenNotConfigured's own doc
// comment), each carrying the annotation this file's package doc promises:
// readOnlyHint on history, destructiveHint on process — plus process's own
// Description/InputSchema passed through from the engine's real tool
// definition (ToolDef), not a hand-duplicated copy.
func TestHandleSessionMCPToolsListIncludesProcessToolWithAnnotations(t *testing.T) {
	h, _ := newProcessHarness(t, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})
	id := h.createSession("")

	_, listData := h.do("POST", "/session/"+id+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "tools/list",
	})
	var listMsg struct {
		Result mcp.ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal(listData, &listMsg); err != nil {
		t.Fatalf("decoding tools/list response: %v (%s)", err, listData)
	}
	// Per-name presence/absence, not a raw tool count (which drifts the
	// moment any always-on tool — like `task`, unconditionally installed
	// by handleCreate's own AdoptRoot call; see
	// TestHandleSessionMCPToolsListOmitsProcessToolWhenNotConfigured's own
	// doc comment — is added elsewhere): history and process are the
	// tools THIS test cares about, task is expected-but-incidental here,
	// and model must be absent (Config.ModelTool is not set by
	// newProcessHarness).
	var hist, proc *mcp.Tool
	for i := range listMsg.Result.Tools {
		switch listMsg.Result.Tools[i].Name {
		case historyToolName:
			hist = &listMsg.Result.Tools[i]
		case "process":
			proc = &listMsg.Result.Tools[i]
		case "model":
			t.Fatalf("tools/list Tools = %+v, want no model (Config.ModelTool not set)", listMsg.Result.Tools)
		}
	}
	if hist == nil {
		t.Fatal("tools/list missing get_conversation_history")
	}
	// json.Marshal compacts an embedded json.RawMessage (no insignificant
	// whitespace survives the round trip through writeResult), so the
	// wire form is "readOnlyHint":true, not "readOnlyHint": true.
	if !strings.Contains(string(hist.Annotations), `"readOnlyHint":true`) {
		t.Errorf("get_conversation_history Annotations = %s, want readOnlyHint true", hist.Annotations)
	}
	if proc == nil {
		t.Fatal("tools/list missing process")
	}
	if !strings.Contains(string(proc.Annotations), `"destructiveHint":true`) {
		t.Errorf("process Annotations = %s, want destructiveHint true", proc.Annotations)
	}
	if !strings.Contains(proc.Description, "long-lived") {
		t.Errorf("process Description = %q, want it to describe managing long-lived box processes", proc.Description)
	}
	if len(proc.InputSchema) == 0 {
		t.Error("process InputSchema is empty, want the engine's own process-tool schema passed through")
	}
}

// TestHandleSessionMCPProcessToolCallStartsRealProcess proves tools/call
// for the process tool routes all the way through
// engine.Session.RunTool -- a "start" call over MCP leaves the process
// RUNNING in the same Manager a native-loop call would have used, not
// merely returning a plausible-looking response.
func TestHandleSessionMCPProcessToolCallStartsRealProcess(t *testing.T) {
	h, mgr := newProcessHarness(t, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", `echo "Ready in 5ms"; sleep 100`}, ReadyRegex: "Ready in .*ms"},
	})
	id := h.createSession("")

	_, callData := h.do("POST", "/session/"+id+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "tools/call",
		"params": map[string]any{"name": "process", "arguments": map[string]any{"action": "start", "name": "dev"}},
	})
	var callMsg struct {
		Result mcp.CallToolResult `json:"result"`
		Error  *mcp.RPCError      `json:"error"`
	}
	if err := json.Unmarshal(callData, &callMsg); err != nil {
		t.Fatalf("decoding tools/call response: %v (%s)", err, callData)
	}
	if callMsg.Error != nil {
		t.Fatalf("tools/call returned a protocol error: %+v", callMsg.Error)
	}
	if callMsg.Result.IsError {
		t.Fatalf("tools/call result IsError = true: %+v", callMsg.Result.Content)
	}

	st, err := mgr.Status("dev")
	if err != nil {
		t.Fatalf("mgr.Status: %v", err)
	}
	if st.State != process.StateReady {
		t.Fatalf("mgr.Status(dev) = %+v, want ready — tools/call must have actually started the process via RunTool", st)
	}
}

// TestHandleSessionMCPProcessToolCallFailureIsToolError proves a process
// tool call that fails at the ACTION level (an unknown action here) comes
// back as a successful JSON-RPC response carrying CallToolResult.IsError —
// the same TOOL-level-vs-protocol-level distinction
// TestRegistryToolsCallHandlerErrorBecomesIsErrorResult already locks in
// for mcpserver generically — never a protocol-level RPCError, since
// "process" IS a known, registered tool.
func TestHandleSessionMCPProcessToolCallFailureIsToolError(t *testing.T) {
	h, _ := newProcessHarness(t, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})
	id := h.createSession("")

	_, callData := h.do("POST", "/session/"+id+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "tools/call",
		"params": map[string]any{"name": "process", "arguments": map[string]any{"action": "not_a_real_action", "name": "dev"}},
	})
	var callMsg struct {
		Result mcp.CallToolResult `json:"result"`
		Error  *mcp.RPCError      `json:"error"`
	}
	if err := json.Unmarshal(callData, &callMsg); err != nil {
		t.Fatalf("decoding tools/call response: %v (%s)", err, callData)
	}
	if callMsg.Error != nil {
		t.Fatalf("tools/call returned a protocol-level error for a tool-level failure: %+v", callMsg.Error)
	}
	if !callMsg.Result.IsError {
		t.Fatalf("tools/call Result.IsError = false, want true for an unknown action")
	}
	if len(callMsg.Result.Content) != 1 || !strings.Contains(callMsg.Result.Content[0].Text, "not_a_real_action") {
		t.Errorf("tools/call Content = %+v, want it to name the unknown action", callMsg.Result.Content)
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

// --- task/model tool exposure ---

// newTaskModelHarness builds a harness whose sessions have BOTH the native
// `task` tool (Config.SessionManager wired to the server's own srv.sessMgr
// — the same wiring production's cmd/harness mkCfg uses, and the pattern
// journal_spawn_sync_test.go's
// TestChildJournaledAfterParentIdleEvictedAndReloaded already establishes
// for this package's tests) and the `model` tool (Config.ModelTool true) —
// the two tools this file's task+model MCP exposure tests need advertised
// together. The default newHarness/newServer helper (server_test.go)
// deliberately leaves both off (see that helper's own mkCfg), so a test
// that needs either builds its own Options here rather than mutating the
// shared default.
func newTaskModelHarness(t *testing.T, reg provider.Registry, defaultModel message.ModelRef) *harness {
	t.Helper()
	const token = "secret-run-token"
	dir := t.TempDir()
	var srv *Server
	opts := Options{
		SessionDir: dir,
		RunToken:   token,
		Version:    "9.9.9",
		NewSession: func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
			if m.IsZero() {
				m = defaultModel
			}
			return engine.NewSession(engine.Config{
				Providers:      reg,
				Model:          m,
				WorkDir:        workDir,
				ParentSession:  parentSession,
				SessionDir:     dir,
				OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
				SessionManager: srv.sessMgr,
				ModelTool:      true,
			}), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(engine.Config{
				Providers:      reg,
				SessionDir:     dir,
				OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
				SessionManager: srv.sessMgr,
				ModelTool:      true,
			}, id)
		},
	}
	var err error
	srv, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

// TestHandleSessionMCPToolsListIncludesTaskAndModelToolsWithAnnotations
// proves a session with Config.SessionManager and Config.ModelTool both set
// advertises `task` and `model` alongside get_conversation_history, each
// carrying the annotation this file's package doc now promises: readOnlyHint
// false on `task` (it can mutate sessions the caller itself spawned —
// spawn/cancel/send — even though its status/log actions are read-only),
// readOnlyHint true on `model` — and, since that hint is only honest
// because this surface is list-only (see modelToolShimInputSchema), also
// proves the PUBLISHED schema itself advertises action enum ["list"] only,
// never "set" or "status". `task`'s Description/InputSchema are checked
// against the engine's own real ToolDef (never a hand-duplicated copy);
// `model`'s are checked against this file's own hand-written shim schema
// instead, since `model` is the one tool this surface deliberately does NOT
// pass the engine's full ToolDef through for.
func TestHandleSessionMCPToolsListIncludesTaskAndModelToolsWithAnnotations(t *testing.T) {
	rootProv := &scriptedProvider{name: "root"}
	childProv := &scriptedProvider{name: "child"}
	h := newTaskModelHarness(t, provider.Registry{
		rootProv.Name():  rootProv,
		childProv.Name(): childProv,
	}, message.ModelRef{Provider: rootProv.Name(), Model: "m1"})
	id := h.createSession("")

	_, listData := h.do("POST", "/session/"+id+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "tools/list",
	})
	var listMsg struct {
		Result mcp.ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal(listData, &listMsg); err != nil {
		t.Fatalf("decoding tools/list response: %v (%s)", err, listData)
	}

	// Per-name presence, not a raw tool count — see
	// TestHandleSessionMCPToolsListIncludesProcessToolWithAnnotations's
	// identical reasoning. `process` is correctly absent here
	// (Config.Processes unset by newTaskModelHarness), which the switch
	// below would catch as an unhandled case if it ever regressed.
	var hist, task, model *mcp.Tool
	for i := range listMsg.Result.Tools {
		switch listMsg.Result.Tools[i].Name {
		case historyToolName:
			hist = &listMsg.Result.Tools[i]
		case "task":
			task = &listMsg.Result.Tools[i]
		case "model":
			model = &listMsg.Result.Tools[i]
		case "process":
			t.Fatalf("tools/list Tools = %+v, want no process (Config.Processes not set)", listMsg.Result.Tools)
		}
	}
	if hist == nil {
		t.Fatal("tools/list missing get_conversation_history")
	}
	if task == nil {
		t.Fatal("tools/list missing task")
	}
	if !strings.Contains(string(task.Annotations), `"readOnlyHint":false`) {
		t.Errorf("task Annotations = %s, want readOnlyHint false", task.Annotations)
	}
	if len(task.InputSchema) == 0 || !strings.Contains(task.Description, "spawn") {
		t.Errorf("task Description/InputSchema = %q/%s, want the engine's own task-tool schema passed through", task.Description, task.InputSchema)
	}
	if model == nil {
		t.Fatal("tools/list missing model")
	}
	if !strings.Contains(string(model.Annotations), `"readOnlyHint":true`) {
		t.Errorf("model Annotations = %s, want readOnlyHint true", model.Annotations)
	}
	// The load-bearing assertion: the PUBLISHED schema's action enum is
	// list-only. json.Marshal compacts an embedded json.RawMessage (no
	// insignificant whitespace survives the round trip through
	// writeResult), so the wire form is exactly `"enum":["list"]`.
	if !strings.Contains(string(model.InputSchema), `"enum":["list"]`) {
		t.Errorf("model InputSchema = %s, want action enum [\"list\"] only", model.InputSchema)
	}
	if strings.Contains(string(model.InputSchema), `"set"`) || strings.Contains(string(model.InputSchema), `"status"`) {
		t.Errorf("model InputSchema = %s, want it to never mention set or status", model.InputSchema)
	}
}

// TestHandleSessionMCPToolsListOmitsModelToolWhenDisabled proves a session
// built with Config.ModelTool false (the ordinary newHarness session)
// advertises history and `task` but never `model` — unlike `task` (always
// installed on a handleCreate-adopted session; see
// TestHandleSessionMCPToolsListOmitsProcessToolWhenNotConfigured's own doc
// comment), `model` has no such adopt-time install and stays governed
// purely by the Config.ModelTool flag the session was constructed with —
// the same "absent when unconfigured" rule already proven for `process`.
func TestHandleSessionMCPToolsListOmitsModelToolWhenDisabled(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("")

	_, listData := h.do("POST", "/session/"+id+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "tools/list",
	})
	var listMsg struct {
		Result mcp.ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal(listData, &listMsg); err != nil {
		t.Fatalf("decoding tools/list response: %v (%s)", err, listData)
	}
	for _, tl := range listMsg.Result.Tools {
		if tl.Name == "model" {
			t.Fatalf("tools/list Tools = %+v, want no model (Config.ModelTool false)", listMsg.Result.Tools)
		}
	}
}

// TestHandleSessionMCPModelToolListCallReturnsConfiguredFamilies proves
// tools/call for the `model` tool's list action routes through
// engine.Session.RunTool and returns the real configured provider families
// — the data a delegated caller (e.g. a claude-code-lane agent) needs to
// pick a family for task's own spawn(model:...) override.
func TestHandleSessionMCPModelToolListCallReturnsConfiguredFamilies(t *testing.T) {
	rootProv := &scriptedProvider{name: "root"}
	childProv := &scriptedProvider{name: "sol"}
	h := newTaskModelHarness(t, provider.Registry{
		rootProv.Name():  rootProv,
		childProv.Name(): childProv,
	}, message.ModelRef{Provider: rootProv.Name(), Model: "m1"})
	id := h.createSession("")

	_, callData := h.do("POST", "/session/"+id+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "tools/call",
		"params": map[string]any{"name": "model", "arguments": map[string]any{"action": "list"}},
	})
	var callMsg struct {
		Result mcp.CallToolResult `json:"result"`
		Error  *mcp.RPCError      `json:"error"`
	}
	if err := json.Unmarshal(callData, &callMsg); err != nil {
		t.Fatalf("decoding tools/call(model list) response: %v (%s)", err, callData)
	}
	if callMsg.Error != nil {
		t.Fatalf("tools/call(model list) returned a protocol error: %+v", callMsg.Error)
	}
	if callMsg.Result.IsError {
		t.Fatalf("tools/call(model list) result IsError=true: %+v", callMsg.Result.Content)
	}
	got := callMsg.Result.Content[0].Text
	for _, want := range []string{`"root"`, `"sol"`} {
		if !strings.Contains(got, want) {
			t.Errorf("model list result = %s, want it to list configured family %s", got, want)
		}
	}
}

// TestHandleSessionMCPModelToolSetAndStatusRejectedOverShim is the
// regression guard for the hijack this file's `model` shim exists to
// close: without modelListOnlyMCPHandler's own action check, a delegated
// caller could send {"name":"model","arguments":{"action":"set", ...}}
// over this exact endpoint and re-point the PARENT session's own live
// model — the session actually driving this delegated turn — a real
// behavioral hijack (the next harness turn stops delegating to whichever
// lane issued the call), not merely an unwanted read. `status` is rejected
// too: it leaks this session's own current-model state, which `list`
// deliberately omits (see modelListResult, engine/model_tool.go). Both
// must come back as an ordinary CallToolResult.IsError tool failure — the
// same tool-level-vs-protocol-level distinction
// TestHandleSessionMCPProcessToolCallFailureIsToolError already locks in
// for `process` — never a protocol-level RPCError (model IS a known,
// registered tool) and never a 200 with the session's model actually
// changed.
//
// Red-verify: delete the `if in.Action != "list"` check in
// modelListOnlyMCPHandler (server/mcp_history.go) and this test fails —
// set's IsError assertion fails first (RunTool actually swaps the model
// and returns success), proving this test would have caught the hijack.
func TestHandleSessionMCPModelToolSetAndStatusRejectedOverShim(t *testing.T) {
	rootProv := &scriptedProvider{name: "root"}
	childProv := &scriptedProvider{name: "sol"}
	h := newTaskModelHarness(t, provider.Registry{
		rootProv.Name():  rootProv,
		childProv.Name(): childProv,
	}, message.ModelRef{Provider: rootProv.Name(), Model: "m1"})
	id := h.createSession("")

	callModel := func(args map[string]any) mcp.CallToolResult {
		t.Helper()
		_, callData := h.do("POST", "/session/"+id+"/mcp", map[string]any{
			"jsonrpc": "2.0", "id": "1", "method": "tools/call",
			"params": map[string]any{"name": "model", "arguments": args},
		})
		var callMsg struct {
			Result mcp.CallToolResult `json:"result"`
			Error  *mcp.RPCError      `json:"error"`
		}
		if err := json.Unmarshal(callData, &callMsg); err != nil {
			t.Fatalf("decoding tools/call(model) response: %v (%s)", err, callData)
		}
		if callMsg.Error != nil {
			t.Fatalf("tools/call(model) returned a protocol-level error for a tool-level rejection: %+v", callMsg.Error)
		}
		return callMsg.Result
	}

	setResult := callModel(map[string]any{"action": "set", "model": "sol/m1"})
	if !setResult.IsError {
		t.Fatalf("tools/call(model set) IsError = false, want true — set must never be reachable over this shim: %+v", setResult.Content)
	}

	statusResult := callModel(map[string]any{"action": "status"})
	if !statusResult.IsError {
		t.Fatalf("tools/call(model status) IsError = false, want true — status must never be reachable over this shim: %+v", statusResult.Content)
	}

	// The actual hijack check: the session's OWN model must be completely
	// unaffected by the rejected set call above — read it back via the
	// one action this shim DOES allow (list carries no current-model
	// field, so this instead re-derives the session's live model via a
	// direct engine-level check, the same session object the server
	// itself is holding for id).
	sess, ok := h.srv.sessMgr.Session(id)
	if !ok {
		t.Fatalf("session %s not tracked by sessMgr", id)
	}
	if got := sess.Model(); got != (message.ModelRef{Provider: rootProv.Name(), Model: "m1"}) {
		t.Fatalf("session model = %v after a rejected set, want unchanged root/m1 — the shim's set rejection must be enforced BEFORE dispatch", got)
	}
}

// TestHandleSessionMCPTaskToolCallUnknownActionIsToolError proves an
// unknown `task` action over this MCP surface comes back as
// CallToolResult.IsError (a tool-level failure), not a protocol-level
// RPCError — the same distinction
// TestHandleSessionMCPProcessToolCallFailureIsToolError already locks in
// for `process`.
func TestHandleSessionMCPTaskToolCallUnknownActionIsToolError(t *testing.T) {
	rootProv := &scriptedProvider{name: "root"}
	h := newTaskModelHarness(t, provider.Registry{rootProv.Name(): rootProv}, message.ModelRef{Provider: rootProv.Name(), Model: "m1"})
	id := h.createSession("")

	_, callData := h.do("POST", "/session/"+id+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "tools/call",
		"params": map[string]any{"name": "task", "arguments": map[string]any{"action": "not_a_real_action"}},
	})
	var callMsg struct {
		Result mcp.CallToolResult `json:"result"`
		Error  *mcp.RPCError      `json:"error"`
	}
	if err := json.Unmarshal(callData, &callMsg); err != nil {
		t.Fatalf("decoding tools/call(task) response: %v (%s)", err, callData)
	}
	if callMsg.Error != nil {
		t.Fatalf("tools/call(task) returned a protocol-level error for a tool-level failure: %+v", callMsg.Error)
	}
	if !callMsg.Result.IsError {
		t.Fatalf("tools/call(task) Result.IsError = false, want true for an unknown action")
	}
	if len(callMsg.Result.Content) != 1 || !strings.Contains(callMsg.Result.Content[0].Text, "not_a_real_action") {
		t.Errorf("tools/call(task) Content = %+v, want it to name the unknown action", callMsg.Result.Content)
	}
}

// TestHandleSessionMCPTaskToolSpawnIsNonBlockingAndStatusPullsResult is the
// end-to-end proof behind this file's task exposure: tools/call(task,
// spawn) over the MCP surface routes through engine.Session.RunTool into
// the REAL, already-non-blocking Session.Spawn (it launches the child's own
// turn in a goroutine and returns the child's session id at once — see
// SessionManager.Spawn's own doc comment) with a model override selecting a
// DIFFERENT configured family (child/m1, distinct from the root session's
// own root/m1) — and a later tools/call(task, status) pulls the child's
// result straight from its own settled node state
// (SessionManager.DescendantInfo), independent of the native push-delivery
// path this MCP surface deliberately routes around (see this file's package
// doc).
//
// childProv is a blockingProvider, deliberately never released until AFTER
// the spawn call and an immediate status check both complete: if
// runTaskSpawn (or RunTool's dispatch of it) ever became blocking — waiting
// on the child's own Prompt call before returning — this test would hang
// rather than merely race, the same "prove non-blocking by parking the
// dependency, not by guessing at scheduling order" technique
// server_test.go's other blockingProvider tests already use. The
// immediate-after-spawn status call is read BEFORE the child is released,
// so it deterministically observes the child still StatusRunning (set
// synchronously, under SessionManager's own lock, before Spawn ever
// returns) — never a race against how fast a real child would finish.
// synctest.Wait() then settles the child's turn (and any auto-resume
// notification it triggers on root, hence rootProv's own scripted "noted"
// turn) deterministically, with zero real wall-clock cost, mirroring
// journal_spawn_sync_test.go's identical use of Wait() for this exact
// purpose.
func TestHandleSessionMCPTaskToolSpawnIsNonBlockingAndStatusPullsResult(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		rootProv := &scriptedProvider{name: "root", turns: [][]provider.Event{
			asstTurn("noted"), // consumes the auto-resume notification turn.
		}}
		childProv := newBlockingProvider("child")
		t.Cleanup(childProv.releaseAll)
		reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}

		var srv *Server
		opts := Options{
			SessionDir: dir,
			RunToken:   "secret-run-token",
			Version:    "9.9.9",
			NewSession: func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
				return engine.NewSession(engine.Config{
					Providers:      reg,
					Model:          m,
					WorkDir:        workDir,
					ParentSession:  parentSession,
					SessionDir:     dir,
					OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
					SessionManager: srv.sessMgr,
					ModelTool:      true,
				}), nil
			},
			LoadSession: func(id string) (*engine.Session, error) {
				return engine.LoadSession(engine.Config{
					Providers:      reg,
					SessionDir:     dir,
					OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
					SessionManager: srv.sessMgr,
					ModelTool:      true,
				}, id)
			},
		}
		var err error
		srv, err = New(opts)
		if err != nil {
			t.Fatal(err)
		}

		rootID := createSessionDirect(t, srv, "root/m1")

		callMCP := func(body string) mcp.CallToolResult {
			t.Helper()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/session/"+rootID+"/mcp", strings.NewReader(body))
			req.SetPathValue("id", rootID)
			srv.handleSessionMCP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("tools/call status %d: %s", rec.Code, rec.Body)
			}
			var msg struct {
				Result mcp.CallToolResult `json:"result"`
				Error  *mcp.RPCError      `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
				t.Fatalf("decoding tools/call response: %v (%s)", err, rec.Body)
			}
			if msg.Error != nil {
				t.Fatalf("tools/call returned a protocol error: %+v", msg.Error)
			}
			return msg.Result
		}

		spawnResult := callMCP(`{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"task","arguments":{"action":"spawn","agent":"general-purpose","prompt":"find the answer","model":"child/m1"}}}`)
		if spawnResult.IsError {
			t.Fatalf("tools/call(spawn) IsError=true: %+v", spawnResult.Content)
		}
		var spawned struct {
			SessionID string `json:"session_id"`
		}
		if len(spawnResult.Content) != 1 {
			t.Fatalf("tools/call(spawn) Content = %+v, want exactly one text item", spawnResult.Content)
		}
		if err := json.Unmarshal([]byte(spawnResult.Content[0].Text), &spawned); err != nil {
			t.Fatalf("decoding spawn result: %v (%s)", err, spawnResult.Content[0].Text)
		}
		if spawned.SessionID == "" {
			t.Fatal("spawn result has no session_id")
		}

		statusCall := `{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"task","arguments":{"action":"status","session_id":"` + spawned.SessionID + `"}}}`

		// The child is still parked on blockingStream.Next (childProv not
		// yet released) — this proves the spawn call above did not wait
		// for it, and DescendantInfo's live status confirms the child is
		// tracked as running, not merely "unknown" or "idle".
		early := callMCP(statusCall)
		if early.IsError {
			t.Fatalf("tools/call(status) IsError=true: %+v", early.Content)
		}
		if !strings.Contains(early.Content[0].Text, `"status":"running"`) {
			t.Fatalf("status immediately after spawn = %s, want status running (child still parked on its provider)", early.Content[0].Text)
		}

		childProv.releaseAll()
		synctest.Wait()

		final := callMCP(statusCall)
		if final.IsError {
			t.Fatalf("tools/call(status) IsError=true: %+v", final.Content)
		}
		if !strings.Contains(final.Content[0].Text, `"status":"done"`) {
			t.Fatalf("status after settling = %s, want status done", final.Content[0].Text)
		}
		if !strings.Contains(final.Content[0].Text, "released") {
			t.Fatalf("status after settling = %s, want the child's own result (\"released\") pulled from its settled node state", final.Content[0].Text)
		}
	})
}
