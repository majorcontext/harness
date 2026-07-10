package openaicompat

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestStreamHTTPErrorClassification is the red-first test for GitHub issue
// #61 on the openaicompat family: a 429 or any 5xx must come back from
// Stream marked provider.RetryableError so the goal loop's long backoff
// (engine/goal.go) can apply. Every other status (400s, auth) must stay
// unmarked, so it keeps failing fast exactly as before.
func TestStreamHTTPErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantClass provider.RetryableClass
		wantRetry bool
	}{
		{"rate limit 429", http.StatusTooManyRequests, provider.RetryableRateLimited, true},
		{"internal 500", http.StatusInternalServerError, provider.RetryableServerError, true},
		{"bad gateway 502", http.StatusBadGateway, provider.RetryableServerError, true},
		{"service unavailable 503", http.StatusServiceUnavailable, provider.RetryableServerError, true},
		{"bad request 400", http.StatusBadRequest, "", false},
		{"auth 401", http.StatusUnauthorized, "", false},
		{"not found 404", http.StatusNotFound, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"error":{"message":"boom","type":"some_error","code":"x"}}`) //nolint:errcheck
			})
			_, err := c.Stream(context.Background(), &provider.Request{
				Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
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
