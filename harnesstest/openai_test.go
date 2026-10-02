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
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/openai"
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
	if err := c.Prewarm(context.Background(), codexRequest()); err != nil {
		t.Fatal(err)
	}
	first := codexTurn(t, c, codexRequest(codexUser("run it")))
	results := message.Message{Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "out"}}},
	}}
	asst := *first.Message
	asst.Role = message.RoleAssistant
	codexTurn(t, c, codexRequest(codexUser("run it"), asst, results))

	want := []WireRequest{
		{Transport: "ws", Event: "dial", Conn: 1, ResponsesWebsockets: true},
		{Transport: "ws", Event: "prewarm", Conn: 1, Params: []string{"max_output_tokens"}},
		{Transport: "ws", Event: "request", Conn: 1, PreviousResponseID: "resp_warm_1", InputItems: 1, Params: []string{"max_output_tokens"}},
		{Transport: "ws", Event: "request", Conn: 1, PreviousResponseID: "resp_1", InputItems: 1, Params: []string{"max_output_tokens"}},
	}
	if got := s.WireRequests(); !reflect.DeepEqual(got, want) {
		t.Errorf("wire = %+v\nwant %+v", got, want)
	}
	reqs := s.Requests()
	last := reqs[1].Messages[len(reqs[1].Messages)-1].Parts[0]
	if last.Kind != "tool_result" || last.ToolName != "bash" || last.Text != "out" {
		t.Errorf("chained request tail = %+v, want a bash tool_result named from the issued call", last)
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
	want := []WireRequest{{Transport: "sse", Event: "request", ContentEncoding: "zstd", InputItems: 1, Params: []string{"max_output_tokens"}}}
	if got := s.WireRequests(); !reflect.DeepEqual(got, want) {
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
	got := s.WireRequests()
	if len(got) != 2 || got[0].Event != "refused" || got[1].Transport != "sse" {
		t.Errorf("wire = %+v, want a refused upgrade then an SSE request", got)
	}
}

func TestOpenAIRejectsChainToUnknownResponse(t *testing.T) {
	s := NewOpenAI(t, OpenAIOptions{})
	url := "ws" + strings.TrimPrefix(s.URL(), "http") + DefaultCodexPath
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

func TestOpenAIDropEndsWebSocketStreamWithoutTerminalEvent(t *testing.T) {
	s := NewOpenAI(t, OpenAIOptions{Replies: map[string]CodexReply{"d": {Drop: true}}}, Step{Name: "d", Reply: Reply{Text: "partial"}})
	st, err := codexClient(s, true).Stream(context.Background(), codexRequest(codexUser("hi")))
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
}

func codexUser(text string) message.Message {
	return message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: text}}}
}
