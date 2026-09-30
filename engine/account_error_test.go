package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

var errBrokerResolution = errors.New("credential resolution failed")

type brokerErrorProvider struct{}

func (brokerErrorProvider) Name() string { return "codex" }

func (brokerErrorProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	return nil, errBrokerResolution
}

func TestSubscriptionAccountErrorsNameOnlyExplicitSelection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selection  map[string]string
		wantPrefix string
	}{
		{name: "explicit", selection: map[string]string{"codex": "acct_child"}, wantPrefix: `subscription account "acct_child": `},
		{name: "box default", selection: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession(Config{
				Providers: provider.Registry{"codex": brokerErrorProvider{}},
				Model:     message.ModelRef{Provider: "codex", Model: "gpt-6.1-sol"},
				AccountRouting: map[string]AccountRoutingConfig{
					"codex": {Vendor: "codex", ProxyURL: "http://subject%7Cbox:password@proxy.example", Protocol: "boxes-v1"},
				},
			})
			s.accountSelection = tc.selection
			_, err := s.Prompt(context.Background(), "go")
			if !errors.Is(err, errBrokerResolution) {
				t.Fatalf("prompt error = %v, want broker failure", err)
			}
			if tc.wantPrefix != "" && !strings.Contains(err.Error(), tc.wantPrefix) {
				t.Fatalf("explicit-account error = %v, want prefix %q", err, tc.wantPrefix)
			}
			if tc.wantPrefix == "" && err.Error() != errBrokerResolution.Error() {
				t.Fatalf("box-default error = %v, want unchanged %q", err, errBrokerResolution)
			}
		})
	}
}
