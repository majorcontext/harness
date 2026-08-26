package anthropic

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestStrippedContentBlockIsInert pins the exact shape a GATEWAY produces
// for a block type it does not model, observed live against Bifrost while
// server-side tool search was active: a content_block_start carrying an
// index and NO content_block field at all, followed by its
// content_block_stop. The Anthropic API itself emits the block; the gateway
// forwards an empty husk.
//
// Two properties matter, and both are what let a real run survive the
// stripping instead of failing the turn: the stream must not error, and the
// husk must not become a phantom part in the assembled message (an empty
// text part would reach history, and from there every later request).
func TestStrippedContentBlockIsInert(t *testing.T) {
	stream := strings.Join([]string{
		sse("message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1}}}`),
		sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I will search for a tool."}}`),
		sse("content_block_stop", `{"type":"content_block_stop","index":0}`),
		// The stripped block: no content_block key whatsoever.
		sse("content_block_start", `{"type":"content_block_start","index":1}`),
		sse("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sse("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"mcp__boxes__list_boxes","input":{}}}`),
		sse("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
		sse("content_block_stop", `{"type":"content_block_stop","index":2}`),
		sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`),
		sse("message_stop", `{"type":"message_stop"}`),
	}, "")

	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream) //nolint:errcheck
	})
	st, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "claude-opus-5"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("a stripped block failed the stream: %v", err)
	}
	defer st.Close()
	events := collect(t, st)
	var texts, tools int
	for _, ev := range events {
		switch {
		case ev.Text != "":
			texts++
		case ev.ToolCall != nil:
			tools++
			if ev.ToolCall.Name != "mcp__boxes__list_boxes" {
				t.Fatalf("tool call = %q, want the discovered deferred tool", ev.ToolCall.Name)
			}
		}
	}
	if tools != 1 {
		t.Fatalf("got %d tool calls, want the one after the stripped block", tools)
	}
	if texts == 0 {
		t.Fatal("the text before the stripped block was lost")
	}
}
