package anthropic

import "testing"

// This adapter is the only path to an Anthropic-shaped upstream, so without
// ExtraHeaders that traffic reaches a gateway carrying no identity and its
// spend cannot be attributed to anyone.
func TestExtraHeadersReachTheUpstream(t *testing.T) {
	h := captureHeaders(t, &Client{
		APIKey:       "test-key",
		ExtraHeaders: map[string]string{"X-Neptune-User": "someone@example.com"},
	})
	if got := h.Get("X-Neptune-User"); got != "someone@example.com" {
		t.Errorf("X-Neptune-User = %q, want %q", got, "someone@example.com")
	}
}

// The fixed headers are the request's authentication and protocol contract,
// so config must not be able to displace them. The extras are applied first
// and overwritten, never the reverse.
func TestExtraHeadersCannotOverrideFixedHeaders(t *testing.T) {
	h := captureHeaders(t, &Client{
		APIKey: "test-key",
		ExtraHeaders: map[string]string{
			"X-Api-Key":         "stolen",
			"Anthropic-Version": "1999-01-01",
			"Content-Type":      "text/plain",
		},
	})
	for _, tc := range []struct{ name, want string }{
		{"X-Api-Key", "test-key"},
		{"Anthropic-Version", apiVersion},
		{"Content-Type", "application/json"},
	} {
		if got := h.Get(tc.name); got != tc.want {
			t.Errorf("%s = %q, want %q (config must not override it)", tc.name, got, tc.want)
		}
	}
}

// With no extras configured the fixed headers must still be intact, so the
// new loop cannot disturb a request that sets none.
func TestNoExtraHeadersKeepsFixedHeaders(t *testing.T) {
	h := captureHeaders(t, &Client{APIKey: "test-key"})
	if got := h.Get("X-Api-Key"); got != "test-key" {
		t.Errorf("X-Api-Key = %q, want %q", got, "test-key")
	}
	if got := h.Get("Anthropic-Version"); got != apiVersion {
		t.Errorf("Anthropic-Version = %q, want %q", got, apiVersion)
	}
}
