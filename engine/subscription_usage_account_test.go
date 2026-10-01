package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

type accountRouteCaptureProvider struct {
	route provider.AccountRouting
}

func (p *accountRouteCaptureProvider) Name() string { return "codex" }

func (p *accountRouteCaptureProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	p.route, _ = provider.AccountRoutingFromContext(ctx)
	return &scriptedStream{events: []provider.Event{{
		Type:              provider.EventDone,
		Message:           &message.Message{ID: "account_usage", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}},
		SubscriptionUsage: &message.SubscriptionUsage{Provider: "codex", Windows: []message.SubscriptionUsageWindow{}},
	}}}, nil
}

func TestPromptRoutesAndCapturesExplicitAccountAtUsageEvent(t *testing.T) {
	prov := &accountRouteCaptureProvider{}
	s := NewSession(Config{
		Providers: provider.Registry{"codex": prov},
		Model:     message.ModelRef{Provider: "codex", Model: "gpt-6.1-sol"},
		AccountRouting: map[string]AccountRoutingConfig{
			"codex": {Vendor: "codex", ProxyURL: "http://subject%7Cbox:password@proxy.example", Protocol: "boxes-v1"},
		},
	})
	s.accountSelection = map[string]string{"codex": ""}
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(prov.route.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.User.Username(), "|accounts-v1=") {
		t.Fatalf("proxy username did not carry selection: %q", u.User.Username())
	}
	usage := s.SubscriptionUsage()
	if usage == nil || usage.AccountID == nil || *usage.AccountID != "" {
		t.Fatalf("captured account_id = %+v, want explicitly present empty ID", usage)
	}
	wire, err := json.Marshal(usage)
	if err != nil || !strings.Contains(string(wire), `"account_id":""`) {
		t.Fatalf("usage DTO does not preserve account_id: %s (%v)", wire, err)
	}
	*usage.AccountID = "acct_mutated"
	s.mu.Lock()
	s.accountSelection["codex"] = "acct_new"
	s.mu.Unlock()
	usage = s.SubscriptionUsage()
	if usage.AccountID == nil || *usage.AccountID != "" {
		t.Fatalf("usage account_id changed after capture: %v", usage.AccountID)
	}
	costOnly := NewSession(Config{})
	costOnly.accountSelection = map[string]string{"claude": "acct_cost"}
	costOnly.applyClaudeCodeUsage(provider.Usage{}, provider.Usage{}, 0, 0)
	costUsage := costOnly.SubscriptionUsage()
	if costUsage.AccountID == nil || *costUsage.AccountID != "acct_cost" {
		t.Fatalf("cost-only Claude account_id = %v, want acct_cost", costUsage.AccountID)
	}
}

func TestAPIProviderPromptRemainsUsableWithOtherConfiguredAccountRoute(t *testing.T) {
	apiProvider := &scriptedProvider{name: "openrouter", turns: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "done"})}}
	s := NewSession(Config{
		Providers: provider.Registry{"openrouter": apiProvider},
		Model:     message.ModelRef{Provider: "openrouter", Model: "model"},
		AccountRouting: map[string]AccountRoutingConfig{
			"codex": {Vendor: "codex", ProxyURL: "http://subject%7Cbox:password@proxy.example", Protocol: "boxes-v1"},
		},
	})
	s.accountSelection = map[string]string{"codex": "acct_a"}
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if apiProvider.call != 1 {
		t.Fatalf("API provider calls = %d, want 1", apiProvider.call)
	}
}
