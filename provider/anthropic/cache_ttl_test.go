package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// cacheTTLRequest transcodes one minimal request at the given TTL.
func cacheTTLRequest(t *testing.T, ttl string) *apiRequest {
	t.Helper()
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	out, err := transcodeRequest(req, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestCacheTTLDefaultIsOneHour: the resolved default TTL is the extended
// 1-hour cache. A 5-minute breakpoint expires between turns of an ordinary
// agentic session, and each expiry rewrites the whole prefix.
func TestCacheTTLDefaultIsOneHour(t *testing.T) {
	got, err := resolveCacheTTL("")
	if err != nil {
		t.Fatal(err)
	}
	if got != CacheTTL1h {
		t.Errorf("resolveCacheTTL(%q) = %q, want %q", "", got, CacheTTL1h)
	}
}

// TestResolveCacheTTLRejectsUnknown: an unknown TTL fails loudly instead of
// falling back, so a typo never silently ships the wrong cache economics.
func TestResolveCacheTTLRejectsUnknown(t *testing.T) {
	if _, err := resolveCacheTTL("30m"); err == nil {
		t.Fatal("resolveCacheTTL(\"30m\") = nil error, want error")
	}
}

// TestCacheTTLOneHourOnEveryBreakpoint: at the 1h TTL every cache_control
// breakpoint — the final system block and the final content block — carries
// {"type":"ephemeral","ttl":"1h"}.
func TestCacheTTLOneHourOnEveryBreakpoint(t *testing.T) {
	out := cacheTTLRequest(t, CacheTTL1h)

	sys := out.System[len(out.System)-1].CacheControl
	if sys == nil {
		t.Fatal("missing cache_control on final system block")
	}
	if sys.Type != "ephemeral" || sys.TTL != "1h" {
		t.Errorf("system cache_control = %+v, want {ephemeral 1h}", *sys)
	}
	last := out.Messages[len(out.Messages)-1]
	msg := last.Content[len(last.Content)-1].CacheControl
	if msg == nil {
		t.Fatal("missing cache_control on final content block")
	}
	if msg.Type != "ephemeral" || msg.TTL != "1h" {
		t.Errorf("message cache_control = %+v, want {ephemeral 1h}", *msg)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), `"cache_control":{"type":"ephemeral","ttl":"1h"}`); n != 2 {
		t.Errorf("wire has %d 1h cache_control blocks, want 2: %s", n, raw)
	}
}

// TestCacheTTLFiveMinutesOmitsTTLField: the 5m TTL is the API default, so the
// wire omits the ttl key entirely — byte-identical to the pre-TTL request.
func TestCacheTTLFiveMinutesOmitsTTLField(t *testing.T) {
	out := cacheTTLRequest(t, CacheTTL5m)

	if cc := out.System[len(out.System)-1].CacheControl; cc == nil || cc.TTL != "" {
		t.Errorf("system cache_control = %+v, want no ttl", cc)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"ttl"`) {
		t.Errorf("wire must omit ttl at 5m: %s", raw)
	}
	if n := strings.Count(string(raw), `"cache_control":{"type":"ephemeral"}`); n != 2 {
		t.Errorf("wire has %d plain cache_control blocks, want 2: %s", n, raw)
	}
}

// captureHeaders runs one Stream call against a stub server and returns the
// request headers it saw.
func captureHeaders(t *testing.T, c *Client) http.Header {
	t.Helper()
	seen := make(chan http.Header, 1)
	srv := newHeaderServer(t, seen)
	c.BaseURL = srv
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return <-seen
}

// newHeaderServer serves the shared stream fixture and publishes the headers
// of the request it received.
func newHeaderServer(t *testing.T, seen chan<- http.Header) string {
	t.Helper()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamFixture))
	})
	return c.BaseURL
}

// TestOneHourTTLSendsBetaHeader: the extended (1h) cache TTL is gated behind
// the extended-cache-ttl-2025-04-11 beta, so the adapter must send the
// anthropic-beta header with it.
func TestOneHourTTLSendsBetaHeader(t *testing.T) {
	h := captureHeaders(t, &Client{APIKey: "test-key", CacheTTL: CacheTTL1h})
	if got := h.Get("Anthropic-Beta"); got != extendedCacheTTLBeta {
		t.Errorf("Anthropic-Beta = %q, want %q", got, extendedCacheTTLBeta)
	}
}

// TestDefaultClientSendsBetaHeader: an unset Client.CacheTTL defaults to 1h,
// so the beta header rides the default configuration too.
func TestDefaultClientSendsBetaHeader(t *testing.T) {
	h := captureHeaders(t, &Client{APIKey: "test-key"})
	if got := h.Get("Anthropic-Beta"); got != extendedCacheTTLBeta {
		t.Errorf("Anthropic-Beta = %q, want %q", got, extendedCacheTTLBeta)
	}
}

// TestFiveMinuteTTLOmitsBetaHeader: the 5m TTL needs no beta, so the header
// is absent — a gateway that rejects unknown betas keeps working at 5m.
func TestFiveMinuteTTLOmitsBetaHeader(t *testing.T) {
	h := captureHeaders(t, &Client{APIKey: "test-key", CacheTTL: CacheTTL5m})
	if got := h.Get("Anthropic-Beta"); got != "" {
		t.Errorf("Anthropic-Beta = %q, want absent", got)
	}
}

// TestInvalidClientCacheTTLFailsStream: a Client built with an unknown TTL
// fails on send with a named error, like a missing API key.
func TestInvalidClientCacheTTLFailsStream(t *testing.T) {
	c := &Client{APIKey: "test-key", CacheTTL: "30m"}
	_, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "claude-fable-5"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("Stream with cache_ttl 30m = nil error, want error")
	}
	if !strings.Contains(err.Error(), "cache_ttl") {
		t.Errorf("error %q does not name cache_ttl", err)
	}
}
