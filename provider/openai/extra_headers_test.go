package openai

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// captureHeaders runs one Stream call against a stub server and returns the
// request headers it saw.
func captureHeaders(t *testing.T, c *Client) http.Header {
	t.Helper()
	seen := make(chan http.Header, 1)
	base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamFixture) //nolint:errcheck
	})
	c.BaseURL = base.BaseURL
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: Family, Model: "gpt-5"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return <-seen
}

// This adapter is one of two paths Bifrost sees traffic through, so without
// ExtraHeaders that traffic reaches the gateway carrying no identity and its
// spend cannot be attributed to anyone. The fixed headers are the request's
// authentication and protocol contract, so an extra with the same name must
// not displace them.
func TestExtraHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, header, extraValue, want string
	}{
		{"reaches the upstream", "X-Neptune-User", "someone@example.com", "someone@example.com"},
		{"cannot override Authorization", "Authorization", "Bearer stolen", "Bearer test-key"},
		{"cannot override Content-Type", "Content-Type", "text/plain", "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := captureHeaders(t, &Client{
				APIKey:       "test-key",
				ExtraHeaders: map[string]string{tc.header: tc.extraValue},
			})
			if got := h.Get(tc.header); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// With no extras configured the fixed headers must still be intact, so the
// new loop cannot disturb a request that sets none.
func TestNoExtraHeadersKeepsFixedHeaders(t *testing.T) {
	h := captureHeaders(t, &Client{APIKey: "test-key"})
	if got := h.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
	}
}
