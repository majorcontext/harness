package openai

import (
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/provider"
)

func TestTelemetryOfACallWithNoPromptTokensHasNoReading(t *testing.T) {
	b := New("codex", config.Provider{}, nil)
	if got := b.telemetry("codex/gpt-5", provider.Usage{OutputTokens: 3}).Context; got != (eventlog.ContextMeasured{}) {
		t.Errorf("context reading = %+v, want none", got)
	}
}
