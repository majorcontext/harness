package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

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

func TestWebSocketEmptyResponseIDDoesNotPublishLineage(t *testing.T) {
	server := newWSLineageServer(t)
	server.scripts <- wsLineageScript{beforeWait: []string{
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
