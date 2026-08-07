package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
)

// TestLoadSessionRepairsEmptyToolResultContent is the red-first regression
// test for NEP-5272's B1 finding: LoadSession never called
// message.Message.Normalize, so a persisted ToolResult with empty Content
// (an older, unpatched write — or a plugin/adapter that bypassed the live
// Normalize path) reloaded unchanged and reproduced the exact wire wedge
// live production hit on resume: an empty tool_result content array that
// omitempty drops from the wire entirely.
//
// This test proves the full path: a raw session log carrying a ToolResult
// with "content":[] loads through LoadSession, and the resulting history
// transcodes, through the REAL Anthropic adapter (not a hand-rolled
// stand-in), to a wire request whose tool_result block carries a present,
// non-empty "content" array.
func TestLoadSessionRepairsEmptyToolResultContent(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_7777777777777777"

	// A hand-written log, exactly as an older, unpatched binary (or any
	// producer that bypasses Normalize) would have left it: the tool call
	// and its result both persisted, but the result's Content is an empty
	// array — the precise shape bash.go's captured-output path leaves for
	// a command with no stdout/stderr (see message.ToolResult.SafeContent's
	// doc comment, "Incident NEP-5272, root cause 2").
	data := `{"type":"session","id":"` + id + `","created_at":"2025-01-02T03:04:05Z"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"go"}]}}
{"type":"message","message":{"id":"msg_2","role":"assistant","parts":[{"type":"tool_call","call_id":"tc1","name":"bash","arguments":{}}]}}
{"type":"message","message":{"id":"msg_3","role":"tool","parts":[{"type":"tool_result","call_id":"tc1","content":[]}]}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir, Model: message.ModelRef{Provider: anthropic.Family, Model: "m"}}, id)
	if err != nil {
		t.Fatalf("LoadSession = %v, want success", err)
	}

	hist := s.History()
	if len(hist) != 3 {
		t.Fatalf("len(history) = %d, want 3", len(hist))
	}
	tr, ok := hist[2].Parts[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("history[2].Parts[0] = %T, want *message.ToolResult", hist[2].Parts[0])
	}
	if len(tr.Content) == 0 {
		t.Fatalf("reloaded ToolResult.Content is still empty, want Normalize to have repaired it on load")
	}

	// Now prove the repair actually closes the wire wedge: transcode
	// through the real Anthropic adapter and inspect the exact bytes it
	// would send.
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"usage\":{\"input_tokens\":1}}}\n\n")            //nolint:errcheck
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n") //nolint:errcheck
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")                                                                            //nolint:errcheck
	}))
	defer srv.Close()

	c := &anthropic.Client{APIKey: "test-key", BaseURL: srv.URL}
	stream, err := c.Stream(context.Background(), &provider.Request{
		Model:     s.Model(),
		Messages:  hist,
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatalf("Stream = %v, want success", err)
	}
	for {
		if _, err := stream.Next(); err != nil {
			break
		}
	}

	msgs, ok := gotBody["messages"].([]any)
	if !ok {
		t.Fatalf("request body has no messages array: %+v", gotBody)
	}
	var foundToolResult bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		content, _ := mm["content"].([]any)
		for _, b := range content {
			block := b.(map[string]any)
			if block["type"] != "tool_result" {
				continue
			}
			if block["tool_use_id"] != "tc1" {
				continue
			}
			foundToolResult = true
			raw, present := block["content"]
			if !present {
				t.Fatalf("wire tool_result block has no \"content\" key at all: %+v", block)
			}
			arr, ok := raw.([]any)
			if !ok || len(arr) == 0 {
				t.Fatalf("wire tool_result block's content = %#v, want a present, non-empty array", raw)
			}
		}
	}
	if !foundToolResult {
		t.Fatalf("wire request never carried a tool_result block for tc1: %+v", gotBody)
	}
}
