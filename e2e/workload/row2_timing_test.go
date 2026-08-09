//go:build e2e

package workload

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Test_Row2_WorkloadTiming is acceptance row 2: real dev-workload timing —
// pnpm install (warm), a build, a test run — on the deployed boxes stack
// (gVisor on GKE), recorded to a JSON artifact so a later run against
// Modal can be diffed against it. See
// docs/design/workload-validation.md for the pass bar (within ~1.5x of the
// Modal baseline per phase).
//
// There is no raw exec endpoint on either API surface this suite has a
// spec for (see doc.go), so each phase is driven through a REAL harness
// agent turn: a directive telling the model to run one shell command via
// its bash tool and reply once it finishes. Timing is measured from
// PromptAsync submission to that turn's turn.end event, which includes
// the agent's own dispatch overhead on top of the raw command's wall
// time — a deliberate, documented choice: it is what a fleet workload
// actually pays, not a synthetic lower bound.
//
// BLOCKED-ON-boxes-service (spawn) and BLOCKED-ON-baked-repo (a project
// with an install/build/test script must already be checked out in the
// image at the workdir this suite points at, e.g. via HARNESS_WORKLOAD_DIR)
// until both exist; skips cleanly until then.
func Test_Row2_WorkloadTiming(t *testing.T) {
	client, ok, missing := NewBoxesClient()
	if !ok {
		t.Skip(skipReason("boxes service reachable", "missing "+missing))
	}

	ctx, cancel := context.WithTimeout(context.Background(), spawnPollDeadline+10*time.Minute)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Skip(skipReason("boxes service reachable", err.Error()))
	}

	ref := imageRef()
	name := fmt.Sprintf("e2e-row2-%d", time.Now().UnixNano())
	box, err := client.Spawn(ctx, SpawnRequest{Name: name, Image: ref})
	if err != nil {
		t.Skip(skipReason("box spawn accepted", err.Error()))
	}
	t.Cleanup(func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer delCancel()
		_ = client.Delete(delCtx, box.ID)
	})

	running := waitForRunningBox(t, ctx, client, box.ID)
	hc := NewHarnessClient(running.TunnelURL, running.RunToken)

	sess, err := hc.CreateSession(ctx, CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		endCtx, endCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer endCancel()
		_ = hc.EndSession(endCtx, sess.ID)
	})

	recorder := newTimingRecorder("boxes-gvisor", ref)
	phases := []struct {
		name      string
		directive string
	}{
		{"pnpm_install_warm", "Run `pnpm install --frozen-lockfile` in the workdir using the bash tool. Reply with only the exit code once it finishes."},
		{"build", "Run the project's build script (e.g. `pnpm build`) using the bash tool. Reply with only the exit code once it finishes."},
		{"test", "Run the project's test script (e.g. `pnpm test`) using the bash tool. Reply with only the exit code once it finishes."},
	}

	for _, phase := range phases {
		phase := phase
		var outcome turnEndPayload
		err := recorder.time(phase.name, func() error {
			resp, err := hc.PromptAsync(ctx, sess.ID, phase.directive)
			if err != nil {
				return err
			}
			outcome, err = waitForTurnEnd(ctx, hc, sess.ID, resp.Seq-1)
			return err
		})
		if err != nil {
			t.Errorf("phase %s: %v", phase.name, err)
			break
		}
		if outcome.Outcome != "completed" {
			t.Errorf("phase %s: turn ended with outcome=%s error=%s", phase.name, outcome.Outcome, outcome.Error)
			break
		}
	}

	t.Log(recorder.summary())

	artifactPath := defaultTimingArtifactPath()
	if err := recorder.writeArtifact(artifactPath); err != nil {
		t.Errorf("write timing artifact to %s: %v", artifactPath, err)
	} else {
		t.Logf("timing artifact written to %s", artifactPath)
	}
}

// waitForRunningBox polls Get until a box reaches status=running with a
// tunnel URL, or fails the test on timeout. Shared by every row that needs
// a live box beyond row 1's own spawn-timing assertion.
func waitForRunningBox(t *testing.T, ctx context.Context, client *BoxesClient, id string) *Box {
	t.Helper()
	deadline := time.Now().Add(spawnPollDeadline)
	for time.Now().Before(deadline) {
		got, err := client.Get(ctx, id)
		if err != nil {
			t.Fatalf("get box %s: %v", id, err)
		}
		if got.Status == "running" && got.TunnelURL != "" {
			return got
		}
		time.Sleep(spawnPollInterval)
	}
	t.Fatalf("box %s did not reach status=running with a tunnel URL within %v", id, spawnPollDeadline)
	return nil
}
