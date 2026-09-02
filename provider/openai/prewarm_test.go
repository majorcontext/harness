package openai

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/majorcontext/harness/provider"
)

var _ provider.StartupPrewarmer = (*Client)(nil)

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
	want := `{"type":"response.create","model":"gpt-5","input":[],"max_output_tokens":100,"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"prewarm-frame","generate":false}`
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
