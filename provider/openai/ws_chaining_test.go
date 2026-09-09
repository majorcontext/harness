package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func rawItems(values ...string) []json.RawMessage {
	items := make([]json.RawMessage, len(values))
	for i, value := range values {
		items[i] = json.RawMessage(value)
	}
	return items
}

func TestIncrementalInputUsesSuffixAfterRequestAndResponsePrefix(t *testing.T) {
	previous := &apiRequest{Input: rawItems(`{"type":"message","role":"user","content":"one"}`)}
	responseItems := rawItems(`{"type":"message", "role":"assistant", "content":"two"}`)
	current := rawItems(
		`{ "content":"one", "role":"user", "type":"message" }`,
		`{"content":"two","role":"assistant","type":"message"}`,
		`{"type":"message","role":"user","content":"three"}`,
	)

	got, ok := incrementalInput(previous, responseItems, current)
	if !ok {
		t.Fatal("incrementalInput rejected a semantic request-and-response prefix")
	}
	want := rawItems(`{"type":"message","role":"user","content":"three"}`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incrementalInput = %s, want %s", got, want)
	}
}

func TestIncrementalInputRejectsChangedOrShortPrefix(t *testing.T) {
	previous := &apiRequest{Input: rawItems(`{"value":1}`)}
	responseItems := rawItems(`{"value":2}`)
	tests := []struct {
		name    string
		current []json.RawMessage
	}{
		{name: "changed request item", current: rawItems(`{"value":9}`, `{"value":2}`, `{"value":3}`)},
		{name: "changed response item", current: rawItems(`{"value":1}`, `{"value":9}`, `{"value":3}`)},
		{name: "short prefix", current: rawItems(`{"value":1}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := incrementalInput(previous, responseItems, tt.current); ok || got != nil {
				t.Fatalf("incrementalInput = (%s, %v), want (nil, false)", got, ok)
			}
		})
	}
}

func TestIncrementalInputRejectsAdjacentLargeIntegers(t *testing.T) {
	previous := &apiRequest{Input: rawItems(`{"value":9007199254740992}`)}
	current := rawItems(`{"value":9007199254740993}`, `{"value":"suffix"}`)

	if got, ok := incrementalInput(previous, nil, current); ok || got != nil {
		t.Fatalf("incrementalInput = (%s, %v), want (nil, false) for distinct large integers", got, ok)
	}
}

func TestResponsesRequestPropertiesRejectAdjacentLargeToolSchemaIntegers(t *testing.T) {
	previous := &apiRequest{Tools: []apiToolDef{{
		Type:       "function",
		Name:       "search",
		Parameters: json.RawMessage(`{"type":"integer","maximum":9007199254740992}`),
	}}}
	current := &apiRequest{Tools: []apiToolDef{{
		Type:       "function",
		Name:       "search",
		Parameters: json.RawMessage(`{"type":"integer","maximum":9007199254740993}`),
	}}}

	if responsesRequestPropertiesMatch(previous, current) {
		t.Fatal("responsesRequestPropertiesMatch accepted distinct adjacent large schema integers")
	}
}

func TestResponsesRequestPropertiesMatchCoversEveryField(t *testing.T) {
	temperature := 0.2
	topP := 0.8
	base := apiRequest{
		Model:           "gpt-5",
		Instructions:    "be exact",
		Input:           rawItems(`{"value":"old"}`),
		Tools:           []apiToolDef{{Type: "function", Name: "search", Description: "search", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Temperature:     &temperature,
		TopP:            &topP,
		MaxOutputTokens: 100,
		Stream:          true,
		Store:           false,
		Include:         []string{"reasoning.encrypted_content"},
		Reasoning:       &apiReasoning{Effort: "low"},
		PromptCacheKey:  "session",
		ServiceTier:     "priority",
	}
	if !responsesRequestPropertiesMatch(&base, &base) {
		t.Fatal("identical request properties do not match")
	}

	tests := []struct {
		field  string
		change func(*apiRequest)
	}{
		{field: "Model", change: func(r *apiRequest) { r.Model = "gpt-5-mini" }},
		{field: "Instructions", change: func(r *apiRequest) { r.Instructions = "be brief" }},
		{field: "Tools", change: func(r *apiRequest) { r.Tools[0].Name = "lookup" }},
		{field: "Temperature", change: func(r *apiRequest) { value := 0.3; r.Temperature = &value }},
		{field: "TopP", change: func(r *apiRequest) { value := 0.9; r.TopP = &value }},
		{field: "MaxOutputTokens", change: func(r *apiRequest) { r.MaxOutputTokens++ }},
		{field: "Store", change: func(r *apiRequest) { r.Store = true }},
		{field: "Include", change: func(r *apiRequest) { r.Include = []string{"other"} }},
		{field: "Reasoning", change: func(r *apiRequest) { r.Reasoning = &apiReasoning{Effort: "high"} }},
		{field: "PromptCacheKey", change: func(r *apiRequest) { r.PromptCacheKey = "other" }},
		{field: "ServiceTier", change: func(r *apiRequest) { r.ServiceTier = "default" }},
	}

	tested := make(map[string]bool, len(tests)+2)
	tested["Input"] = true
	tested["Stream"] = true
	for _, tt := range tests {
		tested[tt.field] = true
		t.Run(tt.field, func(t *testing.T) {
			current := base
			current.Tools = append([]apiToolDef(nil), base.Tools...)
			current.Include = append([]string(nil), base.Include...)
			tt.change(&current)
			if responsesRequestPropertiesMatch(&base, &current) {
				t.Fatalf("responsesRequestPropertiesMatch accepted changed %s", tt.field)
			}
		})
	}
	requestType := reflect.TypeOf(apiRequest{})
	for i := 0; i < requestType.NumField(); i++ {
		field := requestType.Field(i).Name
		if !tested[field] {
			t.Errorf("apiRequest field %s has no deliberate property comparison decision", field)
		}
	}
}

func TestResponseCreateAddsPreviousResponseIDAndSuffix(t *testing.T) {
	body := []byte(`{"model":"gpt-5","instructions":"be exact","input":[{"value":"complete"}],"max_output_tokens":100,"stream":true,"store":false,"include":["reasoning.encrypted_content"]}`)
	got := captureResponseCreate(t, body, responseCreateOptions{
		PreviousResponseID: "resp_123",
		Input:              rawItems(`{"value":"suffix"}`),
		InputSet:           true,
	})
	want := `{"type":"response.create","model":"gpt-5","instructions":"be exact","input":[{"value":"suffix"}],"max_output_tokens":100,"store":false,"include":["reasoning.encrypted_content"],"previous_response_id":"resp_123"}`
	assertJSONEqual(t, got, []byte(want))
}

func TestResponseCreatePrewarmAddsGenerateFalse(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":[{"value":"complete"}],"stream":true,"store":false,"include":["reasoning.encrypted_content"]}`)
	generate := false
	got := captureResponseCreate(t, body, responseCreateOptions{Generate: &generate})
	want := `{"type":"response.create","model":"gpt-5","input":[{"value":"complete"}],"store":false,"include":["reasoning.encrypted_content"],"generate":false}`
	assertJSONEqual(t, got, []byte(want))
}

func TestResponseCreateNormalRequestHasNoChainingFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":[{"value":"complete"}],"stream":true,"store":false,"include":["reasoning.encrypted_content"]}`)
	got := captureResponseCreate(t, body, responseCreateOptions{})
	want := `{"type":"response.create","model":"gpt-5","input":[{"value":"complete"}],"store":false,"include":["reasoning.encrypted_content"]}`
	assertJSONEqual(t, got, []byte(want))
}

func captureResponseCreate(t *testing.T, body []byte, options responseCreateOptions) []byte {
	t.Helper()
	frames := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		_, data, err := conn.Read(r.Context())
		if err == nil {
			frames <- data
		}
	}))
	t.Cleanup(server.Close)

	conn, _, err := websocket.Dial(context.Background(), toWebSocketURL(server.URL), nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	if err := sendResponseCreate(context.Background(), conn, body, options); err != nil {
		t.Fatalf("sendResponseCreate: %v", err)
	}
	return <-frames
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON: %v", err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode want JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

type wsLineageScript struct {
	beforeWait []string
	wait       <-chan struct{}
	afterWait  []string
}

type wsLineageServer struct {
	*httptest.Server
	scripts chan wsLineageScript
	frames  chan []byte
	// conns counts accepted websocket upgrades, so a recovery test can
	// assert a retry landed on a genuinely new connection instead of the
	// one whose conversation the server just rejected.
	conns int32
}

func (ts *wsLineageServer) connCount() int32 {
	return atomic.LoadInt32(&ts.conns)
}

func newWSLineageServer(t *testing.T) *wsLineageServer {
	t.Helper()
	ts := &wsLineageServer{
		scripts: make(chan wsLineageScript, 16),
		frames:  make(chan []byte, 16),
	}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, frame := range completedLineageFrames("resp_http", "http") {
				_, _ = io.WriteString(w, sse(wsFrameEventName(frame), frame))
			}
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		atomic.AddInt32(&ts.conns, 1)
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			_, frame, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			ts.frames <- append([]byte(nil), frame...)
			script := <-ts.scripts
			for _, response := range script.beforeWait {
				if conn.Write(context.Background(), websocket.MessageText, []byte(response)) != nil {
					return
				}
			}
			if script.wait != nil {
				<-script.wait
			}
			for _, response := range script.afterWait {
				if conn.Write(context.Background(), websocket.MessageText, []byte(response)) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func completedLineageFrames(responseID, text string) []string {
	return []string{
		`{"type":"response.created","response":{"id":"` + responseID + `"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"` + text + `"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}`,
		`{"type":"response.completed","response":{"id":"` + responseID + `"}}`,
	}
}

func lineageRequest(session string, messages ...message.Message) *provider.Request {
	return &provider.Request{
		Model:      message.ModelRef{Provider: CodexFamily, Model: "gpt-5"},
		Messages:   messages,
		MaxTokens:  100,
		SessionKey: session,
	}
}

func userMessage(text string) message.Message {
	return message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: text}}}
}

func assistantMessage(id, text string) message.Message {
	return message.Message{ID: id, Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}}
}

func streamLineageTurn(t *testing.T, client *Client, req *provider.Request) []provider.Event {
	t.Helper()
	stream, err := client.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	return collect(t, stream)
}

func decodeResponseCreate(t *testing.T, frame []byte) responseCreatePayload {
	t.Helper()
	var payload responseCreatePayload
	if err := json.Unmarshal(frame, &payload); err != nil {
		t.Fatalf("decode response.create: %v", err)
	}
	return payload
}

func TestWebSocketSecondTurnSendsOnlyIncrementalSuffix(t *testing.T) {
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.created","response":{"id":"resp_created"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"two"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"two"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_one"}}`,
	}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_two", "four")}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	streamLineageTurn(t, client, lineageRequest("suffix", userMessage("one")))
	streamLineageTurn(t, client, lineageRequest("suffix", userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three")))

	<-server.frames
	second := decodeResponseCreate(t, <-server.frames)
	if second.PreviousResponseID != "resp_one" {
		t.Fatalf("previous_response_id = %q, want resp_one", second.PreviousResponseID)
	}
	want := rawItems(`{"type":"message","role":"user","content":[{"type":"input_text","text":"three"}]}`)
	if !reflect.DeepEqual(second.Input, want) {
		t.Fatalf("second input = %s, want suffix %s", second.Input, want)
	}
}

func TestWebSocketTerminalEmptyResponseIDDoesNotReuseCreatedIDForLineage(t *testing.T) {
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.created","response":{"id":"resp_created_must_not_authorize_lineage"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"two"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"two"}]}}`,
		`{"type":"response.completed","response":{}}`,
	}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_two", "four")}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	streamLineageTurn(t, client, lineageRequest("empty-response-id", userMessage("one")))
	streamLineageTurn(t, client, lineageRequest("empty-response-id", userMessage("one"), assistantMessage("", "two"), userMessage("three")))

	<-server.frames
	second := decodeResponseCreate(t, <-server.frames)
	if second.PreviousResponseID != "" {
		t.Fatalf("previous_response_id = %q, want empty", second.PreviousResponseID)
	}
	if len(second.Input) != 3 {
		t.Fatalf("second input has %d items, want complete history after empty response ID: %s", len(second.Input), second.Input)
	}
}

func TestWebSocketToolRoundContinuesResponseLineage(t *testing.T) {
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.created","response":{"id":"resp_tool"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"one\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_tool"}}`,
	}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_after_tool", "done")}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	streamLineageTurn(t, client, lineageRequest("tool", userMessage("one")))
	assistant := message.Message{ID: "resp_tool", Role: message.RoleAssistant, Parts: message.Parts{
		&message.ToolCall{CallID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"one"}`)},
	}}
	result := message.Message{Role: message.RoleUser, Parts: message.Parts{&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "result"}}}}}
	streamLineageTurn(t, client, lineageRequest("tool", userMessage("one"), assistant, result))

	<-server.frames
	second := decodeResponseCreate(t, <-server.frames)
	if second.PreviousResponseID != "resp_tool" {
		t.Fatalf("previous_response_id = %q, want resp_tool", second.PreviousResponseID)
	}
	if len(second.Input) != 1 {
		t.Fatalf("second input has %d items, want only tool result: %s", len(second.Input), second.Input)
	}
	var item map[string]any
	if err := json.Unmarshal(second.Input[0], &item); err != nil {
		t.Fatal(err)
	}
	if item["type"] != "function_call_output" || item["output"] != "result" {
		t.Fatalf("second input = %s, want function_call_output suffix", second.Input)
	}
}

func TestWebSocketFullMismatchReestablishesLineage(t *testing.T) {
	server := newWSLineageServer(t)
	for _, responseID := range []string{"resp_one", "resp_two", "resp_three"} {
		server.scripts <- wsLineageScript{beforeWait: completedLineageFrames(responseID, "two")}
	}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	streamLineageTurn(t, client, lineageRequest("mismatch", userMessage("one")))
	mismatch := lineageRequest("mismatch", userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three"))
	mismatch.MaxTokens = 200
	streamLineageTurn(t, client, mismatch)
	thirdRequest := lineageRequest("mismatch", userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three"), assistantMessage("resp_two", "two"), userMessage("five"))
	thirdRequest.MaxTokens = 200
	streamLineageTurn(t, client, thirdRequest)

	<-server.frames
	second := decodeResponseCreate(t, <-server.frames)
	if second.PreviousResponseID != "" {
		t.Fatalf("mismatch previous_response_id = %q, want empty", second.PreviousResponseID)
	}
	if len(second.Input) != 3 {
		t.Fatalf("mismatch input has %d items, want complete history: %s", len(second.Input), second.Input)
	}
	third := decodeResponseCreate(t, <-server.frames)
	if third.PreviousResponseID != "resp_two" || len(third.Input) != 1 {
		t.Fatalf("full mismatch did not reestablish lineage: previous=%q input=%s", third.PreviousResponseID, third.Input)
	}
}

func TestWebSocketStaleGenerationCannotRearmLineage(t *testing.T) {
	release := make(chan struct{})
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{
		beforeWait: []string{`{"type":"response.created","response":{"id":"resp_stale"}}`},
		wait:       release,
		afterWait:  completedLineageFrames("resp_stale", "two")[1:],
	}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_next", "four")}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	stream, err := client.Stream(context.Background(), lineageRequest("stale", userMessage("one")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	streamLineageTurn(t, client, lineageRequest("stale", userMessage("invalidate generation")))
	close(release)
	collect(t, stream)
	_ = stream.Close()
	streamLineageTurn(t, client, lineageRequest("stale", userMessage("one"), assistantMessage("resp_stale", "two"), userMessage("three")))

	<-server.frames
	second := decodeResponseCreate(t, <-server.frames)
	if second.PreviousResponseID != "" || len(second.Input) != 3 {
		t.Fatalf("stale completion rearmed lineage: previous=%q input=%s", second.PreviousResponseID, second.Input)
	}
}

func TestWebSocketConcurrentFallbackCannotRearmLineage(t *testing.T) {
	release := make(chan struct{})
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{
		beforeWait: []string{`{"type":"response.created","response":{"id":"resp_stale"}}`},
		wait:       release,
		afterWait:  completedLineageFrames("resp_stale", "two")[1:],
	}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_next", "four")}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	first, err := client.Stream(context.Background(), lineageRequest("concurrent", userMessage("one")))
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	streamLineageTurn(t, client, lineageRequest("concurrent", userMessage("competing")))
	close(release)
	collect(t, first)
	_ = first.Close()
	streamLineageTurn(t, client, lineageRequest("concurrent", userMessage("one"), assistantMessage("resp_stale", "two"), userMessage("three")))

	<-server.frames
	secondWS := decodeResponseCreate(t, <-server.frames)
	if secondWS.PreviousResponseID != "" || len(secondWS.Input) != 3 {
		t.Fatalf("concurrent fallback allowed stale lineage: previous=%q input=%s", secondWS.PreviousResponseID, secondWS.Input)
	}
}

func TestWebSocketResponseItemsMatchTextCallsAndReasoning(t *testing.T) {
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.created","response":{"id":"resp_mixed"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"thinking done"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"thinking done"}]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"one\"}"}}`,
		`{"type":"response.output_item.done","output_index":2,"item":{"type":"reasoning","id":"reason_1","encrypted_content":"opaque"}}`,
		`{"type":"response.completed","response":{"id":"resp_mixed"}}`,
	}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_next", "done")}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	events := streamLineageTurn(t, client, lineageRequest("mixed", userMessage("one")))
	var assistant *message.Message
	for i := range events {
		if events[i].Type == provider.EventDone {
			assistant = events[i].Message
		}
	}
	if assistant == nil {
		t.Fatal("first turn did not return an assistant message")
	}
	result := message.Message{Role: message.RoleUser, Parts: message.Parts{&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "result"}}}}}
	streamLineageTurn(t, client, lineageRequest("mixed", userMessage("one"), *assistant, result))

	<-server.frames
	second := decodeResponseCreate(t, <-server.frames)
	if second.PreviousResponseID != "resp_mixed" || len(second.Input) != 1 {
		t.Fatalf("mixed response items did not match: previous=%q input=%s", second.PreviousResponseID, second.Input)
	}
	var suffix map[string]any
	if err := json.Unmarshal(second.Input[0], &suffix); err != nil {
		t.Fatal(err)
	}
	if suffix["type"] != "function_call_output" {
		t.Fatalf("mixed suffix = %s, want only function_call_output", second.Input)
	}
}

func chainMissFrame() string {
	return `{"type":"error","code":"previous_response_not_found","message":"lineage expired"}`
}

func drainLineageStream(stream provider.Stream) ([]provider.Event, error) {
	var events []provider.Event
	for {
		event, err := stream.Next()
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
}

func establishRecoveryLineage(t *testing.T, server *wsLineageServer, client *Client, session string) {
	t.Helper()
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_secret_lineage", "two")}
	streamLineageTurn(t, client, lineageRequest(session, userMessage("one")))
	<-server.frames
}

func TestPreviousResponseNotFoundRetriesFullRequestOnce(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	establishRecoveryLineage(t, server, client, "chain-miss-recovery")
	server.scripts <- wsLineageScript{beforeWait: []string{chainMissFrame()}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_recovered", "four")}

	events := streamLineageTurn(t, client, lineageRequest("chain-miss-recovery", userMessage("one"), assistantMessage("resp_secret_lineage", "two"), userMessage("three")))
	incremental := decodeResponseCreate(t, <-server.frames)
	fullRetry := decodeResponseCreate(t, <-server.frames)
	if incremental.PreviousResponseID != "resp_secret_lineage" || len(incremental.Input) != 1 {
		t.Fatalf("initial request = previous %q, %d items; want incremental lineage request", incremental.PreviousResponseID, len(incremental.Input))
	}
	if fullRetry.PreviousResponseID != "" || len(fullRetry.Input) != 3 {
		t.Fatalf("recovery request = previous %q, %d items; want complete request without lineage", fullRetry.PreviousResponseID, len(fullRetry.Input))
	}
	terminal := events[len(events)-1]
	if terminal.Type != provider.EventDone || terminal.RequestMetadata == nil {
		t.Fatalf("terminal event = %+v, want EventDone with request metadata", terminal)
	}
	want := provider.RequestMetadata{Mode: provider.RequestModeFull, CompleteInputItems: 3, SentInputItems: 3, PreviousResponseUsed: false, ChainRecovered: true}
	if !reflect.DeepEqual(*terminal.RequestMetadata, want) {
		t.Fatalf("request metadata = %+v, want %+v", *terminal.RequestMetadata, want)
	}
	raw, err := json.Marshal(terminal.RequestMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "resp_secret_lineage") {
		t.Fatalf("terminal metadata leaked response ID: %s", raw)
	}
}

func TestPreviousResponseNotFoundAfterVisibleOutputDoesNotRetry(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	establishRecoveryLineage(t, server, client, "chain-miss-visible")
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.output_text.delta","output_index":0,"delta":"visible"}`,
		chainMissFrame(),
	}}

	stream, err := client.Stream(context.Background(), lineageRequest("chain-miss-visible", userMessage("one"), assistantMessage("resp_secret_lineage", "two"), userMessage("three")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	events, streamErr := drainLineageStream(stream)
	if len(events) != 1 || events[0].Type != provider.EventTextDelta {
		t.Fatalf("events = %+v, want one visible text delta", events)
	}
	class, ok := provider.AsRetryable(streamErr)
	if !ok || class != provider.RetryableStreamTruncated {
		t.Fatalf("AsRetryable(%v) = %q, %v; want %q, true", streamErr, class, ok, provider.RetryableStreamTruncated)
	}
	<-server.frames
	if got := len(server.frames); got != 0 {
		t.Fatalf("extra websocket frames = %d, want no local retry after visible output", got)
	}
}

func TestPreviousResponseNotFoundSecondFailureEscapes(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	establishRecoveryLineage(t, server, client, "chain-miss-twice")
	server.scripts <- wsLineageScript{beforeWait: []string{chainMissFrame()}}
	server.scripts <- wsLineageScript{beforeWait: []string{chainMissFrame()}}

	stream, err := client.Stream(context.Background(), lineageRequest("chain-miss-twice", userMessage("one"), assistantMessage("resp_secret_lineage", "two"), userMessage("three")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	_, streamErr := drainLineageStream(stream)
	if streamErr == nil || !strings.Contains(streamErr.Error(), "previous_response_not_found") {
		t.Fatalf("stream error = %v, want second chain miss to escape", streamErr)
	}
	<-server.frames
	<-server.frames
	if got := len(server.frames); got != 0 {
		t.Fatalf("extra websocket frames = %d, want exactly one local retry", got)
	}
}

func TestPreviousResponseNotFoundAfterResponseCreatedDoesNotRetry(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	establishRecoveryLineage(t, server, client, "chain-miss-after-created")
	entry := client.wsPoolFor().entryFor("chain-miss-after-created")
	entry.mu.Lock()
	generationBefore := entry.generation
	entry.mu.Unlock()
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"response.created","response":{"id":"resp_started"}}`,
		chainMissFrame(),
	}}
	// Keep the old, incorrect retry path bounded: it consumes this script.
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_wrong_retry", "wrong")}

	stream, err := client.Stream(context.Background(), lineageRequest("chain-miss-after-created", userMessage("one"), assistantMessage("resp_secret_lineage", "two"), userMessage("three")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	<-server.frames
	events, streamErr := drainLineageStream(stream)
	if streamErr == nil || streamErr == io.EOF || !strings.Contains(streamErr.Error(), "previous_response_not_found") {
		t.Fatalf("stream error = %v, want chain miss after preceding response.created to escape", streamErr)
	}
	if len(events) != 1 || events[0].Type != provider.EventActivity {
		t.Fatalf("events = %+v, want only response.created activity before chain miss", events)
	}
	if got := len(server.frames); got != 0 {
		t.Fatalf("extra websocket frames = %d, want no local retry when chain miss is not first frame", got)
	}
	entry.mu.Lock()
	generationAfter := entry.generation
	entry.mu.Unlock()
	if generationAfter != generationBefore+1 {
		t.Fatalf("connection generation = %d after non-first chain miss, want one invalidation from %d to %d", generationAfter, generationBefore, generationBefore+1)
	}
}

func TestPreviousResponseNotFoundCodeOnlyDoesNotLeaveEntryBusy(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	establishRecoveryLineage(t, server, client, "chain-miss-code-only")
	server.scripts <- wsLineageScript{beforeWait: []string{
		`{"type":"error","code":"previous_response_not_found"}`,
	}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_recovered", "four")}

	events := streamLineageTurn(t, client, lineageRequest("chain-miss-code-only", userMessage("one"), assistantMessage("resp_secret_lineage", "two"), userMessage("three")))
	incremental := decodeResponseCreate(t, <-server.frames)
	fullRetry := decodeResponseCreate(t, <-server.frames)
	if incremental.PreviousResponseID == "" || fullRetry.PreviousResponseID != "" {
		t.Fatalf("requests = incremental previous %q, retry previous %q; want chained request then complete recovery", incremental.PreviousResponseID, fullRetry.PreviousResponseID)
	}
	if terminal := events[len(events)-1]; terminal.Type != provider.EventDone {
		t.Fatalf("terminal event = %+v, want recovered EventDone", terminal)
	}
	entry := client.wsPoolFor().entryFor("chain-miss-code-only")
	entry.mu.Lock()
	busy := entry.busy
	entry.mu.Unlock()
	if busy {
		t.Fatal("pool entry remains busy after code-only chain-miss recovery")
	}
}

// http404ChainMissFrame mirrors chainMissFrame's shape but with the plain
// HTTP-status vocabulary ("404") the live Codex backend has also been
// observed using for an identical "this response/conversation is gone"
// rejection, instead of the literal previous_response_not_found code.
func http404ChainMissFrame() string {
	return `{"type":"error","code":"404","message":"conversation not found"}`
}

func TestNotFoundHTTPStatusCodeRecoversLikeChainMiss(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	establishRecoveryLineage(t, server, client, "http-status-recovery")
	server.scripts <- wsLineageScript{beforeWait: []string{http404ChainMissFrame()}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_recovered", "four")}

	events := streamLineageTurn(t, client, lineageRequest("http-status-recovery", userMessage("one"), assistantMessage("resp_secret_lineage", "two"), userMessage("three")))
	incremental := decodeResponseCreate(t, <-server.frames)
	fullRetry := decodeResponseCreate(t, <-server.frames)
	if incremental.PreviousResponseID != "resp_secret_lineage" {
		t.Fatalf("initial request previous_response_id = %q, want resp_secret_lineage", incremental.PreviousResponseID)
	}
	if fullRetry.PreviousResponseID != "" || len(fullRetry.Input) != 3 {
		t.Fatalf("recovery request = previous %q, %d items; want complete request without lineage", fullRetry.PreviousResponseID, len(fullRetry.Input))
	}
	terminal := events[len(events)-1]
	if terminal.Type != provider.EventDone || terminal.RequestMetadata == nil || !terminal.RequestMetadata.ChainRecovered {
		t.Fatalf("terminal event = %+v, want a recovered EventDone", terminal)
	}
}

func TestChainMissRecoveryDialsFreshConnection(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	establishRecoveryLineage(t, server, client, "fresh-dial-recovery")
	connsBefore := server.connCount()
	server.scripts <- wsLineageScript{beforeWait: []string{chainMissFrame()}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_recovered", "four")}

	streamLineageTurn(t, client, lineageRequest("fresh-dial-recovery", userMessage("one"), assistantMessage("resp_secret_lineage", "two"), userMessage("three")))

	if connsAfter := server.connCount(); connsAfter != connsBefore+1 {
		t.Fatalf("connection count after recovery = %d, want %d: recovery must dial a fresh connection instead of resending on the one the server just rejected", connsAfter, connsBefore+1)
	}
}

// TestPropertyMismatchNotFoundOnReusedConnectionRecovers covers a model
// switch: responsesRequestPropertiesMatch already refuses to chain a
// request whose properties (e.g. Model) changed, so the request that hits
// the wire is a FULL request with no previous_response_id. But the pooled
// connection sending it can still be the same socket the server evicted —
// so a not-found on that connection's first frame must recover exactly
// like an explicit previous_response_id chain miss, not hard-error.
func TestPropertyMismatchNotFoundOnReusedConnectionRecovers(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	establishRecoveryLineage(t, server, client, "mismatch-recovery")
	connsBefore := server.connCount()
	server.scripts <- wsLineageScript{beforeWait: []string{chainMissFrame()}}
	server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_after_switch", "done")}

	mismatch := lineageRequest("mismatch-recovery", userMessage("one"), assistantMessage("resp_secret_lineage", "two"), userMessage("three"))
	mismatch.MaxTokens = 200 // forces a property mismatch: a complete, non-chained request
	events := streamLineageTurn(t, client, mismatch)

	sent := decodeResponseCreate(t, <-server.frames)
	if sent.PreviousResponseID != "" || len(sent.Input) != 3 {
		t.Fatalf("mismatched request = previous %q, %d items; want a complete request with no chaining", sent.PreviousResponseID, len(sent.Input))
	}
	fullRetry := decodeResponseCreate(t, <-server.frames)
	if fullRetry.PreviousResponseID != "" || len(fullRetry.Input) != 3 {
		t.Fatalf("recovery retry = previous %q, %d items; want complete request without lineage", fullRetry.PreviousResponseID, len(fullRetry.Input))
	}
	terminal := events[len(events)-1]
	if terminal.Type != provider.EventDone || terminal.RequestMetadata == nil || !terminal.RequestMetadata.ChainRecovered {
		t.Fatalf("terminal event = %+v, want a recovered EventDone", terminal)
	}
	if connsAfter := server.connCount(); connsAfter != connsBefore+1 {
		t.Fatalf("connection count after recovery = %d, want %d", connsAfter, connsBefore+1)
	}
}

// TestFreshDialFirstTurnNotFoundDoesNotRecover guards the other edge: a
// session's very first request, on a freshly dialed connection, that is
// also not chained has no stale conversation to recover from — a
// not-found there is a genuine error, not a chain miss.
func TestFreshDialFirstTurnNotFoundDoesNotRecover(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	server.scripts <- wsLineageScript{beforeWait: []string{chainMissFrame()}}

	stream, err := client.Stream(context.Background(), lineageRequest("cold-start", userMessage("one")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	_, streamErr := drainLineageStream(stream)
	if streamErr == nil || !strings.Contains(streamErr.Error(), "previous_response_not_found") {
		t.Fatalf("stream error = %v, want a cold-start chain miss (nothing stale to recover from) to escape", streamErr)
	}
	<-server.frames // the one legitimate request
	if got := len(server.frames); got != 0 {
		t.Fatalf("extra websocket frames = %d, want no retry on a fresh dial's first-ever request", got)
	}
}

func TestNotFoundAfterRecoveryDialEscapesWithoutInfiniteRetry(t *testing.T) {
	server := newWSLineageServer(t)
	client := &Client{APIKey: "***", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	establishRecoveryLineage(t, server, client, "double-miss")
	connsBefore := server.connCount()
	server.scripts <- wsLineageScript{beforeWait: []string{chainMissFrame()}}
	server.scripts <- wsLineageScript{beforeWait: []string{chainMissFrame()}}

	stream, err := client.Stream(context.Background(), lineageRequest("double-miss", userMessage("one"), assistantMessage("resp_secret_lineage", "two"), userMessage("three")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	_, streamErr := drainLineageStream(stream)
	if streamErr == nil || !strings.Contains(streamErr.Error(), "previous_response_not_found") {
		t.Fatalf("stream error = %v, want the second chain miss (on the freshly dialed connection) to escape", streamErr)
	}
	if connsAfter := server.connCount(); connsAfter != connsBefore+1 {
		t.Fatalf("connection count = %d, want exactly %d: one recovery dial and no further retries", connsAfter, connsBefore+1)
	}
}

// TestResponsesRequestPropertyDiffNamesTheChangedProperty states the
// operator-facing half of a property refusal: knowing that a request could
// not chain is not actionable, knowing WHICH context-bearing property moved
// is. Every name is a wire field name, never a value, so nothing in a
// refusal reason can carry prompt content.
func TestResponsesRequestPropertyDiffNamesTheChangedProperty(t *testing.T) {
	base := func() *apiRequest {
		return &apiRequest{
			Model:           "gpt-5",
			Instructions:    "be brief",
			MaxOutputTokens: 100,
			PromptCacheKey:  "ses_1",
			ServiceTier:     "ultrafast",
			Tools:           []apiToolDef{{Type: "function", Name: "bash", Parameters: json.RawMessage(`{"type":"object"}`)}},
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*apiRequest)
		want   string
	}{
		{name: "identical", mutate: func(*apiRequest) {}, want: ""},
		{name: "model", mutate: func(r *apiRequest) { r.Model = "gpt-6" }, want: "model"},
		{name: "instructions", mutate: func(r *apiRequest) { r.Instructions = "be terse" }, want: "instructions"},
		{name: "tools", mutate: func(r *apiRequest) {
			r.Tools = append(r.Tools, apiToolDef{Type: "function", Name: "edit_file", Parameters: json.RawMessage(`{"type":"object"}`)})
		}, want: "tools"},
		{name: "max_output_tokens", mutate: func(r *apiRequest) { r.MaxOutputTokens = 200 }, want: "max_output_tokens"},
		{name: "prompt_cache_key", mutate: func(r *apiRequest) { r.PromptCacheKey = "ses_2" }, want: "prompt_cache_key"},
		{name: "service_tier", mutate: func(r *apiRequest) { r.ServiceTier = "standard" }, want: "service_tier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous, current := base(), base()
			tc.mutate(current)
			if got := responsesRequestPropertyDiff(previous, current); got != tc.want {
				t.Fatalf("responsesRequestPropertyDiff = %q, want %q", got, tc.want)
			}
			if want := tc.want == ""; responsesRequestPropertiesMatch(previous, current) != want {
				t.Fatalf("responsesRequestPropertiesMatch = %v, want %v", !want, want)
			}
		})
	}
}

// TestIncrementalInputDiffReportsFirstChangedItem pins the second half of a
// refusal reason: which input item stopped matching. An index localizes the
// culprit (a mutated ambient status block lands on the newest user message,
// a compaction rewrite lands early) without exporting any item content.
func TestIncrementalInputDiffReportsFirstChangedItem(t *testing.T) {
	previous := &apiRequest{Input: []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":"one"}`),
	}}
	responseItems := []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","content":"two"}`)}
	suffix := json.RawMessage(`{"type":"function_call_output","call_id":"c1","output":"three"}`)

	t.Run("match", func(t *testing.T) {
		current := []json.RawMessage{previous.Input[0], responseItems[0], suffix}
		got, index, ok := incrementalInputDiff(previous, responseItems, current)
		if !ok {
			t.Fatalf("refused a matching prefix at item %d", index)
		}
		if len(got) != 1 || string(got[0]) != string(suffix) {
			t.Fatalf("suffix = %s, want %s", got, suffix)
		}
		if index != -1 {
			t.Fatalf("index = %d, want -1 on a match", index)
		}
	})

	t.Run("changed request item", func(t *testing.T) {
		changed := json.RawMessage(`{"type":"message","role":"user","content":"one!"}`)
		current := []json.RawMessage{changed, responseItems[0], suffix}
		if _, index, ok := incrementalInputDiff(previous, responseItems, current); ok || index != 0 {
			t.Fatalf("diff = (index %d, ok %v), want (0, false)", index, ok)
		}
	})

	t.Run("changed response item", func(t *testing.T) {
		changed := json.RawMessage(`{"type":"message","role":"assistant","content":"two!"}`)
		current := []json.RawMessage{previous.Input[0], changed, suffix}
		if _, index, ok := incrementalInputDiff(previous, responseItems, current); ok || index != 1 {
			t.Fatalf("diff = (index %d, ok %v), want (1, false)", index, ok)
		}
	})

	t.Run("shorter than the prefix", func(t *testing.T) {
		current := []json.RawMessage{previous.Input[0]}
		if _, index, ok := incrementalInputDiff(previous, responseItems, current); ok || index != -1 {
			t.Fatalf("diff = (index %d, ok %v), want (-1, false)", index, ok)
		}
	})
}

func lineageTerminalMetadata(t *testing.T, events []provider.Event) provider.RequestMetadata {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	terminal := events[len(events)-1]
	if terminal.Type != provider.EventDone || terminal.RequestMetadata == nil {
		t.Fatalf("terminal event = %+v, want EventDone with request metadata", terminal)
	}
	return *terminal.RequestMetadata
}

// TestWebSocketChainRefusalMetadataNamesTheReason is the whole point of the
// refusal vocabulary: a full-mode call re-sends the entire input uncached,
// and today's metadata reports only THAT it happened. Each case below drives
// one distinct cause through the real pool and asserts the reported reason.
func TestWebSocketChainRefusalMetadataNamesTheReason(t *testing.T) {
	server := newWSLineageServer(t)
	for _, responseID := range []string{"resp_one", "resp_two", "resp_three"} {
		server.scripts <- wsLineageScript{beforeWait: completedLineageFrames(responseID, "two")}
	}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	first := lineageTerminalMetadata(t, streamLineageTurn(t, client, lineageRequest("refusal", userMessage("one"))))
	if first.ChainRefusal != provider.ChainRefusalNoLineage || first.ChainRefusalDetail != "" {
		t.Fatalf("first turn refusal = %q/%q, want %q with no detail", first.ChainRefusal, first.ChainRefusalDetail, provider.ChainRefusalNoLineage)
	}

	property := lineageRequest("refusal", userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three"))
	property.MaxTokens = 200
	got := lineageTerminalMetadata(t, streamLineageTurn(t, client, property))
	if got.ChainRefusal != provider.ChainRefusalPropertyChanged || got.ChainRefusalDetail != "max_output_tokens" {
		t.Fatalf("property refusal = %q/%q, want %q/%q", got.ChainRefusal, got.ChainRefusalDetail, provider.ChainRefusalPropertyChanged, "max_output_tokens")
	}
	if got.ChainRefusalItem != nil {
		t.Fatalf("property refusal item = %v, want none: only a prefix refusal has an item", got.ChainRefusalItem)
	}

	// Same properties as the call that installed resp_two, but its first
	// input item is no longer byte-identical -- exactly what a re-rendered
	// ambient status block did before it became chain-stable.
	prefix := lineageRequest("refusal", userMessage("one!"), assistantMessage("resp_one", "two"), userMessage("three"), assistantMessage("resp_two", "two"), userMessage("five"))
	prefix.MaxTokens = 200
	got = lineageTerminalMetadata(t, streamLineageTurn(t, client, prefix))
	if got.ChainRefusal != provider.ChainRefusalPrefixChanged {
		t.Fatalf("prefix refusal = %q, want %q", got.ChainRefusal, provider.ChainRefusalPrefixChanged)
	}
	// The index rides its own numeric field. Item 0 is also the value a
	// plain int field cannot tell apart from "no item".
	if got.ChainRefusalItem == nil || *got.ChainRefusalItem != 0 {
		t.Fatalf("prefix refusal item = %v, want a reported 0", got.ChainRefusalItem)
	}
	if got.ChainRefusalDetail != "" {
		t.Fatalf("prefix refusal detail = %q, want empty: the index is not a detail string", got.ChainRefusalDetail)
	}
	if got.Mode != provider.RequestModeFull || got.PreviousResponseUsed {
		t.Fatalf("prefix refusal metadata = %+v, want a full, unchained request", got)
	}
}

// TestInputItemLocatorKeepsTheIndexOutOfTheDetailString pins the reported
// regression. inputItemLocator used to render "input[<n>]" into
// ChainRefusalDetail. BetterStack ingest reads that value as a path
// expression and splits it, so a live row stored chain_refusal_detail="input"
// with the index moved to a sibling chain_refusal_detail_json=[139] field
// that no dashboard reads: the operator saw THAT request assembly rewrote
// history, never WHICH item. The index must travel as a number, and every
// detail string this adapter reports must be free of the "[" that triggers
// the split.
func TestInputItemLocatorKeepsTheIndexOutOfTheDetailString(t *testing.T) {
	for _, index := range []int{0, 1, 4, 85, 139} {
		item, detail := inputItemLocator(index)
		if item == nil || *item != index {
			t.Errorf("locator(%d) item = %v, want %d", index, item, index)
		}
		if detail != "" {
			t.Errorf("locator(%d) detail = %q, want empty", index, detail)
		}
	}

	// A current input too short to extend the prefix at all has no index to
	// report, so this case keeps a bracket-free detail string instead.
	item, detail := inputItemLocator(-1)
	if item != nil {
		t.Errorf("locator(-1) item = %v, want none", item)
	}
	if detail != "input_shorter_than_prefix" {
		t.Errorf("locator(-1) detail = %q, want %q", detail, "input_shorter_than_prefix")
	}
}

// TestWebSocketChainedTurnReportsNoRefusal asserts the surplus half: a call
// that DID chain must carry no refusal reason, so a log query can count
// refusals without subtracting chained calls.
func TestWebSocketChainedTurnReportsNoRefusal(t *testing.T) {
	server := newWSLineageServer(t)
	for _, responseID := range []string{"resp_one", "resp_two"} {
		server.scripts <- wsLineageScript{beforeWait: completedLineageFrames(responseID, "two")}
	}
	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

	streamLineageTurn(t, client, lineageRequest("chained", userMessage("one")))
	got := lineageTerminalMetadata(t, streamLineageTurn(t, client, lineageRequest("chained", userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three"))))
	if !got.PreviousResponseUsed || got.Mode != provider.RequestModeIncremental {
		t.Fatalf("second turn metadata = %+v, want an incremental chained request", got)
	}
	if got.ChainRefusal != provider.ChainRefusalNone || got.ChainRefusalDetail != "" {
		t.Fatalf("chained turn reported refusal %q/%q, want none", got.ChainRefusal, got.ChainRefusalDetail)
	}
}

// ageEntry pushes one pool entry's connection lifestamps into the past, so
// a test reaches the reuse-refused paths under the PRODUCTION idle timeout
// and maximum connection age instead of shrinking either one (the idle
// timeout also bounds every frame read, so a tiny value breaks the read
// before it can expire a connection).
func ageEntry(t *testing.T, pool *wsPool, sessionKey string, idle, age time.Duration) {
	t.Helper()
	entry := pool.entryFor(sessionKey)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.conn == nil {
		t.Fatal("pool entry holds no connection to age")
	}
	entry.lastUsedAt = entry.lastUsedAt.Add(-idle)
	entry.connectedAt = entry.connectedAt.Add(-age)
}

// TestWebSocketDroppedConnectionRefusalKeepsItsCause is the red-first guard
// for the reason a lost pooled connection reports. A dropped socket takes
// its lineage with it, so the generic "no usable lineage" answer is true but
// useless: it hides the fact that the fleet paid a whole uncached re-send
// for ordinary think time between two turns. The specific cause must win.
func TestWebSocketDroppedConnectionRefusalKeepsItsCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		idle time.Duration
		age  time.Duration
		want provider.ChainRefusal
	}{
		{name: "idle", idle: 2 * wsDefaultIdleTimeout, want: provider.ChainRefusalConnectionIdle},
		{name: "aged", age: 2 * wsDefaultMaxConnectionAge, want: provider.ChainRefusalConnectionAged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newWSLineageServer(t)
			for _, responseID := range []string{"resp_one", "resp_two"} {
				server.scripts <- wsLineageScript{beforeWait: completedLineageFrames(responseID, "two")}
			}
			client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
			session := "dropped-" + tc.name

			streamLineageTurn(t, client, lineageRequest(session, userMessage("one")))
			ageEntry(t, client.wsPoolFor(), session, tc.idle, tc.age)
			second := lineageTerminalMetadata(t, streamLineageTurn(t, client, lineageRequest(session, userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three"))))
			if second.ChainRefusal != tc.want {
				t.Fatalf("refusal = %q, want %q", second.ChainRefusal, tc.want)
			}
			if second.Mode != provider.RequestModeFull {
				t.Fatalf("mode = %q, want a full request", second.Mode)
			}
		})
	}
}
