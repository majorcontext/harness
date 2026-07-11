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
// hand-test checklist: it exists so plain `go test -race ./...` — the exact
// command already used to verify this repo, no extra step required —
// checks, without any manual browser session, that:
//   - the hub server serves tools/hub/index.html byte-for-byte (production
//     wiring, not a stale copy);
//   - a populated URL fragment renders its box skeleton synchronously, no
//     "no boxes yet" flash;
//   - a real health/session poll resolves to a healthy dot, and the box
//     card's DOM node survives a real session being created;
//   - a real engine turn (via the scripted provider in this package) renders
//     as a keyed durable message; expanding its reasoning block survives a
//     second real, server-driven turn — the actual keyed append-only
//     timeline behavior, exercised over a real SSE stream;
//   - a scrolled-up viewport is left alone by a real subsequent render
//     (pinned-tail autoscroll).
//
// Dependency setup is automatic, not a documented manual prerequisite: if
// jsdom isn't already installed in this directory, the test runs
// `npm ci` (falling back to `npm install` if there is no lockfile-clean
// install available) itself before driving real_e2e.mjs, using the
// package.json/package-lock.json committed alongside this file. `node` (and
// therefore `npm`, which ships with it) is already a hard requirement of
// this repo's own `node --test tools/hub/*_test.mjs` check, so the only way
// this test skips is the one case where that other required command would
// ALSO be unrunnable: no Node toolchain on PATH at all. Any environment
// that can run the three documented verification commands runs this test
// for real, every time.
func TestRealEndToEnd(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found on PATH — this environment could not run `node --test tools/hub/*_test.mjs` either; skipping real end-to-end hub verification")
	}
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not found on PATH (normally ships with node); skipping real end-to-end hub verification")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine tools/hub/e2e directory")
	}
	dir := filepath.Dir(thisFile)
	script := filepath.Join(dir, "real_e2e.mjs")

	if _, err := os.Stat(filepath.Join(dir, "node_modules", "jsdom")); err != nil {
		installDeps(t, npmPath, dir)
	}

	stub, err := Start()
	if err != nil {
		t.Fatalf("starting the real box/hub stub servers: %v", err)
	}
	defer stub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

// installDeps runs `npm ci` (a clean, lockfile-exact install, using the
// package-lock.json committed in this directory) to fetch jsdom before the
// real end-to-end check needs it, so a fresh clone requires no manual setup
// step beyond having node/npm on PATH. Falls back to `npm install` if `npm
// ci` itself is unavailable in this npm version (older npm predates it).
// Requires network access to npm's registry; a genuinely offline CI run
// fails loudly here (t.Fatalf) rather than silently skipping the real
// check — an offline environment is a real gap in verification, not a
// reason to pretend everything passed.
func installDeps(t *testing.T, npmPath, dir string) {
	t.Helper()
	t.Logf("jsdom not present in %s; running npm ci to install it (see package.json/package-lock.json)", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, npmPath, "ci", "--no-audit", "--no-fund")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Logf("npm ci failed (%v), output:\n%s\nfalling back to npm install", err, out.String())
		cmd = exec.CommandContext(ctx, npmPath, "install", "--no-audit", "--no-fund")
		cmd.Dir = dir
		out.Reset()
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("npm install failed too (%v); real end-to-end hub verification requires network access to npm's registry on first run:\n%s", err, out.String())
		}
	}
	t.Log(out.String())
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "jsdom")); err != nil {
		t.Fatalf("jsdom still missing from %s/node_modules after npm install; something is wrong with the dependency install", dir)
	}
}
