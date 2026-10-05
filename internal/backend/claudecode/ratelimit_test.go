package claudecode_test

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
)

func TestClaudeCodeRecordsOneMeasurementForTheCallThatTheRateLimitEventPrecedes(t *testing.T) {
	fakeClaude(t, "rate_limit_event")
	st := harness.NewMemStore()
	r := claudeRuntimeWith(t, st, nil, false, nil)
	defer closeRuntime(t, r)
	s := createClaude(t, r, nil)
	turnOf(t, s, text("a", "hi"))
	recs, err := st.Read(bg, "s1", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var measured []eventlog.ContextMeasured
	for _, rec := range recs {
		var env struct {
			K string
			D eventlog.ContextMeasured
		}
		if json.Unmarshal(rec.Data, &env) == nil && env.K == "context.measured" {
			measured = append(measured, env.D)
		}
	}
	if len(measured) != 1 {
		t.Fatalf("context.measured records = %d, want 1 for the one call", len(measured))
	}
	if m := measured[0]; m.Usage == (eventlog.Usage{}) || m.SubscriptionUsage == nil {
		t.Errorf("record = %+v, want the usage and the subscription snapshot together", m)
	}
}
