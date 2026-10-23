package openai

import (
	"testing"
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
