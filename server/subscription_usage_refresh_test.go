package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// subscriptionUsageRefresherProvider extends scriptedProvider with an
// on-demand provider.SubscriptionUsageRefresher implementation, so a route
// test can control exactly what a refresh call returns without a real
// HTTP round trip to a vendor.
type subscriptionUsageRefresherProvider struct {
	scriptedProvider
	usage *message.SubscriptionUsage
	err   error
}

func (p *subscriptionUsageRefresherProvider) RefreshSubscriptionUsage(_ context.Context) (*message.SubscriptionUsage, error) {
	return p.usage, p.err
}

// TestSubscriptionUsageRefreshEndpointReturnsFreshSnapshot drives the real
// POST /session/{id}/subscription-usage/refresh route against a provider
// that supports it: 200, supported=true, and the fresh snapshot readable
// straight off the response, matching GET /session's own subscription_usage
// field afterward.
func TestSubscriptionUsageRefreshEndpointReturnsFreshSnapshot(t *testing.T) {
	prov := &subscriptionUsageRefresherProvider{
		scriptedProvider: scriptedProvider{name: "test"},
		usage:            &message.SubscriptionUsage{Provider: "codex", Plan: "pro", Windows: []message.SubscriptionUsageWindow{}},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/subscription-usage/refresh", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	var got subscriptionUsageRefreshResponseJSON
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Supported {
		t.Fatal("supported = false, want true (this provider implements the capability)")
	}
	if got.SubscriptionUsage == nil || got.SubscriptionUsage.Plan != "pro" {
		t.Fatalf("subscription_usage = %+v, want a pro-plan snapshot", got.SubscriptionUsage)
	}

	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		SubscriptionUsage *message.SubscriptionUsage `json:"subscription_usage"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.SubscriptionUsage == nil || sess.SubscriptionUsage.Plan != "pro" {
		t.Fatalf("GET /session subscription_usage = %+v, want the refreshed pro-plan snapshot", sess.SubscriptionUsage)
	}
}

// TestSubscriptionUsageRefreshEndpointUnsupportedLane: a provider with no
// provider.SubscriptionUsageRefresher implementation (the ordinary
// scriptedProvider) answers 200 with supported=false — a documented
// outcome, never an error status a caller could mistake for a failure.
func TestSubscriptionUsageRefreshEndpointUnsupportedLane(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/subscription-usage/refresh", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s, want 200 (unsupported is not a failure)", resp.StatusCode, data)
	}
	var got subscriptionUsageRefreshResponseJSON
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Supported {
		t.Fatal("supported = true, want false (this provider has no refresh capability)")
	}
}

// TestSubscriptionUsageRefreshEndpointUpstreamFailure: a refresher's own
// fetch error is a real failure (502), distinct from the unsupported-lane
// 200 above.
func TestSubscriptionUsageRefreshEndpointUpstreamFailure(t *testing.T) {
	prov := &subscriptionUsageRefresherProvider{
		scriptedProvider: scriptedProvider{name: "test"},
		err:              errors.New("upstream unavailable"),
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/subscription-usage/refresh", nil)
	if resp.StatusCode != 502 {
		t.Fatalf("status %d: %s, want 502 (a real fetch failure)", resp.StatusCode, data)
	}
}

// TestSubscriptionUsageRefreshEndpointUnknownSession: an unknown session is
// 404, mirroring every other session subresource route.
func TestSubscriptionUsageRefreshEndpointUnknownSession(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session/ses_01000000000000000000000000/subscription-usage/refresh", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown-session status %d: %s, want 404", resp.StatusCode, data)
	}
}

var _ provider.SubscriptionUsageRefresher = (*subscriptionUsageRefresherProvider)(nil)
