//go:build e2e

package workload

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Test_Row3_AgentSessionGoalLoop is acceptance row 3: a real harness agent
// session with a goal loop performing file edits, tool calls, and git on
// the baked repo — with the session log fsyncing in DEFAULT mode (plain
// `session_sync: "fsync"`, never the Modal volume workaround this repo's
// docs/deploy-modal.md documents), and no wedge (the goal reaches a
// terminal outcome inside this test's own deadline, never left "busy"
// forever).
//
// BLOCKED-ON-boxes-service until BOXES_URL/BOXES_TOKEN name a live
// deployment; skips cleanly until then.
func Test_Row3_AgentSessionGoalLoop(t *testing.T) {
	client, ok, missing := NewBoxesClient()
	if !ok {
		t.Skip(skipReason("boxes service reachable", "missing "+missing))
	}

	ctx, cancel := context.WithTimeout(context.Background(), spawnPollDeadline+15*time.Minute)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Skip(skipReason("boxes service reachable", err.Error()))
	}

	ref := imageRef()
	name := fmt.Sprintf("e2e-row3-%d", time.Now().UnixNano())
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

	health, err := hc.Health(ctx)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	// The invariant this row exists to prove: session durability rides the
	// DEFAULT fsync path on real boxes-stack storage, not the Modal
	// Volume v2 fsync-skip workaround (docs/deploy-modal.md's
	// session_sync: "volume" mode) — that workaround exists specifically
	// because Modal's FUSE-backed volume can wedge on fsync, a failure
	// mode the boxes stack's own storage must not reproduce.
	if health.SessionSync != "fsync" {
		t.Errorf("box %s reports session_sync=%q, want %q (the default-mode fsync invariant this row asserts)",
			box.ID, health.SessionSync, "fsync")
	}

	sess, err := hc.CreateSession(ctx, CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		endCtx, endCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer endCancel()
		_ = hc.EndSession(endCtx, sess.ID)
	})

	condition := "a new file named e2e-row3-proof.txt exists in the repository root, " +
		"containing the exact text DONE, and `git log -1 --oneline` shows a commit " +
		"that added it — verified by reading the file and running that git command, " +
		"not merely asserted"
	if err := hc.StartGoal(ctx, sess.ID, GoalStartRequest{Condition: condition, MaxTurns: 8}); err != nil {
		t.Fatalf("start goal: %v", err)
	}

	outcome, err := waitForGoalOutcome(ctx, hc, sess.ID, sess.Seq)
	if err != nil {
		t.Fatalf("goal on session %s never reached a terminal outcome (possible wedge): %v", sess.ID, err)
	}
	if outcome != "achieved" {
		t.Errorf("goal on session %s ended with outcome=%s, want achieved", sess.ID, outcome)
	}

	final, err := hc.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session %s after goal: %v", sess.ID, err)
	}
	if final.State == "busy" {
		t.Errorf("session %s still reports state=busy after a terminal goal outcome (wedge)", sess.ID)
	}
	t.Logf("session %s: goal outcome=%s final state=%s messages=%d", sess.ID, outcome, final.State, final.Messages)
}
