package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
)

// TestMaxTokensPartialJSONMarshalsThroughRealTranscoder pins the rebuttal of
// adversarial review finding 1 on the PR that introduced max_tokens
// auto-continue. The finding claimed a StopMaxTokens turn's trailing
// ToolCall -- carrying raw, truncated partial_json Arguments like `{"comm`,
// the shape Anthropic's own protocol leaves behind when max_tokens lands
// before a tool_use block's content_block_stop -- gets replayed into the
// continuation request and fails json.Marshal before it ever reaches the
// provider.
//
// That does not hold: message.Message.Normalize (Session.append's
// appendWithUsage, run on every append) already coerces the identical
// invalid-Arguments shape to nil in place -- the deliberate, incident-tested
// fix for a real production defect (see TestPersistTruncatedToolCallArguments,
// engine/tool_call_poison_test.go, NEP-5272-adjacent) -- before the
// continuation request is ever built. This test proves that end to end
// through the REAL production entry point rather than a hand-rolled check:
// a genuine `*anthropic.Client` (provider/anthropic), talking to an httptest
// server over real HTTP, drives an actual Session.Prompt call through a
// truncated-partial_json max_tokens stop and its auto-continuation. If the
// wire request failed to marshal, Client.Stream would return an error before
// the second HTTP request is ever sent, and this test would see Prompt fail
// and the server receive only one request -- neither happens.
//
// This is the test that pins the rebuttal: red-verify it by reintroducing
// any change that drops the partial call instead of clearing its Arguments
// (the case this test's own tool_use/tool_result assertions below would
// then fail, since the dropped shape carries no tool_use block at all) or
// that bypasses message.Message.Normalize on the append path (which would
// resurface the original marshal failure and fail this test's Stream/Prompt
// calls directly).
func TestMaxTokensPartialJSONMarshalsThroughRealTranscoder(t *testing.T) {
	var mu sync.Mutex
	var reqCount int
	var secondBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		n := reqCount
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// The real Anthropic wire shape for a tool_use block cut off
			// mid-emission by max_tokens: content_block_stop still fires
			// normally (see provider/anthropic/anthropic.go's doc comments
			// on this exact incident), but the accumulated partial_json is
			// truncated mid-token -- `{"comm`, never a complete
			// `{"command":"echo hi"}`.
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":10}}}\n\n")                                         //nolint:errcheck
			io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"bash\"}}\n\n") //nolint:errcheck
			io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"comm\"}}\n\n")       //nolint:errcheck
			io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")                                                                                  //nolint:errcheck
			io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":5}}\n\n")                             //nolint:errcheck
			io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")                                                                                                          //nolint:errcheck
			return
		}
		// The continuation request: decode it server-side before responding
		// -- a decode failure here means the client never sent a well-formed
		// body, which is exactly what a marshal failure client-side would
		// otherwise have prevented from arriving at all.
		if err := json.NewDecoder(r.Body).Decode(&secondBody); err != nil {
			t.Errorf("decode continuation request body: %v", err)
		}
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"usage\":{\"input_tokens\":10}}}\n\n")                //nolint:errcheck
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")   //nolint:errcheck
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n") //nolint:errcheck
		io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")                                                         //nolint:errcheck
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")      //nolint:errcheck
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")                                                                                 //nolint:errcheck
	}))
	defer srv.Close()

	c := &anthropic.Client{APIKey: "test-key", BaseURL: srv.URL}
	s := NewSession(Config{
		Providers:              provider.Registry{anthropic.Family: c},
		Model:                  message.ModelRef{Provider: anthropic.Family, Model: "m"},
		MaxTokensContinuations: 3,
	})

	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt = %v, want success (the continuation request must marshal and send)", err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q, want %q", final.Parts.Text(), "done")
	}

	mu.Lock()
	n := reqCount
	mu.Unlock()
	if n != 2 {
		t.Fatalf("server received %d requests, want 2 (initial max_tokens turn, then the auto-continuation)", n)
	}
	if secondBody == nil {
		t.Fatal("continuation request body was never decoded")
	}

	msgs, ok := secondBody["messages"].([]any)
	if !ok {
		t.Fatalf("continuation request has no messages array: %+v", secondBody)
	}

	// The truncated call's identity (id, name) survives the round trip
	// through the real transcoder, with its Arguments cleared to an empty
	// object -- the incident-tested behavior TestPersistTruncatedToolCallArguments
	// protects -- and a paired is_error tool_result immediately follows it,
	// so the wire request is fully valid, not merely non-crashing.
	var foundToolUse, foundToolResult bool
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		content, _ := mm["content"].([]any)
		for _, b := range content {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "tool_use":
				if block["id"] != "toolu_1" || block["name"] != "bash" {
					continue
				}
				foundToolUse = true
				input, ok := block["input"].(map[string]any)
				if !ok {
					t.Fatalf("tool_use block's input = %#v, want a present empty object (the truncated Arguments cleared)", block["input"])
				}
				if len(input) != 0 {
					t.Errorf("tool_use block's input = %#v, want empty (truncated JSON is unusable)", input)
				}
			case "tool_result":
				if block["tool_use_id"] != "toolu_1" {
					continue
				}
				foundToolResult = true
				if v, _ := block["is_error"].(bool); !v {
					t.Errorf("tool_result block for toolu_1 is_error = %v, want true", block["is_error"])
				}
			}
		}
	}
	if !foundToolUse {
		t.Fatalf("continuation request never replayed a tool_use block for toolu_1/bash: %+v", secondBody)
	}
	if !foundToolResult {
		t.Fatalf("continuation request never carried the paired is_error tool_result for toolu_1: %+v", secondBody)
	}
}
