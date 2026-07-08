package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// sse formats one server-sent event.
func sse(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

var streamFixture = strings.Join([]string{
	sse("message_start", `{"type":"message_start","message":{"id":"msg_01","usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":25}}}`),
	sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
	sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`),
	sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig999"}}`),
	sse("content_block_stop", `{"type":"content_block_stop","index":0}`),
	sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
	sse("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Run"}}`),
	sse("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ning ls"}}`),
	sse("content_block_stop", `{"type":"content_block_stop","index":1}`),
	sse("ping", `{"type":"ping"}`),
	sse("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_77","name":"bash","input":{}}}`),
	sse("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}`),
	sse("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"and\":\"ls\"}"}}`),
	sse("content_block_stop", `{"type":"content_block_stop","index":2}`),
	sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`),
	sse("message_stop", `{"type":"message_stop"}`),
}, "")

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{APIKey: "test-key", BaseURL: srv.URL}
}

func collect(t *testing.T, s provider.Stream) []provider.Event {
	t.Helper()
	var events []provider.Event
	for {
		ev, err := s.Next()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
}

func TestStreamAssembly(t *testing.T) {
	var gotBody apiRequest
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" || r.Header.Get("Anthropic-Version") == "" {
			t.Errorf("missing auth headers: %v", r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamFixture) //nolint:errcheck
	})

	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "claude-fable-5"},
		System:    []string{"sys"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "ls please"}}}},
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events := collect(t, s)

	if !gotBody.Stream || gotBody.Model != "claude-fable-5" {
		t.Errorf("request body: stream=%v model=%q", gotBody.Stream, gotBody.Model)
	}

	var text, reasoning string
	var toolCalls []*message.ToolCall
	var done *provider.Event
	for i := range events {
		ev := events[i]
		switch ev.Type {
		case provider.EventTextDelta:
			text += ev.Text
		case provider.EventReasoningDelta:
			reasoning += ev.Text
		case provider.EventToolCall:
			toolCalls = append(toolCalls, ev.ToolCall)
		case provider.EventDone:
			done = &events[i]
		}
	}

	if text != "Running ls" {
		t.Errorf("text = %q", text)
	}
	if reasoning != "pondering" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if len(toolCalls) != 1 || toolCalls[0].CallID != "toolu_77" || toolCalls[0].Name != "bash" ||
		string(toolCalls[0].Arguments) != `{"command":"ls"}` {
		t.Errorf("tool calls = %+v", toolCalls)
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if done.StopReason != provider.StopToolUse {
		t.Errorf("stop reason = %s", done.StopReason)
	}
	if done.Usage.InputTokens != 100 || done.Usage.OutputTokens != 42 ||
		done.Usage.CacheWriteTokens != 50 || done.Usage.CacheReadTokens != 25 {
		t.Errorf("usage = %+v", done.Usage)
	}

	msg := done.Message
	if msg.ID != "msg_01" || msg.Role != message.RoleAssistant || len(msg.Parts) != 3 {
		t.Fatalf("message = %+v", msg)
	}
	r, ok := msg.Parts[0].(*message.Reasoning)
	if !ok || r.Text != "pondering" {
		t.Fatalf("part 0 = %+v", msg.Parts[0])
	}
	var data anthropicReasoningData
	if err := json.Unmarshal(r.ProviderData[Family], &data); err != nil || data.Signature != "sig999" {
		t.Errorf("reasoning provider data = %s (err %v)", r.ProviderData[Family], err)
	}
	if txt, ok := msg.Parts[1].(*message.Text); !ok || txt.Text != "Running ls" {
		t.Errorf("part 1 = %+v", msg.Parts[1])
	}
	if tc, ok := msg.Parts[2].(*message.ToolCall); !ok || tc.CallID != "toolu_77" {
		t.Errorf("part 2 = %+v", msg.Parts[2])
	}
}

func TestStreamHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`) //nolint:errcheck
	})
	_, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamInlineError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse("message_start", `{"type":"message_start","message":{"id":"msg_02","usage":{"input_tokens":1}}}`)) //nolint:errcheck
		io.WriteString(w, sse("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))           //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for {
		_, err := s.Next()
		if err != nil {
			if !strings.Contains(err.Error(), "Overloaded") {
				t.Fatalf("err = %v", err)
			}
			return
		}
	}
}

func TestStreamNoAPIKey(t *testing.T) {
	c := &Client{}
	_, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err = %v", err)
	}
}
