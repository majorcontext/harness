package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/coder/websocket"
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
