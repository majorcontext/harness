//go:build e2e

package workload

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// spawnPollInterval/spawnPollDeadline bound every out-of-process "wait for
// the box to become ready" loop in this suite. Per AGENTS.md's Testing
// section, cross-process e2e is the one place a deadline-bounded poll loop
// is allowed in place of a channel wait — no in-process channel can cross
// the box's own process boundary.
const (
	spawnPollInterval = 2 * time.Second
	spawnPollDeadline = 5 * time.Minute
)

// Test_Row1_ImageSpawnsAndServes is acceptance row 1
// (docs/design/workload-validation.md): the real, digest-pinned harness
// image spawns on the deployed boxes stack and `harness serve` answers
// health checks on 4096. RUNNABLE-NOW once BOXES_URL/BOXES_TOKEN name a
// live service and the image referenced by HARNESS_IMAGE_REF (or the
// default GAR path) is published — both unmet as of this suite's authoring
// (see TestPrecondition_HarnessImageNonRoot's own finding), so this test
// currently skips.
func Test_Row1_ImageSpawnsAndServes(t *testing.T) {
	client, ok, missing := NewBoxesClient()
	if !ok {
		t.Skip(skipReason("boxes service reachable", "missing "+missing))
	}

	ctx, cancel := context.WithTimeout(context.Background(), spawnPollDeadline+30*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Skip(skipReason("boxes service reachable", err.Error()))
	}

	ref := imageRef()
	name := fmt.Sprintf("e2e-row1-%d", time.Now().UnixNano())
	box, err := client.Spawn(ctx, SpawnRequest{Name: name, Image: ref})
	if err != nil {
		t.Skip(skipReason("box spawn accepted", err.Error()))
	}
	t.Cleanup(func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer delCancel()
		if err := client.Delete(delCtx, box.ID); err != nil {
			t.Logf("cleanup: delete box %s: %v", box.ID, err)
		}
	})

	spawnStart := time.Now()
	var running *Box
	deadline := time.Now().Add(spawnPollDeadline)
	for time.Now().Before(deadline) {
		got, err := client.Get(ctx, box.ID)
		if err != nil {
			t.Fatalf("get box %s: %v", box.ID, err)
		}
		if got.Status == "running" && got.TunnelURL != "" {
			running = got
			break
		}
		time.Sleep(spawnPollInterval)
	}
	if running == nil {
		t.Fatalf("box %s did not reach status=running with a tunnel URL within %v", box.ID, spawnPollDeadline)
	}
	spawnToRunning := time.Since(spawnStart)
	t.Logf("box %s reached status=running in %v (tunnel_url=%s)", box.ID, spawnToRunning, running.TunnelURL)

	hc := NewHarnessClient(running.TunnelURL, running.RunToken)
	healthCtx, healthCancel := context.WithTimeout(ctx, 15*time.Second)
	defer healthCancel()
	health, err := hc.Health(healthCtx)
	if err != nil {
		t.Fatalf("GET /health on box %s: %v", box.ID, err)
	}
	if health.Version == "" {
		t.Errorf("health response has empty version: %+v", health)
	}
	t.Logf("box %s harness serve: version=%s vcs_revision=%s session_sync=%s (spawn-to-healthy=%v)",
		box.ID, health.Version, health.VCSRevision, health.SessionSync, spawnToRunning)
}
