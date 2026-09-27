package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

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

// TestCodexSubscriptionUsageFromRateLimitsEvent proves the event -> message.SubscriptionUsage mapping, reusing codexWindowLabel.
func TestCodexSubscriptionUsageFromRateLimitsEvent(t *testing.T) {
	w := func(k, l string, u float64, r int64) message.SubscriptionUsageWindow {
		return message.SubscriptionUsageWindow{Key: k, Label: l, UsedPercent: u, ResetsAt: r}
	}
	cases := []struct {
		name, event string
		want        *message.SubscriptionUsage
	}{
		{"full snapshot, unknown fields ignored",
			`{"type":"codex.rate_limits","plan_type":"team","rate_limits":{"primary":{"used_percent":36,"window_minutes":300,"reset_at":1790482956},"secondary":{"used_percent":21,"window_minutes":10080,"reset_at":1791037689}},"credits":{"has_credits":false,"unlimited":false,"balance":null},"metered_limit_name":"codex","limit_name":null}`,
			&message.SubscriptionUsage{Provider: "codex", Plan: "team", Windows: []message.SubscriptionUsageWindow{w("primary", "5-hour", 36, 1790482956), w("secondary", "Weekly", 21, 1791037689)}}},
		{"null secondary window dropped",
			`{"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"primary":{"used_percent":5,"window_minutes":300,"reset_at":111}}}`,
			&message.SubscriptionUsage{Provider: "codex", Plan: "pro", Windows: []message.SubscriptionUsageWindow{w("primary", "5-hour", 5, 111)}}},
		{"missing window_minutes and reset_at",
			`{"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"primary":{"used_percent":5}}}`,
			&message.SubscriptionUsage{Provider: "codex", Plan: "pro", Windows: []message.SubscriptionUsageWindow{w("primary", "", 5, 0)}}},
		{"no plan and no rate_limits maps to nil", `{"type":"codex.rate_limits"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codexSubscriptionUsageFromRateLimitsEvent([]byte(tc.event))
			if err != nil {
				t.Fatalf("codexSubscriptionUsageFromRateLimitsEvent: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("codexSubscriptionUsageFromRateLimitsEvent() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// newRateLimitsWSServer replays next's used_percent per response.create.
func newRateLimitsWSServer(t *testing.T, next func() (usedPercent float64, ok bool)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			_, _, err := conn.Read(ctx)
			cancel()
			if err != nil {
				return
			}
			used, ok := next()
			if !ok {
				return
			}
			frames := []string{
				fmt.Sprintf(`{"type":"codex.rate_limits","plan_type":"team","rate_limits":{"primary":{"used_percent":%v,"window_minutes":300,"reset_at":111}}}`, used),
				`{"type":"response.created","response":{"id":"resp_ws_1"}}`,
				`{"type":"response.completed","response":{"id":"resp_ws_1","usage":{"input_tokens":5,"output_tokens":2}}}`,
			}
			for _, f := range frames {
				if err := conn.Write(context.Background(), websocket.MessageText, []byte(f)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func streamDone(t *testing.T, c *Client, req *provider.Request) *provider.Event {
	t.Helper()
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	return lastDoneEvent(t, s)
}

// TestWebSocketStreamReportsRateLimitsPerTurn proves each turn reports its
// own codex.rate_limits event, not one frozen from the pooled dial.
func TestWebSocketStreamReportsRateLimitsPerTurn(t *testing.T) {
	percents := []float64{10, 90}
	srv := newRateLimitsWSServer(t, func() (float64, bool) {
		if len(percents) == 0 {
			return 0, false
		}
		p := percents[0]
		percents = percents[1:]
		return p, true
	})
	c := &Client{APIKey: "k", BaseURL: srv.URL, Family: CodexFamily, UseWebSocketTransport: true}
	req := wsRequest("sess-ratelimits-per-turn")
	if got := streamDone(t, c, req).SubscriptionUsage; got == nil || got.Windows[0].UsedPercent != 10 {
		t.Fatalf("turn 1 SubscriptionUsage = %+v, want a 10%% primary window", got)
	}
	if got := streamDone(t, c, req).SubscriptionUsage; got == nil || got.Windows[0].UsedPercent != 90 {
		t.Fatalf("turn 2 SubscriptionUsage = %+v, want a 90%% primary window (not turn 1's frozen 10%%)", got)
	}
}

// TestWebSocketStreamIgnoresRateLimitsForNonCodexFamily proves a non-Codex-
// family ws client never attaches a codex.rate_limits event to EventDone.
func TestWebSocketStreamIgnoresRateLimitsForNonCodexFamily(t *testing.T) {
	srv := newRateLimitsWSServer(t, func() (float64, bool) { return 50, true })
	c := &Client{APIKey: "k", BaseURL: srv.URL, UseWebSocketTransport: true}
	if got := streamDone(t, c, wsRequest("sess-ratelimits-non-codex")).SubscriptionUsage; got != nil {
		t.Errorf("SubscriptionUsage = %+v, want nil (not a codex-family client)", got)
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

// TestWebSocketStreamSurvivesMalformedRateLimits: usage reporting is
// cosmetic, so a rate-limits frame this package cannot parse must leave the
// turn intact rather than failing it — the same permissive posture
// codexHeaderFloat documents for the header lane. Failing here would also
// bypass recoverChainMiss, which only runs for a previousResponseNotFound.
func TestWebSocketStreamSurvivesMalformedRateLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wsProtocolHeader}})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		frames := []string{
			`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":"not-a-number"}}}`,
			`{"type":"response.created","response":{"id":"resp_bad_rl"}}`,
			`{"type":"response.completed","response":{"id":"resp_bad_rl","usage":{"input_tokens":5,"output_tokens":2}}}`,
		}
		for _, f := range frames {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(f)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := &Client{APIKey: "k", BaseURL: srv.URL, Family: CodexFamily, UseWebSocketTransport: true}
	s, err := c.Stream(context.Background(), wsRequest("sess-bad-ratelimits"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	done := lastDoneEvent(t, s)
	if done == nil {
		t.Fatal("no done event: a malformed rate-limits frame must not fail the turn")
	}
	if done.SubscriptionUsage != nil {
		t.Errorf("SubscriptionUsage = %+v, want nil for an unparseable frame", done.SubscriptionUsage)
	}
}
