package modelapi

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/provider"
	responses "github.com/majorcontext/harness/internal/provider/openai"
)

func TestTelemetryOfACallWithNoPromptTokensHasNoReading(t *testing.T) {
	b := New(&responses.Client{Family: "codex"}, 0, nil)
	if got := b.telemetry("codex/gpt-5", provider.Usage{OutputTokens: 3}, 0, nil).Context; got != (protocol.ContextMeasured{}) {
		t.Errorf("context reading = %+v, want none", got)
	}
}

func TestIncompleteResponseWithNoUsageNamesTheRequestSize(t *testing.T) {
	ev := provider.Event{StopReason: provider.StopMaxTokens, RequestMetadata: &provider.RequestMetadata{IncompleteReason: "max_output_tokens", RequestBytes: 1234}}
	err := incomplete(ev, false)
	if err == nil || !strings.Contains(err.Error(), "the backend returned no output and no usage; the request is likely too large for the backend (request_bytes=1234)") {
		t.Fatalf("error = %v, want the no-usage message", err)
	}
	if class, ok := provider.AsRetryable(err); !ok || class != provider.RetryableStreamTruncated {
		t.Errorf("class = %q, want stream_truncated", class)
	}
	ev.RequestMetadata.ProviderUsage = &provider.RawUsage{Input: 5, Output: 1}
	if err := incomplete(ev, false); err == nil || strings.Contains(err.Error(), "no usage") {
		t.Errorf("error = %v, want the plain cut message when the response carried usage", err)
	}
}
