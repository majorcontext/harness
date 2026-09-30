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

type brokerErrorProvider struct {
	streamError bool
}

func (brokerErrorProvider) Name() string { return "codex" }

func (p brokerErrorProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	if p.streamError {
		return brokerErrorStream{}, nil
	}
	return nil, errBrokerResolution
}

type brokerErrorStream struct{}

func (brokerErrorStream) Next() (provider.Event, error) { return provider.Event{}, errBrokerResolution }
func (brokerErrorStream) Close() error                  { return nil }

func TestAuxiliarySubscriptionErrorsNameOnlyExplicitSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation string
		streamErr bool
		selected  bool
	}{
		{name: "compact dial explicit", operation: "compact", selected: true},
		{name: "compact stream explicit", operation: "compact", streamErr: true, selected: true},
		{name: "evaluator dial explicit", operation: "evaluator", selected: true},
		{name: "evaluator stream explicit", operation: "evaluator", streamErr: true, selected: true},
		{name: "compact dial default", operation: "compact"},
		{name: "evaluator stream default", operation: "evaluator", streamErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := message.ModelRef{Provider: "codex", Model: "gpt-6.1-sol"}
			prov := brokerErrorProvider{streamError: tc.streamErr}
			s := NewSession(Config{
				Providers: provider.Registry{"codex": prov},
				Model:     model,
				AccountRouting: map[string]AccountRoutingConfig{
					"codex": {Vendor: "codex", ProxyURL: "http://subject%7Cbox:password@proxy.example", Protocol: "boxes-v1"},
				},
			})
			if tc.selected {
				s.accountSelection = map[string]string{"codex": "acct_aux"}
			}
			var err error
			if tc.operation == "compact" {
				_, _, err = s.runCompactionSummary(context.Background(), model, nil)
			} else {
				_, err = s.runEvaluator(context.Background(), "condition", model, "system")
			}
			if !errors.Is(err, errBrokerResolution) {
				t.Fatalf("auxiliary error = %v, want broker failure", err)
			}
			if tc.selected && !strings.Contains(err.Error(), `subscription account "acct_aux": `) {
				t.Fatalf("selected-account auxiliary error = %v", err)
			}
			if !tc.selected && err.Error() != errBrokerResolution.Error() {
				t.Fatalf("default-account auxiliary error = %v, want unchanged", err)
			}
		})
	}
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
