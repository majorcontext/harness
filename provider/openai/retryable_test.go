package openai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestStreamHTTPErrorClassification is the red-first test for GitHub issue
// #79: an OpenAI HTTP 429 (rate limit) or any 5xx must come back from Stream
// marked provider.RetryableError so the goal loop's long backoff
// (engine/goal.go) can apply — mirroring provider/anthropic's classifyStatus
// (see its TestStreamHTTPErrorClassification). Every other status (400s,
// auth) must stay unmarked, so it keeps failing fast exactly as before.
func TestStreamHTTPErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		errType   string
		wantClass provider.RetryableClass
		wantRetry bool
	}{
		{"rate limit 429", http.StatusTooManyRequests, "rate_limit_exceeded", provider.RetryableRateLimited, true},
		{"internal 500", http.StatusInternalServerError, "server_error", provider.RetryableServerError, true},
		{"bad gateway 502", http.StatusBadGateway, "server_error", provider.RetryableServerError, true},
		{"service unavailable 503", http.StatusServiceUnavailable, "server_error", provider.RetryableServerError, true},
		{"bad request 400", http.StatusBadRequest, "invalid_request_error", "", false},
		{"auth 401", http.StatusUnauthorized, "invalid_request_error", "", false},
		{"not found 404", http.StatusNotFound, "invalid_request_error", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"error":{"type":"`+tc.errType+`","message":"boom"}}`) //nolint:errcheck
			})
			_, err := c.Stream(context.Background(), &provider.Request{
				Model:     message.ModelRef{Provider: Family, Model: "m"},
				Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
				MaxTokens: 10,
			})
			if err == nil {
				t.Fatal("err = nil, want an HTTP error")
			}
			class, ok := provider.AsRetryable(err)
			if ok != tc.wantRetry {
				t.Fatalf("AsRetryable(%v) ok = %v, want %v", err, ok, tc.wantRetry)
			}
			if ok && class != tc.wantClass {
				t.Errorf("class = %q, want %q", class, tc.wantClass)
			}
		})
	}
}

// TestStreamInlineErrorClassification covers the Responses API's mid-stream
// "response.failed"/"error" events, which carry a wire "code" instead of an
// HTTP status — the equivalent of anthropic's classifyErrorType, keyed on
// the code vocabulary this wire actually uses.
func TestStreamInlineErrorClassification(t *testing.T) {
	cases := []struct {
		code      string
		wantClass provider.RetryableClass
		wantRetry bool
	}{
		{"rate_limit_exceeded", provider.RetryableRateLimited, true},
		{"server_error", provider.RetryableServerError, true},
		{"invalid_request_error", "", false},
		{"content_policy_violation", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse("response.created", `{"type":"response.created","response":{"id":"resp_9"}}`))                                 //nolint:errcheck
				io.WriteString(w, sse("response.failed", `{"type":"response.failed","response":{"error":{"code":"`+tc.code+`","message":"boom"}}}`)) //nolint:errcheck
			})
			s, err := c.Stream(context.Background(), &provider.Request{
				Model:     message.ModelRef{Provider: Family, Model: "m"},
				Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
				MaxTokens: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var streamErr error
			for streamErr == nil {
				_, streamErr = s.Next()
				if streamErr == io.EOF {
					t.Fatal("stream ended without an error")
				}
			}
			if !strings.Contains(streamErr.Error(), "boom") {
				t.Fatalf("err = %v, want it to contain %q", streamErr, "boom")
			}
			class, ok := provider.AsRetryable(streamErr)
			if ok != tc.wantRetry {
				t.Fatalf("AsRetryable(%v) ok = %v, want %v", streamErr, ok, tc.wantRetry)
			}
			if ok && class != tc.wantClass {
				t.Errorf("class = %q, want %q", class, tc.wantClass)
			}
		})
	}
}
