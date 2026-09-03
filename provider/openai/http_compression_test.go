package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func decodeZstdRequest(t *testing.T, r *http.Request) []byte {
	t.Helper()
	if got := r.Header.Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("Content-Encoding = %q, want zstd", got)
	}
	compressed, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatalf("decode zstd request: %v", err)
	}
	if bytes.Equal(decoded, compressed) {
		t.Fatal("zstd request body was not compressed")
	}
	return decoded
}

func compressionRequest(family string) *provider.Request {
	return &provider.Request{
		Model:      message.ModelRef{Provider: family, Model: "gpt-test"},
		System:     []string{"stable system"},
		Messages:   []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: strings.Repeat("compressible input ", 128)}}}},
		MaxTokens:  64,
		SessionKey: "session-compression",
	}
}

func writeCompletedResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, sse("response.created", `{"type":"response.created","response":{"id":"resp_compressed"}}`))
	_, _ = io.WriteString(w, sse("response.completed", `{"type":"response.completed","response":{"id":"resp_compressed","usage":{"input_tokens":10,"output_tokens":1}}}`))
}

func TestCodexHTTPCompressesRequestWithZstd(t *testing.T) {
	var decoded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded = decodeZstdRequest(t, r)
		writeCompletedResponse(w)
	}))
	defer server.Close()

	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily}
	stream, err := client.Stream(context.Background(), compressionRequest(CodexFamily))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	collect(t, stream)

	var wire apiRequest
	if err := json.Unmarshal(decoded, &wire); err != nil {
		t.Fatalf("decompressed request is not JSON: %v", err)
	}
	if wire.Model != "gpt-test" || wire.PromptCacheKey != "session-compression" || wire.Store {
		t.Fatalf("decompressed request = %+v", wire)
	}
}

func TestWebSocketFailureFallsBackToZstdHTTP(t *testing.T) {
	var httpCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "websocket disabled", http.StatusForbidden)
			return
		}
		httpCalls++
		decoded := decodeZstdRequest(t, r)
		if !json.Valid(decoded) {
			t.Fatalf("decompressed fallback body is not JSON: %q", decoded)
		}
		writeCompletedResponse(w)
	}))
	defer server.Close()

	client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}
	stream, err := client.Stream(context.Background(), compressionRequest(CodexFamily))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	collect(t, stream)
	if httpCalls != 1 {
		t.Fatalf("HTTP fallback calls = %d, want 1", httpCalls)
	}
}

func TestGenericOpenAIHTTPRemainsUncompressed(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding = %q, want absent", got)
		}
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		writeCompletedResponse(w)
	}))
	defer server.Close()

	client := &Client{APIKey: "test", BaseURL: server.URL, Family: Family}
	stream, err := client.Stream(context.Background(), compressionRequest(Family))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	collect(t, stream)
	if !json.Valid(body) {
		t.Fatalf("generic OpenAI body is not plain JSON: %q", body)
	}
}
