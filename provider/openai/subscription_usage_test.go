package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// codexHeaders is the documented capture this feature is built against:
// the ChatGPT Codex backend's x-codex-* response headers on
// chatgpt.com/backend-api/codex/responses, plan windows plus an unused
// secondary window plus a bengalfox 5-hour+weekly pair.
func codexHeaders() http.Header {
	h := http.Header{}
	h.Set("x-codex-plan-type", "pro")
	h.Set("x-codex-primary-used-percent", "0")
	h.Set("x-codex-primary-window-minutes", "10080")
	h.Set("x-codex-primary-reset-at", "1788785267")
	h.Set("x-codex-secondary-used-percent", "0")
	h.Set("x-codex-secondary-window-minutes", "0")
	h.Set("x-codex-secondary-reset-at", "")
	h.Set("x-codex-bengalfox-primary-used-percent", "12.5")
	h.Set("x-codex-bengalfox-primary-window-minutes", "300")
	h.Set("x-codex-bengalfox-primary-reset-at", "1788700000")
	h.Set("x-codex-bengalfox-secondary-used-percent", "3")
	h.Set("x-codex-bengalfox-secondary-window-minutes", "10080")
	h.Set("x-codex-bengalfox-secondary-reset-at", "1789200000")
	return h
}

// TestCodexSubscriptionUsageFromHeaders proves the header->
// message.SubscriptionUsage mapping the documented capture demands:
// plan from x-codex-plan-type; a "primary" window labeled "Weekly" (10080
// minutes); a "bengalfox_primary" window labeled "5-hour" (300 minutes);
// the unused "secondary" window (window-minutes 0) dropped, not emitted as
// a hollow zero-value entry; no Overage (codex has none).
func TestCodexSubscriptionUsageFromHeaders(t *testing.T) {
	got := codexSubscriptionUsageFromHeaders(codexHeaders())
	if got == nil {
		t.Fatal("codexSubscriptionUsageFromHeaders = nil, want a captured snapshot")
	}
	if got.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", got.Provider)
	}
	if got.Plan != "pro" {
		t.Errorf("Plan = %q, want pro", got.Plan)
	}
	if got.Overage != nil {
		t.Errorf("Overage = %+v, want nil (codex has no overage concept)", got.Overage)
	}
	if len(got.Windows) != 2 {
		t.Fatalf("Windows = %+v, want 2 entries (secondary must be dropped)", got.Windows)
	}
	primary, bengal := got.Windows[0], got.Windows[1]
	if primary.Key != "primary" || primary.Label != "Weekly" || primary.UsedPercent != 0 || primary.ResetsAt != 1788785267 {
		t.Errorf("Windows[0] = %+v, want {primary Weekly 0 1788785267}", primary)
	}
	if bengal.Key != "bengalfox_primary" || bengal.Label != "5-hour" || bengal.UsedPercent != 12.5 || bengal.ResetsAt != 1788700000 {
		t.Errorf("Windows[1] = %+v, want {bengalfox_primary 5-hour 12.5 1788700000}", bengal)
	}
}

// TestCodexSubscriptionUsageFromHeadersNoSignal proves an ordinary,
// non-Codex response (no x-codex-* headers at all) maps to nil, not a
// hollow zero-value snapshot.
func TestCodexSubscriptionUsageFromHeadersNoSignal(t *testing.T) {
	if got := codexSubscriptionUsageFromHeaders(http.Header{}); got != nil {
		t.Errorf("codexSubscriptionUsageFromHeaders(no headers) = %+v, want nil", got)
	}
}

// codexStreamFixture is a minimal complete Responses SSE turn — enough to
// reach response.completed and queue EventDone, the only event this
// feature attaches SubscriptionUsage to.
var codexStreamFixture = sse("response.completed", `{"type":"response.completed","response":{"id":"resp_codex_1","usage":{"input_tokens":5,"output_tokens":2}}}`)

// TestStreamCapturesCodexSubscriptionUsageOverHTTP proves the HTTP+SSE path
// (Client.Stream, openai.go) reads x-codex-* response headers and attaches
// the mapped message.SubscriptionUsage to the turn's EventDone — but ONLY
// for a client configured under CodexFamily; an ordinary "openai"-family
// client talking to the exact same headers must not.
func TestStreamCapturesCodexSubscriptionUsageOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, vs := range codexHeaders() {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, codexStreamFixture) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	t.Run("codex family captures it", func(t *testing.T) {
		c := &Client{APIKey: "k", BaseURL: srv.URL, Family: CodexFamily}
		s, err := c.Stream(context.Background(), &provider.Request{
			Model:    message.ModelRef{Provider: CodexFamily, Model: "gpt-5"},
			Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		done := lastDoneEvent(t, s)
		if done.SubscriptionUsage == nil {
			t.Fatal("SubscriptionUsage = nil, want a captured snapshot")
		}
		if done.SubscriptionUsage.Provider != "codex" || done.SubscriptionUsage.Plan != "pro" {
			t.Errorf("SubscriptionUsage = %+v", done.SubscriptionUsage)
		}
	})

	t.Run("plain openai family does not capture it", func(t *testing.T) {
		c := &Client{APIKey: "k", BaseURL: srv.URL}
		s, err := c.Stream(context.Background(), &provider.Request{
			Model:    message.ModelRef{Provider: Family, Model: "gpt-5"},
			Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		done := lastDoneEvent(t, s)
		if done.SubscriptionUsage != nil {
			t.Errorf("SubscriptionUsage = %+v, want nil (not a codex-family client)", done.SubscriptionUsage)
		}
	})
}

// TestWebSocketTransportCapturesCodexSubscriptionUsage proves the websocket
// path (ws.go/ws_pool.go) reads the SAME x-codex-* headers off the upgrade
// RESPONSE (coder/websocket's own Dial return, not any frame on the wire)
// and attaches them to EventDone identically to the HTTP path above.
func TestWebSocketTransportCapturesCodexSubscriptionUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, vs := range codexHeaders() {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		for _, f := range wsCannedFrames {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(f)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := &Client{APIKey: "k", BaseURL: srv.URL, Family: CodexFamily, UseWebSocketTransport: true}
	s, err := c.Stream(context.Background(), wsRequest("sess-subusage"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	done := lastDoneEvent(t, s)
	if done.SubscriptionUsage == nil {
		t.Fatal("SubscriptionUsage = nil, want a captured snapshot from the upgrade response header")
	}
	if done.SubscriptionUsage.Provider != "codex" || done.SubscriptionUsage.Plan != "pro" {
		t.Errorf("SubscriptionUsage = %+v", done.SubscriptionUsage)
	}
	if len(done.SubscriptionUsage.Windows) != 2 {
		t.Errorf("Windows = %+v, want 2 entries", done.SubscriptionUsage.Windows)
	}
}

// lastDoneEvent drains s and returns its EventDone, failing the test if
// none arrived.
func lastDoneEvent(t *testing.T, s provider.Stream) *provider.Event {
	t.Helper()
	for _, ev := range collect(t, s) {
		if ev.Type == provider.EventDone {
			cp := ev
			return &cp
		}
	}
	t.Fatal("no EventDone")
	return nil
}
