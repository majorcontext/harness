package openai

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

const reasoningItem = `{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"thinking about it"}],"encrypted_content":"ENC"}`

var streamFixture = strings.Join([]string{
	sse("response.created", `{"type":"response.created","response":{"id":"resp_1"}}`),
	sse("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"thinking about it"}`),
	sse("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":`+reasoningItem+`}`),
	sse("response.output_text.delta", `{"type":"response.output_text.delta","output_index":1,"delta":"Run"}`),
	sse("response.output_text.delta", `{"type":"response.output_text.delta","output_index":1,"delta":"ning ls"}`),
	sse("response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Running ls"}]}}`),
	sse("response.output_item.done", `{"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_77","name":"bash","arguments":"{\"command\":\"ls\"}"}}`),
	sse("response.completed", `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":100,"output_tokens":42,"input_tokens_details":{"cached_tokens":25}}}}`),
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
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header: %v", r.Header)
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamFixture) //nolint:errcheck
	})

	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "gpt-5"},
		System:    []string{"sys"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "ls please"}}}},
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events := collect(t, s)

	if !gotBody.Stream || gotBody.Model != "gpt-5" {
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
	if reasoning != "thinking about it" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if len(toolCalls) != 1 || toolCalls[0].CallID != "call_77" || toolCalls[0].Name != "bash" ||
		string(toolCalls[0].Arguments) != `{"command":"ls"}` {
		t.Errorf("tool calls = %+v", toolCalls)
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if done.StopReason != provider.StopToolUse {
		t.Errorf("stop reason = %s", done.StopReason)
	}
	if done.Usage.InputTokens != 100 || done.Usage.OutputTokens != 42 || done.Usage.CacheReadTokens != 25 {
		t.Errorf("usage = %+v", done.Usage)
	}

	msg := done.Message
	if msg.ID != "resp_1" || msg.Role != message.RoleAssistant || len(msg.Parts) != 3 {
		t.Fatalf("message = %+v", msg)
	}
	rp, ok := msg.Parts[0].(*message.Reasoning)
	if !ok || rp.Text != "thinking about it" {
		t.Fatalf("part 0 = %+v", msg.Parts[0])
	}
	// The entire raw reasoning item is stored verbatim under ProviderData.
	raw, ok := rp.ProviderData[Family]
	if !ok {
		t.Fatal("reasoning provider data missing openai key")
	}
	if !jsonEqual(t, raw, json.RawMessage(reasoningItem)) {
		t.Errorf("reasoning item not verbatim:\n got %s\nwant %s", raw, reasoningItem)
	}
	if txt, ok := msg.Parts[1].(*message.Text); !ok || txt.Text != "Running ls" {
		t.Errorf("part 1 = %+v", msg.Parts[1])
	}
	if tc, ok := msg.Parts[2].(*message.ToolCall); !ok || tc.CallID != "call_77" ||
		string(tc.Arguments) != `{"command":"ls"}` {
		t.Errorf("part 2 = %+v", msg.Parts[2])
	}
}

func TestStreamEndTurnNoTools(t *testing.T) {
	fixture := strings.Join([]string{
		sse("response.created", `{"type":"response.created","response":{"id":"resp_2"}}`),
		sse("response.output_text.delta", `{"type":"response.output_text.delta","output_index":0,"delta":"hello"}`),
		sse("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`),
		sse("response.completed", `{"type":"response.completed","response":{"id":"resp_2","usage":{"input_tokens":5,"output_tokens":3}}}`),
	}, "")
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, fixture) //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "gpt-5"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events := collect(t, s)
	done := events[len(events)-1]
	if done.Type != provider.EventDone || done.StopReason != provider.StopEndTurn {
		t.Errorf("done = %+v", done)
	}
}

func TestStreamIncomplete(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   provider.StopReason
	}{
		{"max_output_tokens", "max_output_tokens", provider.StopMaxTokens},
		{"content_filter", "content_filter", provider.StopRefusal},
		{"unknown reason", "mystery", provider.StopOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := strings.Join([]string{
				sse("response.created", `{"type":"response.created","response":{"id":"resp_inc"}}`),
				sse("response.output_text.delta", `{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`),
				sse("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_inc","type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}}`),
				sse("response.incomplete", `{"type":"response.incomplete","response":{"id":"resp_inc","incomplete_details":{"reason":"`+tc.reason+`"},"usage":{"input_tokens":7,"output_tokens":9,"input_tokens_details":{"cached_tokens":2}}}}`),
			}, "")
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, fixture) //nolint:errcheck
			})
			s, err := c.Stream(context.Background(), &provider.Request{
				Model:     message.ModelRef{Provider: Family, Model: "gpt-5"},
				Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
				MaxTokens: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			events := collect(t, s)
			done := events[len(events)-1]
			if done.Type != provider.EventDone {
				t.Fatalf("last event = %+v", done)
			}
			if done.StopReason != tc.want {
				t.Errorf("stop reason = %s, want %s", done.StopReason, tc.want)
			}
			if done.Usage.InputTokens != 7 || done.Usage.OutputTokens != 9 || done.Usage.CacheReadTokens != 2 {
				t.Errorf("usage = %+v", done.Usage)
			}
			msg := done.Message
			if msg == nil || msg.ID != "resp_inc" || len(msg.Parts) != 1 {
				t.Fatalf("message = %+v", msg)
			}
			if txt, ok := msg.Parts[0].(*message.Text); !ok || txt.Text != "partial" {
				t.Errorf("part 0 = %+v", msg.Parts[0])
			}
		})
	}
}

func TestStreamHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"Incorrect API key provided"}}`) //nolint:errcheck
	})
	_, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "Incorrect API key provided") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamInlineError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse("response.created", `{"type":"response.created","response":{"id":"resp_3"}}`))                                  //nolint:errcheck
		io.WriteString(w, sse("response.failed", `{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`)) //nolint:errcheck
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
			if !strings.Contains(err.Error(), "boom") {
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
