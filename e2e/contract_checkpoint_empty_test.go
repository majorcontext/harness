package e2e

import (
	"reflect"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

func checkCheckpointEmptyValues(t *testing.T, base harness.Store, id string) {
	t.Helper()
	ctx := t.Context()
	head, err := base.Head(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	log := snapshotScenarioLog(t, base, id, head)
	allowed := []string{}
	accounts := map[string]string{}
	zero := 0.0
	payloads := []protocol.Payload{
		protocol.SettingsChanged{AllowedTools: &allowed, Accounts: &accounts},
		protocol.ContextMeasured{CostUSD: &zero, SubscriptionUsage: &protocol.SubscriptionUsage{Provider: "test", Windows: []protocol.SubscriptionUsageWindow{}, CapturedAt: 1, SessionCostUSD: &zero}},
	}
	for _, payload := range payloads {
		data, err := (eventlog.Envelope{Seq: head + 1, Time: time.Unix(1, 0), Event: payload}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(ctx, id, head, data); err != nil {
			t.Fatal(err)
		}
		head++
	}
	want, err := harness.OpenView(ctx, log, id)
	if err != nil {
		t.Fatal(err)
	}
	tracking := &replayTrackingStore{Store: log}
	st := &checkpointStore{Store: tracking}
	if _, err := harness.OpenView(ctx, st, id); err != nil {
		t.Fatal(err)
	}
	tracking.fromStart = false
	got, err := harness.OpenView(ctx, st, id)
	if err != nil {
		t.Fatal(err)
	}
	if tracking.fromStart == testHasVCS() {
		t.Fatalf("empty-value checkpoint replay from start = %v, VCS identity = %v", tracking.fromStart, testHasVCS())
	}
	if !reflect.DeepEqual(got.Snapshot(), want.Snapshot()) {
		t.Fatalf("empty-value checkpoint = %+v subscription=%+v; replay = %+v subscription=%+v", got.Snapshot(), got.Snapshot().SubscriptionUsage, want.Snapshot(), want.Snapshot().SubscriptionUsage)
	}
}
