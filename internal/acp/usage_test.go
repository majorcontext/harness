package acp

import (
	"testing"

	"github.com/majorcontext/harness/protocol"
)

func TestUsageBodyCarriesTheCumulativeCostInUSD(t *testing.T) {
	cost := 0.045
	s := protocol.Session{Context: protocol.Context{Tokens: 53000, Window: 200000}, SubscriptionUsage: &protocol.SubscriptionUsage{SessionCostUSD: &cost}}
	body := usageBody(usageOf(s))
	if body["used"] != int64(53000) || body["size"] != int64(200000) {
		t.Errorf("body = %v, want used 53000 and size 200000", body)
	}
	got, _ := body["cost"].(map[string]any)
	if got["amount"] != 0.045 || got["currency"] != "USD" {
		t.Errorf("cost = %v, want 0.045 USD", body["cost"])
	}
}

func TestUsageBodyOmitsCostWhenTheSessionReportsNone(t *testing.T) {
	for _, s := range []protocol.Session{
		{Context: protocol.Context{Tokens: 1, Window: 2}},
		{Context: protocol.Context{Tokens: 1, Window: 2}, SubscriptionUsage: &protocol.SubscriptionUsage{}},
	} {
		if _, ok := usageBody(usageOf(s))["cost"]; ok {
			t.Errorf("session %+v: body holds a cost, want none", s)
		}
	}
}
