//go:build e2e

package workload

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Test_Row6_KillResumeRealSpawn is acceptance row 6: kill-resume on a REAL
// spawn — SIGKILL the boxes-service's own lifecycle worker WHILE a spawn
// is in flight, not the fake-backed demo (the in-repo e2e/ package's own
// SIGKILL tests, e2e/e2e_test.go, already cover the harness-process side of
// this with a fake provider; this row is the complementary infra-side
// chaos case).
//
// BLOCKED-ON-infra-chaos-hook: killing "the lifecycle worker" means killing
// a specific process or pod inside the boxes-service's own deployment
// (e.g. `kubectl delete pod -l role=lifecycle-worker --grace-period=0`) —
// something only cluster-level access can do, which this suite deliberately
// does not carry (it owns e2e/ only, not infra/ or deploy/). BOXES_CHAOS_
// KILL_CMD lets the infra/deploy side wire that access in once it exists;
// this test skips cleanly without it.
//
// The property under test, once that hook exists: a spawn request issued
// just before the kill either (a) completes successfully once the lifecycle
// worker's replacement picks it back up, or (b) fails cleanly and is safe
// to retry — never a box stuck permanently in a half-spawned limbo state
// with no path forward.
func Test_Row6_KillResumeRealSpawn(t *testing.T) {
	killCmd := os.Getenv("BOXES_CHAOS_KILL_CMD")
	if killCmd == "" {
		t.Skip(skipReason("infra chaos hook", "BOXES_CHAOS_KILL_CMD not set; killing the lifecycle worker needs cluster access this suite does not carry"))
	}

	client, ok, missing := NewBoxesClient()
	if !ok {
		t.Skip(skipReason("boxes service reachable", "missing "+missing))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*spawnPollDeadline)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Skip(skipReason("boxes service reachable", err.Error()))
	}

	ref := imageRef()
	name := fmt.Sprintf("e2e-row6-%d", time.Now().UnixNano())

	spawnErrCh := make(chan error, 1)
	var box *Box
	go func() {
		var err error
		box, err = client.Spawn(ctx, SpawnRequest{Name: name, Image: ref})
		spawnErrCh <- err
	}()

	// Give the spawn request a moment to actually land on the lifecycle
	// worker before killing it — this is inherent slop in a black-box
	// chaos test against a real service with no hook to synchronize on
	// "the worker has started processing this spawn", so the window is a
	// short, explicit, deadline-bounded sleep rather than an unbounded
	// guess; it does not gate correctness, only how likely this run is to
	// land mid-spawn versus pre- or post-spawn.
	time.Sleep(500 * time.Millisecond)
	runShell(t, ctx, killCmd)

	spawnErr := <-spawnErrCh
	if spawnErr != nil {
		t.Logf("spawn request failed during chaos (acceptable — retry is the recovery path): %v", spawnErr)
		box, spawnErr = client.Spawn(ctx, SpawnRequest{Name: name, Image: ref})
		if spawnErr != nil {
			t.Fatalf("retry spawn after chaos also failed: %v", spawnErr)
		}
	}
	t.Cleanup(func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer delCancel()
		_ = client.Delete(delCtx, box.ID)
	})

	running := waitForRunningBox(t, ctx, client, box.ID)
	hc := NewHarnessClient(running.TunnelURL, running.RunToken)
	if _, err := hc.Health(ctx); err != nil {
		t.Fatalf("box %s did not come up healthy after kill-resume: %v", box.ID, err)
	}
	t.Logf("box %s reached status=running and healthy after lifecycle-worker chaos", box.ID)
}
