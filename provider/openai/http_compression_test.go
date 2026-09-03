package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func compressionRequestGolden(t *testing.T) []byte {
	t.Helper()
	text, err := json.Marshal(strings.Repeat("compressible input ", 128))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(`{"model":"gpt-test","instructions":"stable system","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(text) + `}]}],"max_output_tokens":64,"stream":true,"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"session-compression"}`)
}

func assertCompressionRequestGolden(t *testing.T, body []byte) {
	t.Helper()
	if want := compressionRequestGolden(t); !bytes.Equal(body, want) {
		t.Fatalf("request body differs from golden\n got: %s\nwant: %s", body, want)
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

	assertCompressionRequestGolden(t, decoded)
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
		assertCompressionRequestGolden(t, decoded)
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
	assertCompressionRequestGolden(t, body)
}

func TestZstdEncoderPoolWaitHonorsCancellation(t *testing.T) {
	pool := zstdRequestEncoderPool{capacity: 1}
	pool.initialize()
	pool.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pool.compress(ctx, []byte("request"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("compress error = %v, want context.Canceled", err)
	}
	<-pool.slots
}
