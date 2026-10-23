package openai

import (
	"testing"

	"github.com/majorcontext/harness/internal/provider"
)

func TestWebSocketInstructionsRefusalReportsFirstDifferingOffset(t *testing.T) {
	server := newWSLineageServer(t)
	for _, responseID := range []string{"resp_one", "resp_two"} {
		server.scripts <- wsLineageScript{beforeWait: completedLineageFrames(responseID, "two")}
	}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	first := lineageRequest("offset", userMessage("one"))
	first.System = []string{"base prompt"}
	streamLineageTurn(t, client, first)

	second := lineageRequest("offset", userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three"))
	second.System = []string{"base prompX"}
	got := lineageTerminalMetadata(t, streamLineageTurn(t, client, second))
	if got.ChainRefusalDetail != "instructions" {
		t.Fatalf("refusal detail = %q, want instructions", got.ChainRefusalDetail)
	}
	if got.ChainRefusalOffset == nil || *got.ChainRefusalOffset != 10 {
		t.Fatalf("refusal offset = %v, want 10", got.ChainRefusalOffset)
	}
	if got.ChainRefusalPreviousLen == nil || *got.ChainRefusalPreviousLen != 11 || got.ChainRefusalCurrentLen == nil || *got.ChainRefusalCurrentLen != 11 {
		t.Fatalf("refusal lengths = %v/%v, want 11/11", got.ChainRefusalPreviousLen, got.ChainRefusalCurrentLen)
	}
}

func TestWebSocketToolsRefusalNamesFirstDifferingTool(t *testing.T) {
	server := newWSLineageServer(t)
	for _, responseID := range []string{"resp_one", "resp_two", "resp_three"} {
		server.scripts <- wsLineageScript{beforeWait: completedLineageFrames(responseID, "two")}
	}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	tools := func(description string) []provider.ToolDef {
		return []provider.ToolDef{
			{Name: "read", Description: "read a file", InputSchema: []byte(`{"type":"object"}`)},
			{Name: "write", Description: description, InputSchema: []byte(`{"type":"object"}`)},
		}
	}

	first := lineageRequest("tools", userMessage("one"))
	first.Tools = tools("write a file")
	streamLineageTurn(t, client, first)

	second := lineageRequest("tools", userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three"))
	second.Tools, second.WebSearch = tools("write a file atomically"), true
	got := lineageTerminalMetadata(t, streamLineageTurn(t, client, second))
	if got.ChainRefusalDetail != "tools" || got.ChainRefusalTool != "write" || got.ChainRefusalToolPart != "description" {
		t.Fatalf("refusal = %q tool %q part %q, want tools, write, description", got.ChainRefusalDetail, got.ChainRefusalTool, got.ChainRefusalToolPart)
	}

	third := lineageRequest("tools", userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three"), assistantMessage("resp_two", "two"), userMessage("four"))
	third.Tools, third.WebSearch = append(tools("write a file atomically"), provider.ToolDef{Name: "grep", Description: "search", InputSchema: []byte(`{"type":"object"}`)}), true
	got = lineageTerminalMetadata(t, streamLineageTurn(t, client, third))
	if got.ChainRefusalTool != "grep" || got.ChainRefusalToolPart != "name" {
		t.Fatalf("with web search on, refusal tool %q part %q, want grep, name", got.ChainRefusalTool, got.ChainRefusalToolPart)
	}
}
