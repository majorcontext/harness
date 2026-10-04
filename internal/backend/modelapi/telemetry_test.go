package modelapi

import (
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/provider"
	responses "github.com/majorcontext/harness/provider/openai"
)

func TestTelemetryOfACallWithNoPromptTokensHasNoReading(t *testing.T) {
	b := New(&responses.Client{Family: "codex"}, 0)
	if got := b.telemetry("codex/gpt-5", provider.Usage{OutputTokens: 3}).Context; got != (eventlog.ContextMeasured{}) {
		t.Errorf("context reading = %+v, want none", got)
	}
}

func TestTelemetryWindowOfAModelModelmetaDoesNotKnow(t *testing.T) {
	b := New(&responses.Client{Family: "codex"}, 123_000)
	got := b.telemetry("codex/not-in-modelmeta", provider.Usage{InputTokens: 10}).Context.Window
	if got != 123_000 {
		t.Errorf("context window = %d, want the configured 123000", got)
	}
}
