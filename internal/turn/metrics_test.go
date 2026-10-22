package turn

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

type telemetryBackend struct{}

func (telemetryBackend) Capabilities(string) Capabilities { return Capabilities{} }

func (telemetryBackend) Run(_ context.Context, _ Request, out Sink) error {
	out.Telemetry(Telemetry{Call: &CallMetrics{}})
	return out.Item(protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: "ok"}}})
}

func TestAskLogsCallPurpose(t *testing.T) {
	var output bytes.Buffer
	_, _, err := Ask(context.Background(), telemetryBackend{}, Request{Purpose: "judge"}, Limits{Log: slog.New(slog.NewTextHandler(&output, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "purpose=judge") {
		t.Fatalf("turn_metrics = %q, missing purpose", output.String())
	}
}
