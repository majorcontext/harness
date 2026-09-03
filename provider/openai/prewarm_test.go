package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/majorcontext/harness/provider"
)

var _ provider.StartupPrewarmer = (*Client)(nil)

func TestStartupPrewarmEnabledOnlyForCodexWebSocket(t *testing.T) {
	tests := []struct {
		name      string
		family    string
		websocket bool
		want      bool
	}{
		{name: "codex websocket", family: CodexFamily, websocket: true, want: true},
		{name: "codex http", family: CodexFamily, want: false},
		{name: "generic OpenAI websocket", family: Family, websocket: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{Family: tt.family, UseWebSocketTransport: tt.websocket}
			if got := client.StartupPrewarmEnabled(); got != tt.want {
				t.Fatalf("StartupPrewarmEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexPrewarmCancellationAfterCreatedReturnsWithoutLineage(t *testing.T) {
	accepted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		close(accepted)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.Dial(context.Background(), toWebSocketURL(server.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lineagePublished := false
	source := &wsFrameSource{
		ctx:      ctx,
		conn:     conn,
		buffered: &wsFrame{name: "response.created", data: []byte(`{"type":"response.created","response":{"id":"resp_cancelled"}}`)},
		onTerminal: func(string, []byte, bool) {
			lineagePublished = true
		},
	}
	stream := &stream{wsConn: source, model: lineageRequest("prewarm-cancel").Model, family: CodexFamily}
	<-accepted
	if ev, err := stream.Next(); err != nil || ev.Type != provider.EventActivity {
		t.Fatalf("created frame = (%+v, %v), want activity", ev, err)
	}
	cancel()

	if _, err := stream.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-created Next error = %v, want context.Canceled", err)
	}
	if lineagePublished {
		t.Fatal("canceled prewarm published response lineage")
	}
}

func TestCodexPrewarmSendsGenerateFalseAndEmptyInput(t *testing.T) {
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.created","response":{"id":"resp_warm"}}`,
		`{"type":"response.completed","response":{"id":"resp_warm"}}`,
	}}
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	if err := client.Prewarm(context.Background(), lineageRequest("prewarm-frame")); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}

	got := <-server.frames
	want := `{"type":"response.create","model":"gpt-5","input":[],"max_output_tokens":100,"store":false,"include":["reasoning.encrypted_content"],"reasoning":{"summary":"auto"},"prompt_cache_key":"prewarm-frame","generate":false}`
	if string(got) != want {
		t.Fatalf("prewarm frame = %s, want %s", got, want)
	}
}

func TestCodexPrewarmEstablishesEmptyOutputLineage(t *testing.T) {
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.created","response":{"id":"resp_warm"}}`,
		`{"type":"response.completed","response":{"id":"resp_warm"}}`,
	}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_real", "answer")}
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	if err := client.Prewarm(context.Background(), lineageRequest("prewarm-lineage")); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	streamLineageTurn(t, client, lineageRequest("prewarm-lineage", userMessage("question")))

	<-server.frames
	real := decodeResponseCreate(t, <-server.frames)
	if real.PreviousResponseID != "resp_warm" {
		t.Fatalf("previous_response_id = %q, want resp_warm", real.PreviousResponseID)
	}
	wantInput := rawItems(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}`)
	if !reflect.DeepEqual(real.Input, wantInput) {
		t.Fatalf("real input = %s, want suffix %s", real.Input, wantInput)
	}
	if real.Generate != nil {
		t.Fatalf("real generate = %v, want omitted", *real.Generate)
	}
}

func TestCodexPrewarmExistingInputDoesNotDropRealRequestInput(t *testing.T) {
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.created","response":{"id":"resp_warm"}}`,
		`{"type":"response.completed","response":{"id":"resp_warm"}}`,
	}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_real", "answer")}
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	if err := client.Prewarm(context.Background(), lineageRequest("prewarm-existing-input", userMessage("existing"))); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	streamLineageTurn(t, client, lineageRequest("prewarm-existing-input", userMessage("existing"), userMessage("new")))

	<-server.frames
	real := decodeResponseCreate(t, <-server.frames)
	if real.PreviousResponseID != "resp_warm" {
		t.Fatalf("previous_response_id = %q, want resp_warm", real.PreviousResponseID)
	}
	wantInput := rawItems(
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"existing"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"new"}]}`,
	)
	if !reflect.DeepEqual(real.Input, wantInput) {
		t.Fatalf("real input = %s, want all real-request input %s", real.Input, wantInput)
	}
}

func TestOpenAIFamilyPrewarmDoesNothing(t *testing.T) {
	client := &Client{Family: Family, UseWebSocketTransport: true}
	if err := client.Prewarm(context.Background(), &provider.Request{}); err != nil {
		t.Fatalf("Prewarm for OpenAI family: %v", err)
	}
}

func TestOrdinaryRequestStillRejectsEmptyInput(t *testing.T) {
	client := &Client{APIKey: "***", Family: CodexFamily, UseWebSocketTransport: true}
	_, err := client.Stream(context.Background(), lineageRequest("ordinary-empty"))
	if err == nil || !strings.Contains(err.Error(), "request has no transcodable messages") {
		t.Fatalf("Stream error = %v, want no transcodable messages", err)
	}
}

func TestPrewarmFailureLeavesFullRequestAvailable(t *testing.T) {
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.failed","response":{"error":{"code":"server_error","message":"warmup failed"}}}`,
	}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_real", "answer")}
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	if err := client.Prewarm(context.Background(), lineageRequest("prewarm-failure")); err == nil {
		t.Fatal("Prewarm succeeded, want failure")
	}
	streamLineageTurn(t, client, lineageRequest("prewarm-failure", userMessage("question")))

	<-server.frames
	real := decodeResponseCreate(t, <-server.frames)
	if real.PreviousResponseID != "" {
		t.Fatalf("previous_response_id = %q, want omitted after failed prewarm", real.PreviousResponseID)
	}
	wantInput := rawItems(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}`)
	if !reflect.DeepEqual(real.Input, wantInput) {
		t.Fatalf("real input = %s, want complete request %s", real.Input, wantInput)
	}
}
