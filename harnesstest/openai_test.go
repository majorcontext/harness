package harnesstest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/openai"
)

var (
	codexInclude = []string{"reasoning.encrypted_content"}
	maxOutput    = []string{"max_output_tokens"}
)

func codexClient(s *OpenAI, websocket bool) *openai.Client {
	return &openai.Client{
		APIKey: "k", BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses",
		Family: openai.CodexFamily, UseWebSocketTransport: websocket,
	}
}

func codexRequest(msgs ...message.Message) *provider.Request {
	return &provider.Request{
		Model:      message.ModelRef{Provider: openai.CodexFamily, Model: "gpt-5"},
		System:     []string{"sys"},
		Tools:      []provider.ToolDef{{Name: "bash", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		SessionKey: "ses_1",
		MaxTokens:  100,
		Messages:   msgs,
	}
}

func codexTurn(t testing.TB, c *openai.Client, req *provider.Request) provider.Event {
	t.Helper()
	st, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	return done(t, drain(t, st))
}

func TestOpenAIPrewarmChainsFirstTurnAndResolvesChainedToolNames(t *testing.T) {
	s := NewOpenAI(t, OpenAIOptions{},
		Step{Name: "call", Match: LastUserText("run"), Reply: Reply{ToolCalls: []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "ls"}}}}},
		Step{Name: "after", Match: LastToolResult("bash"), Reply: Reply{Text: "ok"}},
	)
	c := codexClient(s, true)
	if err := c.Warm(context.Background(), codexRequest()); err != nil {
		t.Fatal(err)
	}
	first := codexTurn(t, c, codexRequest(codexUser("run it")))
	results := message.Message{Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "out"}}},
	}}
	asst := *first.Message
	asst.Role = message.RoleAssistant
	codexTurn(t, c, codexRequest(codexUser("run it"), asst, results))

	want := []WireEvent{
		{Transport: "ws", Event: "dial", Conn: 1, ResponsesWebsockets: true},
		{Transport: "ws", Event: "prewarm", Conn: 1, Params: maxOutput, Include: codexInclude, ReasoningSummary: "auto"},
		{Transport: "ws", Event: "request", Conn: 1, PreviousResponseID: "resp_warm_1", InputItems: 1, Params: maxOutput, Include: codexInclude, ReasoningSummary: "auto"},
		{Transport: "ws", Event: "request", Conn: 1, PreviousResponseID: "resp_1", InputItems: 1, Params: maxOutput, Include: codexInclude, ReasoningSummary: "auto"},
	}
	if got := s.WireEvents(); !reflect.DeepEqual(got, want) {
		t.Errorf("wire = %+v\nwant %+v", got, want)
	}
	if got := s.PrewarmInstructions(); !reflect.DeepEqual(got, []string{"sys"}) {
		t.Errorf("prewarm instructions = %q, want the system prompt of the request", got)
	}
	reqs := s.Requests()
	last := reqs[1].Messages[len(reqs[1].Messages)-1].Parts[0]
	if last.Kind != "tool_result" || last.ToolName != "bash" || last.Text != "out" {
		t.Errorf("chained request tail = %+v, want a bash tool_result named from the issued call", last)
	}
}

func TestOpenAIToolResultNameComesFromTheRequestsOwnHistory(t *testing.T) {
	s := NewOpenAI(t, OpenAIOptions{},
		Step{Name: "call", Match: LastUserText("run"), Reply: Reply{ToolCalls: []ToolCall{{ID: "call_1", Name: "bash"}}}},
		Step{Name: "other", Match: LastUserText("again"), Reply: Reply{Text: "ok"}},
	)
	c := codexClient(s, false)
	codexTurn(t, c, codexRequest(codexUser("run")))
	asst := message.Message{Role: message.RoleAssistant, Parts: message.Parts{
		&message.ToolCall{CallID: "call_1", Name: "grep", Arguments: json.RawMessage(`{}`)},
	}}
	results := message.Message{Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "out"}}},
	}}
	codexTurn(t, c, codexRequest(codexUser("again"), asst, results))
	reqs := s.Requests()
	last := reqs[1].Messages[len(reqs[1].Messages)-1].Parts[0]
	if last.Kind != "tool_result" || last.ToolName != "grep" {
		t.Errorf("tool result = %+v, want the name grep from the request's own function_call", last)
	}
}

func TestOpenAIReasoningItemSurfacesAndReplays(t *testing.T) {
	s := NewOpenAI(t, OpenAIOptions{Replies: map[string]CodexReply{"think": {Reasoning: []string{"plan", "check"}}}},
		Step{Name: "think", Match: LastUserText("go"), Reply: Reply{ToolCalls: []ToolCall{{ID: "call_1", Name: "bash"}}}},
		Step{Name: "after", Match: LastToolResult("bash"), Reply: Reply{Text: "ok"}},
	)
	c := codexClient(s, false)
	first := codexTurn(t, c, codexRequest(codexUser("go")))
	var got *message.Reasoning
	for _, p := range first.Message.Parts {
		if r, ok := p.(*message.Reasoning); ok {
			got = r
		}
	}
	if got == nil || got.Text != "plan\n\ncheck" {
		t.Fatalf("reasoning part = %+v, want the two summary parts joined", got)
	}
	asst := *first.Message
	asst.Role = message.RoleAssistant
	results := message.Message{Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "out"}}},
	}}
	codexTurn(t, c, codexRequest(codexUser("go"), asst, results))
	if w := s.WireEvents(); len(w) != 2 || w[0].ReasoningItems != 0 || w[1].ReasoningItems != 1 {
		t.Errorf("wire = %+v, want the second request to replay one reasoning item", w)
	}
}

func TestOpenAIRecordsSchemaKeywordsTheBackendRejects(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"email":{"type":"string","pattern":"^a","format":"email"},"n":{"type":"array","items":{"type":"string","minLength":1}}}}`)
	for _, tc := range []struct {
		name     string
		sanitize bool
		want     []string
	}{
		{"sanitize off", false, []string{"format", "minLength", "pattern"}},
		{"sanitize on", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewOpenAI(t, OpenAIOptions{}, Step{Reply: Reply{Text: "hi"}})
			c := codexClient(s, false)
			c.SanitizeToolSchemas = tc.sanitize
			req := codexRequest(codexUser("hi"))
			req.Tools = []provider.ToolDef{{Name: "send", InputSchema: schema}}
			codexTurn(t, c, req)
			if got := s.WireEvents()[0].RejectedSchemaKeywords; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rejected keywords = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOpenAISSEDecodesZstdAndReportsRateLimitHeaders(t *testing.T) {
	s := NewOpenAI(t, OpenAIOptions{Replies: map[string]CodexReply{"r": {RateLimits: &RateLimits{
		Plan: "pro", Primary: &RateWindow{UsedPercent: 12.5, WindowMinutes: 10080, ResetAt: 99},
	}}}}, Step{Name: "r", Reply: Reply{Text: "hi"}})
	ev := codexTurn(t, codexClient(s, false), codexRequest(codexUser("hi")))
	if got := ev.SubscriptionUsage; got == nil || got.Plan != "pro" || len(got.Windows) != 1 || got.Windows[0].Label != "Weekly" || got.Windows[0].UsedPercent != 12.5 {
		t.Errorf("subscription usage = %+v, want pro plan with a Weekly 12.5%% window", got)
	}
	want := []WireEvent{{Transport: "sse", Event: "request", ContentEncoding: "zstd", InputItems: 1, Params: maxOutput, Include: codexInclude, ReasoningSummary: "auto"}}
	if got := s.WireEvents(); !reflect.DeepEqual(got, want) {
		t.Errorf("wire = %+v, want %+v", got, want)
	}
}

func TestOpenAIRateLimitsFrameOverWebSocket(t *testing.T) {
	s := NewOpenAI(t, OpenAIOptions{Replies: map[string]CodexReply{"r": {RateLimits: &RateLimits{
		Plan: "plus", Secondary: &RateWindow{UsedPercent: 3, WindowMinutes: 300, ResetAt: 7},
	}}}}, Step{Name: "r", Reply: Reply{Text: "hi"}})
	ev := codexTurn(t, codexClient(s, true), codexRequest(codexUser("hi")))
	if got := ev.SubscriptionUsage; got == nil || got.Plan != "plus" || len(got.Windows) != 1 || got.Windows[0].Key != "secondary" || got.Windows[0].Label != "5-hour" {
		t.Errorf("subscription usage = %+v, want plus plan with a 5-hour secondary window", got)
	}
}

func TestOpenAIRefusedWebSocketFallsBackToSSE(t *testing.T) {
	s := NewOpenAI(t, OpenAIOptions{RefuseWebSocket: true}, Step{Reply: Reply{Text: "hi"}})
	codexTurn(t, codexClient(s, true), codexRequest(codexUser("hi")))
	got := s.WireEvents()
	if len(got) != 2 || got[0].Event != "refused" || got[1].Transport != "sse" {
		t.Errorf("wire = %+v, want a refused upgrade then an SSE request", got)
	}
}

func chainMissFrame(t *testing.T, s *OpenAI) []byte {
	t.Helper()
	url := "ws" + strings.TrimPrefix(s.URL(), "http") + codexPath
	hdr := http.Header{"Authorization": {"Bearer k"}}
	conn, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"m","input":[],"previous_response_id":"resp_gone"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOpenAIRejectsChainToUnknownResponse(t *testing.T) {
	data := chainMissFrame(t, NewOpenAI(t, OpenAIOptions{}))
	var fr struct {
		Type  string `json:"type"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &fr); err != nil || fr.Type != "error" || fr.Error.Code != "previous_response_not_found" {
		t.Errorf("frame = %s, want an error frame with code previous_response_not_found", data)
	}
}

func TestOpenAIUncodedChainMissNamesTheFieldWithoutACode(t *testing.T) {
	data := string(chainMissFrame(t, NewOpenAI(t, OpenAIOptions{UncodedChainMiss: true})))
	if !strings.Contains(data, "Invalid `previous_response_id`.") || strings.Contains(data, `"code"`) {
		t.Errorf("frame = %s, want the live message with no code", data)
	}
}

func TestOpenAIDropEndsStreamWithoutTerminalEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		ws   bool
	}{{"websocket", true}, {"sse", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewOpenAI(t, OpenAIOptions{Replies: map[string]CodexReply{"d": {Drop: true}}}, Step{Name: "d", Reply: Reply{Text: "partial"}})
			st, err := codexClient(s, tc.ws).Stream(context.Background(), codexRequest(codexUser("hi")))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			for {
				ev, err := st.Next()
				if err == io.EOF || ev.Type == provider.EventDone {
					t.Fatal("stream ended cleanly, want a truncation error")
				}
				if err != nil {
					if class, ok := provider.AsRetryable(err); !ok || class != provider.RetryableStreamTruncated {
						t.Fatalf("err = %v, want a stream-truncated retryable error", err)
					}
					return
				}
			}
		})
	}
}

func codexUser(text string) message.Message {
	return message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: text}}}
}
