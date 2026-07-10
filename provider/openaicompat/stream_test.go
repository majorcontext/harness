package openaicompat

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

// sseData formats one "data: ..." SSE line (this wire has no "event:" line).
func sseData(data string) string {
	return "data: " + data + "\n\n"
}

const sseDone = "data: [DONE]\n\n"

func testClient(t *testing.T, family string, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{Family: family, APIKey: "test-key", BaseURL: srv.URL}
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

var streamFixture = strings.Join([]string{
	sseData(`{"id":"chatcmpl_1","model":"some/model","choices":[{"index":0,"delta":{"role":"assistant"}}]}`),
	sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"reasoning_content":"pondering"}}]}`),
	sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"Run"}}]}`),
	sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"ning ls"}}]}`),
	sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_77","function":{"name":"bash","arguments":""}}]}}]}`),
	sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"comm"}}]}}]}`),
	sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}}]}}]}`),
	sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	sseData(`{"id":"chatcmpl_1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":42}}`),
	sseDone,
}, "")

func TestStreamAssembly(t *testing.T) {
	var gotBody apiRequest
	var gotAuth, gotReferer, gotTitle string
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamFixture) //nolint:errcheck
	})
	c.ExtraHeaders = map[string]string{"HTTP-Referer": "https://harness.example", "X-Title": "harness"}

	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "some/model"},
		System:    []string{"sys"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "ls please"}}}},
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events := collect(t, s)

	if gotAuth != "Bearer test-key" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotReferer != "https://harness.example" || gotTitle != "harness" {
		t.Errorf("extra headers = %q %q", gotReferer, gotTitle)
	}
	if !gotBody.Stream || gotBody.Model != "some/model" {
		t.Errorf("request body: stream=%v model=%q", gotBody.Stream, gotBody.Model)
	}
	if gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
		t.Errorf("stream_options = %+v", gotBody.StreamOptions)
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
	if done.Usage.InputTokens != 100 || done.Usage.OutputTokens != 42 {
		t.Errorf("usage = %+v", done.Usage)
	}

	msg := done.Message
	if msg.ID != "chatcmpl_1" || msg.Role != message.RoleAssistant || len(msg.Parts) != 3 {
		t.Fatalf("message = %+v", msg)
	}
	if msg.Model.Provider != "openrouter" || msg.Model.Model != "some/model" {
		t.Errorf("message model = %+v", msg.Model)
	}
	rp, ok := msg.Parts[0].(*message.Reasoning)
	if !ok || rp.Text != "pondering" {
		t.Fatalf("part 0 = %+v", msg.Parts[0])
	}
	if len(rp.ProviderData) != 0 {
		t.Errorf("reasoning provider data = %+v, want none", rp.ProviderData)
	}
	if txt, ok := msg.Parts[1].(*message.Text); !ok || txt.Text != "Running ls" {
		t.Errorf("part 1 = %+v", msg.Parts[1])
	}
	if tc, ok := msg.Parts[2].(*message.ToolCall); !ok || tc.CallID != "call_77" ||
		string(tc.Arguments) != `{"command":"ls"}` {
		t.Errorf("part 2 = %+v", msg.Parts[2])
	}
}

func TestStreamParallelToolCallsInterleaved(t *testing.T) {
	fixture := strings.Join([]string{
		sseData(`{"id":"chatcmpl_2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"bash","arguments":"{\"a"}}]}}]}`),
		sseData(`{"id":"chatcmpl_2","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"read","arguments":"{\"p"}}]}}]}`),
		sseData(`{"id":"chatcmpl_2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\":1}"}}]}}]}`),
		sseData(`{"id":"chatcmpl_2","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"ath\":2}"}}]}}]}`),
		sseData(`{"id":"chatcmpl_2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		sseDone,
	}, "")
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, fixture) //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events := collect(t, s)

	var toolCalls []*message.ToolCall
	for _, ev := range events {
		if ev.Type == provider.EventToolCall {
			toolCalls = append(toolCalls, ev.ToolCall)
		}
	}
	if len(toolCalls) != 2 {
		t.Fatalf("tool calls = %d", len(toolCalls))
	}
	if toolCalls[0].CallID != "call_a" || string(toolCalls[0].Arguments) != `{"a":1}` {
		t.Errorf("tool call 0 = %+v", toolCalls[0])
	}
	if toolCalls[1].CallID != "call_b" || string(toolCalls[1].Arguments) != `{"path":2}` {
		t.Errorf("tool call 1 = %+v", toolCalls[1])
	}
}

func TestStreamFinishReasonMapping(t *testing.T) {
	cases := []struct {
		reason string
		want   provider.StopReason
	}{
		{"stop", provider.StopEndTurn},
		{"tool_calls", provider.StopToolUse},
		{"length", provider.StopMaxTokens},
		{"content_filter", provider.StopRefusal},
		{"mystery", provider.StopOther},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			fixture := strings.Join([]string{
				sseData(`{"id":"chatcmpl_3","choices":[{"index":0,"delta":{"content":"hi"}}]}`),
				sseData(`{"id":"chatcmpl_3","choices":[{"index":0,"delta":{},"finish_reason":"` + tc.reason + `"}]}`),
				sseDone,
			}, "")
			c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, fixture) //nolint:errcheck
			})
			s, err := c.Stream(context.Background(), &provider.Request{
				Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
				Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
				MaxTokens: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			events := collect(t, s)
			done := events[len(events)-1]
			if done.Type != provider.EventDone || done.StopReason != tc.want {
				t.Errorf("done = %+v, want stop reason %s", done, tc.want)
			}
		})
	}
}

func TestStreamMissingFinishReasonUsesDone(t *testing.T) {
	// A server that omits finish_reason entirely: tool calls are still
	// emitted, at [DONE].
	fixture := strings.Join([]string{
		sseData(`{"id":"chatcmpl_4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}]}}]}`),
		sseDone,
	}, "")
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, fixture) //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events := collect(t, s)
	var sawToolCall bool
	for _, ev := range events {
		if ev.Type == provider.EventToolCall {
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Error("expected a tool call event by [DONE]")
	}
	done := events[len(events)-1]
	if done.Type != provider.EventDone {
		t.Fatalf("last event = %+v", done)
	}
}

func TestStreamUsageTerminalChunk(t *testing.T) {
	fixture := strings.Join([]string{
		sseData(`{"id":"chatcmpl_5","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`),
		sseData(`{"id":"chatcmpl_5","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22}}`),
		sseDone,
	}, "")
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, fixture) //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events := collect(t, s)
	done := events[len(events)-1]
	if done.Usage.InputTokens != 11 || done.Usage.OutputTokens != 22 {
		t.Errorf("usage = %+v", done.Usage)
	}
}

func TestStreamHTTPError(t *testing.T) {
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`) //nolint:errcheck
	})
	_, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "Invalid API key") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamNoAPIKey(t *testing.T) {
	c := &Client{Family: "openrouter"}
	_, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamNoFamily(t *testing.T) {
	c := &Client{APIKey: "test-key"}
	_, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "Family") {
		t.Fatalf("err = %v", err)
	}
}

func TestName(t *testing.T) {
	c := &Client{Family: "ollama"}
	if c.Name() != "ollama" {
		t.Errorf("Name() = %q", c.Name())
	}
}

// TestStreamMidStreamErrorClassifiedRetryable pins the mid-stream error
// path: an OpenAI-compat `{"error":{...}}` chunk (how OpenRouter reports an
// overload that begins after headers were already sent) must surface as an
// error — classified retryable when the code says so — never as a silent
// end-of-turn. Before the fix, stream.handle unmarshalled the chunk, found
// no choices, and returned nil: the turn just ended, indistinguishable
// from success, and the goal loop's backoff never engaged.
func TestStreamMidStreamErrorClassifiedRetryable(t *testing.T) {
	c := testClient(t, "openrouter", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.Join([]string{ //nolint:errcheck
			sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"partial"}}]}`),
			sseData(`{"error":{"message":"upstream overloaded, please retry","code":529}}`),
		}, ""))
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: "openrouter", Model: "some/model"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var streamErr error
	for {
		_, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			streamErr = err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("mid-stream error chunk was silently swallowed: stream ended with no error")
	}
	class, ok := provider.AsRetryable(streamErr)
	if !ok {
		t.Fatalf("mid-stream 529 not classified retryable: %v", streamErr)
	}
	if class != provider.RetryableOverloaded {
		t.Errorf("class = %q, want %q", class, provider.RetryableOverloaded)
	}
	if !strings.Contains(streamErr.Error(), "upstream overloaded") {
		t.Errorf("original message lost: %v", streamErr)
	}
}
