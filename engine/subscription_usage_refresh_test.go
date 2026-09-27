package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// subscriptionUsageRefresherProvider extends scriptedProvider with an
// on-demand provider.SubscriptionUsageRefresher implementation, so a test
// can control exactly what a refresh call returns without a real HTTP
// round trip.
type subscriptionUsageRefresherProvider struct {
	scriptedProvider
	usage *message.SubscriptionUsage
	err   error
	calls int
}

func (p *subscriptionUsageRefresherProvider) RefreshSubscriptionUsage(_ context.Context) (*message.SubscriptionUsage, error) {
	p.calls++
	return p.usage, p.err
}

// TestRefreshSubscriptionUsageAppliesResult: a provider implementing
// provider.SubscriptionUsageRefresher returns a fresh snapshot, and
// Session.RefreshSubscriptionUsage applies it through applySubscriptionUsage
// (the same choke point a turn-side capture uses), stamping CapturedAt from
// the session's own clock.
func TestRefreshSubscriptionUsageAppliesResult(t *testing.T) {
	prov := &subscriptionUsageRefresherProvider{
		scriptedProvider: scriptedProvider{name: "test"},
		usage: &message.SubscriptionUsage{
			Provider: "codex",
			Plan:     "pro",
			Windows:  []message.SubscriptionUsageWindow{{Key: "primary", Label: "Weekly", UsedPercent: 10}},
		},
	}
	fixedNow := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		Now:       func() time.Time { return fixedNow },
	}
	s := NewSession(cfg)

	got, err := s.RefreshSubscriptionUsage(context.Background())
	if err != nil {
		t.Fatalf("RefreshSubscriptionUsage: %v", err)
	}
	if prov.calls != 1 {
		t.Fatalf("provider refresh calls = %d, want 1", prov.calls)
	}
	if got == nil || got.Plan != "pro" || len(got.Windows) != 1 {
		t.Fatalf("got = %+v, want a captured pro-plan snapshot", got)
	}
	if got.CapturedAt != fixedNow.Unix() {
		t.Errorf("CapturedAt = %d, want %d (the session's own clock)", got.CapturedAt, fixedNow.Unix())
	}
	if cur := s.SubscriptionUsage(); cur == nil || cur.Plan != "pro" {
		t.Errorf("SubscriptionUsage() after refresh = %+v, want the same pro-plan snapshot", cur)
	}
}

// TestRefreshSubscriptionUsageUnsupportedProviderLeavesCacheUntouched: a
// provider with no provider.SubscriptionUsageRefresher implementation (the
// ordinary scriptedProvider) reports
// provider.ErrSubscriptionUsageRefreshUnsupported and leaves the session's
// cached snapshot exactly as it was — nil here, since no turn has run.
func TestRefreshSubscriptionUsageUnsupportedProviderLeavesCacheUntouched(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	cfg := Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	}
	s := NewSession(cfg)

	_, err := s.RefreshSubscriptionUsage(context.Background())
	if !errors.Is(err, provider.ErrSubscriptionUsageRefreshUnsupported) {
		t.Fatalf("err = %v, want provider.ErrSubscriptionUsageRefreshUnsupported", err)
	}
	if got := s.SubscriptionUsage(); got != nil {
		t.Errorf("SubscriptionUsage() = %+v, want nil (untouched)", got)
	}
}

// TestRefreshSubscriptionUsagePropagatesFetchErrorWithoutClobberingCache: a
// refresher's own fetch failure propagates verbatim and must not overwrite
// a snapshot a prior successful refresh already cached — a failed refresh
// must never clobber a known-good reading.
func TestRefreshSubscriptionUsagePropagatesFetchErrorWithoutClobberingCache(t *testing.T) {
	good := &message.SubscriptionUsage{Provider: "codex", Plan: "pro", Windows: []message.SubscriptionUsageWindow{}}
	prov := &subscriptionUsageRefresherProvider{scriptedProvider: scriptedProvider{name: "test"}, usage: good}
	cfg := Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	}
	s := NewSession(cfg)
	if _, err := s.RefreshSubscriptionUsage(context.Background()); err != nil {
		t.Fatalf("seeding refresh: %v", err)
	}

	fetchErr := errors.New("upstream unavailable")
	prov.err = fetchErr
	prov.usage = nil
	_, err := s.RefreshSubscriptionUsage(context.Background())
	if !errors.Is(err, fetchErr) {
		t.Fatalf("err = %v, want %v", err, fetchErr)
	}
	if got := s.SubscriptionUsage(); got == nil || got.Plan != "pro" {
		t.Errorf("SubscriptionUsage() after failed refresh = %+v, want the prior pro-plan snapshot intact", got)
	}
}
