package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRealEndToEnd starts a REAL box server + REAL hub server (see Start)
// and drives the ACTUAL tools/hub/index.html against them with Node +
// jsdom, using Node's own unmocked fetch — real HTTP, real SSE, real engine
// turns. This is the automated counterpart to the header comment's
// hand-test checklist: it exists so `go test ./tools/hub/e2e/...` (already
// part of the standard `go test -race ./...`) can verify, without any
// manual browser session, that:
//   - the hub server serves tools/hub/index.html byte-for-byte (production
//     wiring, not a stale copy);
//   - a populated URL fragment renders its box skeleton synchronously, no
//     "no boxes yet" flash;
//   - a real health/session poll resolves to a healthy dot + real
//     vcs_revision, and the box card's DOM node survives a real session
//     being created;
//   - a real engine turn (via the scripted provider in this package) renders
//     as a keyed durable message; expanding its reasoning block survives a
//     second real, server-driven turn — the actual keyed append-only
//     timeline behavior, exercised over a real SSE stream;
//   - a scrolled-up viewport is left alone by a real subsequent render
//     (pinned-tail autoscroll).
//
// Requires "node" on PATH and jsdom installed in this directory (see
// package.json — `npm install` once). Skips (does not fail) when either is
// unavailable, so the ordinary `go test -race ./...` recipe stays green in
// environments that haven't opted into this extra verification step.
func TestRealEndToEnd(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found on PATH; skipping real end-to-end hub verification (see tools/hub/e2e/README.md)")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine tools/hub/e2e directory")
	}
	dir := filepath.Dir(thisFile)
	script := filepath.Join(dir, "real_e2e.mjs")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "jsdom")); err != nil {
		t.Skipf("jsdom not installed in %s; run `npm install` there once (see README.md) to enable real end-to-end hub verification", dir)
	}

	stub, err := Start()
	if err != nil {
		t.Fatalf("starting the real box/hub stub servers: %v", err)
	}
	defer stub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, nodePath, script, stub.BoxBase, stub.HubBase, stub.Token)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	t.Log(out.String())
	if runErr != nil {
		t.Fatalf("real_e2e.mjs failed: %v", runErr)
	}
}
