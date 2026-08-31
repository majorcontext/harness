package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// withSubscriptionUsageTurn is withUsageTurn's (usage_test.go) sibling: a
// completed turn whose EventDone also carries a captured
// message.SubscriptionUsage — the shape provider/openai's codex family
// attaches (see provider/openai/subscription_usage_test.go for the header-
// capture half; this exercises only the engine->server wiring GET /session
// reads).
func withSubscriptionUsageTurn(text string, usage message.SubscriptionUsage) []provider.Event {
	msg := &message.Message{ID: message.ProviderCallID("m", text, 12), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}}
	return []provider.Event{{
		Type:              provider.EventDone,
		Message:           msg,
		StopReason:        provider.StopEndTurn,
		Usage:             provider.Usage{InputTokens: 5, OutputTokens: 2},
		SubscriptionUsage: &usage,
	}}
}

// TestSessionSubscriptionUsageSurfacedOnGet proves GET /session/{id}
// carries subscription_usage: null before any turn has carried the
// signal, then the mapped snapshot once one has — the exact field
// buildSession (handlers.go) reads from engine.Session.SubscriptionUsage.
func TestSessionSubscriptionUsageSurfacedOnGet(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		{{Type: provider.EventDone, Message: &message.Message{ID: message.ProviderCallID("m", "no signal", 12), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "no signal"}}}, StopReason: provider.StopEndTurn}},
		withSubscriptionUsageTurn("here's your usage", message.SubscriptionUsage{
			Provider: "codex",
			Plan:     "pro",
			Windows: []message.SubscriptionUsageWindow{
				{Key: "primary", Label: "Weekly", UsedPercent: 12.5, ResetsAt: 1788785267},
			},
		}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")

	// Turn 1: no SubscriptionUsage on this turn's EventDone at all —
	// subscription_usage must stay null (not, say, a zero-value object).
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	sse.collectUntilIdle(t)

	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var sess sessionJSONForTest
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.SubscriptionUsage != nil {
		t.Fatalf("subscription_usage before any turn carried the signal = %+v, want null", sess.SubscriptionUsage)
	}

	// Turn 2: this turn's EventDone carries a captured snapshot.
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "again"}},
	})
	sse.collectUntilIdle(t)

	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.SubscriptionUsage == nil {
		t.Fatal("subscription_usage after a turn carried the signal = null, want the captured snapshot")
	}
	su := sess.SubscriptionUsage
	if su.Provider != "codex" || su.Plan != "pro" {
		t.Errorf("subscription_usage = %+v, want provider=codex plan=pro", su)
	}
	if len(su.Windows) != 1 || su.Windows[0].Key != "primary" || su.Windows[0].Label != "Weekly" ||
		su.Windows[0].UsedPercent != 12.5 || su.Windows[0].ResetsAt != 1788785267 {
		t.Errorf("subscription_usage.windows = %+v", su.Windows)
	}
	if su.CapturedAt == 0 {
		t.Error("subscription_usage.captured_at = 0, want a stamped Unix timestamp")
	}
	if su.Overage != nil {
		t.Errorf("subscription_usage.overage = %+v, want nil (this turn's usage carried none)", su.Overage)
	}
}
