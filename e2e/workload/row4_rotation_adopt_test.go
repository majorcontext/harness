//go:build e2e

package workload

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Test_Row4_RotationMidSessionAdopt is acceptance row 4: log rotation
// (compressed max-age) mid-session, with disk carrying the session across
// the rotation via ADOPT (docs/design/fleet-model.md §4) — a fresh box
// process on the SAME name/volume restores full history.
//
// This row bundles two properties that are independently testable, and
// this test scaffolds both, gated on separate preconditions:
//
//  1. ADOPT itself: kill/respawn a box under the same name and confirm the
//     session (message count, seq) survives. This is a harness/fleet-model
//     property already specified and implemented
//     (docs/design/fleet-model.md) — RUNNABLE-NOW once a boxes service
//     exists to spawn/respawn against.
//  2. Log rotation under "compressed max-age": no harness- or repo-level
//     mechanism performs this — it is a box-host log-management policy
//     (e.g. logrotate, or the boxes service's own log shipper) that lives
//     entirely outside this repo's code and outside client_boxes.go's
//     provisional API. BLOCKED-ON-log-rotation-mechanism: there is nothing
//     in this repo, or in the boxes-service shapes named by the acceptance
//     table, to trigger or observe it. This test accepts an optional
//     ROTATE_LOGS_CMD env var (a shell command the infra/deploy side can
//     wire up once its rotation mechanism exists, e.g. one that SSHes into
//     the box's host and forces logrotate) and skips the rotation-specific
//     assertion — while still exercising the ADOPT half — when it is unset.
func Test_Row4_RotationMidSessionAdopt(t *testing.T) {
	client, ok, missing := NewBoxesClient()
	if !ok {
		t.Skip(skipReason("boxes service reachable", "missing "+missing))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*spawnPollDeadline+2*time.Minute)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Skip(skipReason("boxes service reachable", err.Error()))
	}

	ref := imageRef()
	name := fmt.Sprintf("e2e-row4-%d", time.Now().UnixNano())

	box, err := client.Spawn(ctx, SpawnRequest{Name: name, Image: ref})
	if err != nil {
		t.Skip(skipReason("box spawn accepted", err.Error()))
	}
	// Deliberately no unconditional cleanup on `box.ID` — this test
	// respawns under the SAME name, so the box identity changes mid-test;
	// see the respawned box's own cleanup below.

	running := waitForRunningBox(t, ctx, client, box.ID)
	hc := NewHarnessClient(running.TunnelURL, running.RunToken)

	sess, err := hc.CreateSession(ctx, CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := hc.PromptAsync(ctx, sess.ID, "Reply with only the word: adopted"); err != nil {
		t.Fatalf("prompt before rotation/respawn: %v", err)
	}
	if _, err := waitForTurnEnd(ctx, hc, sess.ID, sess.Seq); err != nil {
		t.Fatalf("waiting for pre-respawn turn to finish: %v", err)
	}
	before, err := hc.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session before respawn: %v", err)
	}

	if rotateCmd := os.Getenv("ROTATE_LOGS_CMD"); rotateCmd != "" {
		runShell(t, ctx, rotateCmd)
	} else {
		t.Log(skipReason("log rotation (compressed max-age) trigger", "ROTATE_LOGS_CMD not set; exercising ADOPT only"))
	}

	// Kill this box (compute is cattle) and respawn under the SAME name
	// (docs/design/fleet-model.md's ADOPT) — the session must restore from
	// the surviving volume, not from anything this test remembers.
	if err := client.Delete(ctx, box.ID); err != nil {
		t.Fatalf("delete box %s before respawn: %v", box.ID, err)
	}

	respawned, err := client.Spawn(ctx, SpawnRequest{Name: name, Image: ref})
	if err != nil {
		t.Fatalf("respawn box %q (ADOPT): %v", name, err)
	}
	t.Cleanup(func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer delCancel()
		_ = client.Delete(delCtx, respawned.ID)
	})

	adoptedBox := waitForRunningBox(t, ctx, client, respawned.ID)
	adoptedHC := NewHarnessClient(adoptedBox.TunnelURL, adoptedBox.RunToken)

	after, err := adoptedHC.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session %s after ADOPT respawn: %v", sess.ID, err)
	}
	if after.Messages < before.Messages {
		t.Errorf("session %s lost history across ADOPT: messages before=%d after=%d", sess.ID, before.Messages, after.Messages)
	}
	if after.Seq < before.Seq {
		t.Errorf("session %s seq went backwards across ADOPT: before=%d after=%d", sess.ID, before.Seq, after.Seq)
	}
	t.Logf("ADOPT verified: session %s messages before=%d after=%d, seq before=%d after=%d",
		sess.ID, before.Messages, after.Messages, before.Seq, after.Seq)
}
