package anthropic

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestStreamHTTPErrorClassification is the red-first test for GitHub issue
// #61: an Anthropic HTTP 529 (overloaded_error), 429 (rate limit), or any
// 5xx must come back from Stream marked provider.RetryableError so the goal
// loop's long backoff (engine/goal.go) can apply — never by the engine
// string-matching "overloaded_error" out of the error text. Every other
// status (400s, auth) must stay unmarked, so it keeps failing fast exactly
// as before.
func TestStreamHTTPErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		errType   string
		wantClass provider.RetryableClass
		wantRetry bool
	}{
		{"overloaded 529", 529, "overloaded_error", provider.RetryableOverloaded, true},
		{"rate limit 429", http.StatusTooManyRequests, "rate_limit_error", provider.RetryableRateLimited, true},
		{"internal 500", http.StatusInternalServerError, "api_error", provider.RetryableServerError, true},
		{"bad gateway 502", http.StatusBadGateway, "api_error", provider.RetryableServerError, true},
		{"bad request 400", http.StatusBadRequest, "invalid_request_error", "", false},
		{"auth 401", http.StatusUnauthorized, "authentication_error", "", false},
		{"not found 404", http.StatusNotFound, "not_found_error", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"type":"error","error":{"type":"`+tc.errType+`","message":"boom"}}`) //nolint:errcheck
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

// TestStreamInlineErrorClassification covers Anthropic's mid-stream "error"
// SSE event (no HTTP status to key off of — only the wire error "type"),
// which is exactly the shape the GitHub issue #61 incidents hit ("engine:
// goal loop stalled: anthropic: Overloaded (overloaded_error)").
func TestStreamInlineErrorClassification(t *testing.T) {
	cases := []struct {
		errType   string
		wantClass provider.RetryableClass
		wantRetry bool
	}{
		{"overloaded_error", provider.RetryableOverloaded, true},
		{"rate_limit_error", provider.RetryableRateLimited, true},
		{"api_error", provider.RetryableServerError, true},
		{"invalid_request_error", "", false},
		{"authentication_error", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.errType, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse("message_start", `{"type":"message_start","message":{"id":"msg_02","usage":{"input_tokens":1}}}`)) //nolint:errcheck
				io.WriteString(w, sse("error", `{"type":"error","error":{"type":"`+tc.errType+`","message":"boom"}}`))                   //nolint:errcheck
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
