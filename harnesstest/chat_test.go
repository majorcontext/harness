package harnesstest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/provider/openaicompat"
)

func chatStream(t testing.TB, s *Server, req *provider.Request) ([]provider.Event, error) {
	t.Helper()
	c := &openaicompat.Client{Family: "bifrost", APIKey: "k", BaseURL: s.URL(), ExtraHeaders: map[string]string{"X-Tag": "e2e"}}
	if req.Model.Model == "" {
		req.Model = message.ModelRef{Provider: "bifrost", Model: "vendor/m"}
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 10
	}
	st, err := c.Stream(context.Background(), req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	var evs []provider.Event
	for {
		ev, err := st.Next()
		if err == io.EOF {
			return evs, nil
		}
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
}

func userMsg(text string) message.Message {
	return message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: text}}}
}

func TestChatReplyStreamsReasoningTextAndToolCalls(t *testing.T) {
	s := NewChat(t, Step{Reply: Reply{
		Reasoning: "think", Text: "hello", Usage: Usage{Input: 7, Output: 2},
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: "bash", Input: map[string]any{"command": "echo a"}},
			{ID: "call_2", Name: "ls"},
		},
	}})
	evs, err := chatStream(t, s, &provider.Request{Messages: []message.Message{userMsg("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	d := done(t, evs)
	if d.StopReason != provider.StopToolUse || d.Usage.InputTokens != 7 || d.Usage.OutputTokens != 2 {
		t.Errorf("stop %q usage %+v, want tool_use 7/2", d.StopReason, d.Usage)
	}
	var kinds []string
	for _, p := range d.Message.Parts {
		switch v := p.(type) {
		case *message.Reasoning:
			kinds = append(kinds, "reasoning:"+v.Text)
		case *message.Text:
			kinds = append(kinds, "text:"+v.Text)
		case *message.ToolCall:
			kinds = append(kinds, "call:"+v.CallID+":"+v.Name+":"+string(v.Arguments))
		}
	}
	want := []string{"reasoning:think", "text:hello", `call:call_1:bash:{"command":"echo a"}`, "call:call_2:ls:{}"}
	if !slices.Equal(kinds, want) {
		t.Errorf("parts = %q, want %q", kinds, want)
	}
}

func TestChatRequestDecodeAndMatchers(t *testing.T) {
	s := NewChat(t,
		Step{Name: "call", Match: LastUserText("run"), Reply: Reply{ToolCalls: []ToolCall{
			{ID: "call_1", Name: "bash"}, {ID: "call_2", Name: "ls"},
		}}},
		Step{Name: "after", Match: LastToolResult("ls"), Reply: Reply{Text: "ok"}},
	)
	first := &provider.Request{
		System: []string{"sys one", "sys two"}, Messages: []message.Message{userMsg("run it")},
		Tools:  []provider.ToolDef{{Name: "ls", InputSchema: json.RawMessage(`{}`)}, {Name: "bash", InputSchema: json.RawMessage(`{}`)}},
		Effort: message.EffortHigh, SessionKey: "ses_1",
	}
	evs, err := chatStream(t, s, first)
	if err != nil {
		t.Fatal(err)
	}
	asst := done(t, evs).Message
	asst.Role = message.RoleAssistant
	results := message.Message{Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "a"}}},
		&message.ToolResult{CallID: "call_2", Content: message.Parts{&message.Text{Text: "b"}}},
	}}
	if _, err := chatStream(t, s, &provider.Request{Messages: []message.Message{userMsg("run it"), *asst, results}}); err != nil {
		t.Fatal(err)
	}

	reqs := s.Requests()
	if reqs[0].System != "sys one\n\nsys two" || !slices.Equal(reqs[0].Tools, []string{"bash", "ls"}) {
		t.Errorf("system %q tools %q, want joined system and sorted tools", reqs[0].System, reqs[0].Tools)
	}
	if reqs[0].ReasoningEffort != "high" || reqs[0].User != "ses_1" || reqs[0].PromptCacheKey != "ses_1" || reqs[0].Header.Get("X-Tag") != "e2e" {
		t.Errorf("effort %q user %q cache key %q tag %q, want high ses_1 ses_1 e2e",
			reqs[0].ReasoningEffort, reqs[0].User, reqs[0].PromptCacheKey, reqs[0].Header.Get("X-Tag"))
	}
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	if len(last.Parts) != 2 || last.Role != "user" || last.Parts[0].ToolName != "bash" || last.Parts[1].ToolName != "ls" {
		t.Errorf("last message = %+v, want one user message of two named tool results", last)
	}
}

func TestChatErrorReplyIsRetryableStatus(t *testing.T) {
	s := NewChat(t, Step{Reply: Reply{HTTPStatus: 429, RetryAfter: "1", ErrorMessage: "slow down"}})
	_, err := chatStream(t, s, &provider.Request{Messages: []message.Message{userMsg("hi")}})
	if class, ok := provider.AsRetryable(err); !ok || class != provider.RetryableRateLimited {
		t.Fatalf("err = %v, want a rate-limited retryable error", err)
	}
}

func TestChatMaxTokensStopReason(t *testing.T) {
	s := NewChat(t, Step{Reply: Reply{Text: "cut", StopReason: "max_tokens"}})
	evs, err := chatStream(t, s, &provider.Request{Messages: []message.Message{userMsg("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := done(t, evs).StopReason; got != provider.StopMaxTokens {
		t.Errorf("stop = %q, want max_tokens", got)
	}
}

func TestChatLastUserTextSkipsEngineContextMessage(t *testing.T) {
	s := NewChat(t, Step{Match: LastUserText("real prompt"), Reply: Reply{Text: "ok"}})
	ctx := message.Message{Role: message.RoleUser, Parts: message.Parts{&message.EngineContext{Text: "ambient"}}}
	if _, err := chatStream(t, s, &provider.Request{Messages: []message.Message{userMsg("real prompt"), ctx}}); err != nil {
		t.Fatal(err)
	}
}

func TestChatRejectsRequestsARealGatewayRejects(t *testing.T) {
	const opts = `"stream":true,"stream_options":{"include_usage":true}`
	asst := `{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"bash","arguments":"{}"}}]}`
	tests := []struct {
		name, path, body, want string
	}{
		{"wrong path", "/v1/messages", `{"messages":[],` + opts + `}`, `request path "/v1/messages"`},
		{"not streaming", "/chat/completions", `{"messages":[],"stream_options":{"include_usage":true}}`, "stream is not true"},
		{"no include_usage", "/chat/completions", `{"messages":[],"stream":true}`, "include_usage is not true"},
		{"unknown role", "/chat/completions", `{"messages":[{"role":"critic","content":"x"}],` + opts + `}`, `unknown message role "critic"`},
		{"malformed arguments", "/chat/completions", `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"bash","arguments":"{"}}]}],` + opts + `}`, "malformed arguments"},
		{"orphan tool message", "/chat/completions", `{"messages":[` + asst + `,{"role":"tool","tool_call_id":"c2","content":"x"}],` + opts + `}`, `"c2" has no matching`},
		{"duplicate tool message", "/chat/completions", `{"messages":[` + asst + `,{"role":"tool","tool_call_id":"c1","content":"x"},{"role":"tool","tool_call_id":"c1","content":"x"}],` + opts + `}`, `"c1" has no matching`},
		{"message between call and result", "/chat/completions", `{"messages":[` + asst + `,{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"c1","content":"x"}],` + opts + `}`, "user message follows assistant tool calls"},
		{"missing tool message", "/chat/completions", `{"messages":[` + asst + `],` + opts + `}`, "have no tool message"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{TB: t}
			s := NewChat(rec, Step{Repeat: true, Reply: Reply{Text: "x"}})
			resp, err := http.Post(s.URL()+tc.path, "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode < 400 {
				t.Errorf("status = %d, want an error", resp.StatusCode)
			}
			rec.runCleanups()
			if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], tc.want) {
				t.Errorf("errors = %q, want one containing %q", rec.errors, tc.want)
			}
		})
	}
}

func TestChatBlockUntilRelease(t *testing.T) {
	tests := []struct {
		name      string
		reply     Reply
		wantDelta string
	}{
		{"text", Reply{Text: "slow text", Block: true}, `"content":"slow text"`},
		{"tool call only", Reply{Block: true, ToolCalls: []ToolCall{{ID: "call_1", Name: "bash"}}}, `"id":"call_1"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &Server{closing: make(chan struct{}), releases: map[string]chan struct{}{}, blocked: map[string]chan struct{}{}, canceled: map[string]chan struct{}{}}
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					s.chatStream(w, r, 1, "slow", tc.reply)
				}()
				synctest.Wait()
				select {
				case <-s.blockedCh("slow"):
				default:
					t.Fatal("handler is not waiting on Release")
				}
				if body := w.Body.String(); !strings.Contains(body, tc.wantDelta) || strings.Contains(body, "[DONE]") {
					t.Fatalf("body before Release = %q, want the first delta and no [DONE]", body)
				}
				s.Release("slow")
				synctest.Wait()
				select {
				case <-finished:
				default:
					t.Fatal("handler still waiting after Release")
				}
				if !strings.Contains(w.Body.String(), "[DONE]") {
					t.Errorf("body after Release = %q, want [DONE]", w.Body.String())
				}
			})
		})
	}
}

func TestChatErrorCodeClassifiesContextOverflow(t *testing.T) {
	s := NewChat(t, Step{Reply: Reply{HTTPStatus: 400, ErrorMessage: ContextOverflowMessage, ErrorCode: "context_length_exceeded"}})
	_, err := chatStream(t, s, &provider.Request{Messages: []message.Message{userMsg("hi")}})
	if !provider.IsContextOverflow(err) {
		t.Fatalf("IsContextOverflow(%v) = false, want true", err)
	}
}
