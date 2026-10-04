package modelapi

import (
	"reflect"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	responses "github.com/majorcontext/harness/provider/openai"
)

func TestTelemetryOfACallWithNoPromptTokensHasNoReading(t *testing.T) {
	b := New(&responses.Client{Family: "codex"}, 0)
	if got := b.telemetry("codex/gpt-5", provider.Usage{OutputTokens: 3}, nil).Context; got != (eventlog.ContextMeasured{}) {
		t.Errorf("context reading = %+v, want none", got)
	}
}

func TestTelemetryWindowOfAModelModelmetaDoesNotKnow(t *testing.T) {
	b := New(&responses.Client{Family: "codex"}, 123_000)
	got := b.telemetry("codex/not-in-modelmeta", provider.Usage{InputTokens: 10}, nil).Context.Window
	if got != 123_000 {
		t.Errorf("context window = %d, want the configured 123000", got)
	}
}

func TestTelemetryCarriesTheSubscriptionSnapshotOfTheCall(t *testing.T) {
	b := New(&responses.Client{Family: "codex"}, 0)
	snap := &message.SubscriptionUsage{Provider: "codex", Plan: "plus", Windows: []message.SubscriptionUsageWindow{{Key: "primary", Label: "5-hour", UsedPercent: 12.5, ResetsAt: 9}},
		Overage: &message.SubscriptionOverage{InUse: true, Status: "allowed", ResetsAt: 11}}
	want := &eventlog.SubscriptionUsage{Provider: "codex", Plan: "plus", Windows: []eventlog.SubscriptionUsageWindow{{Key: "primary", Label: "5-hour", UsedPercent: 12.5, ResetsAt: 9}},
		Overage: &eventlog.SubscriptionOverage{InUse: true, Status: "allowed", ResetsAt: 11}}
	if got := b.telemetry("codex/gpt-5", provider.Usage{InputTokens: 1}, snap).SubscriptionUsage; !reflect.DeepEqual(got, want) {
		t.Errorf("subscription usage = %+v, want %+v", got, want)
	}
}
