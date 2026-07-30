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

// TestRealEndToEnd starts a REAL box server + a REAL static file server for
// the ACTUAL tools/monitor/index.html (see stub.go's Start) and drives that
// page with Node + jsdom, using Node's own unmocked fetch — real HTTP, real
// SSE, real engine turns (including a REAL "bash" tool execution — see
// stub.go's toolCallPart). This is the automated counterpart to
// index.html's header-comment hand-test checklist: it exists so plain
// `go test -race ./...` — the exact command already used to verify this
// repo, no extra step required — checks, without any manual browser
// session, that:
//   - connecting with a wrong token surfaces an inline error, and a correct
//     one renders a real /health identity line and an empty board;
//   - a real scripted turn (streaming text + a real, briefly-blocking bash
//     tool call) drives a board row through the streaming/tool phases and
//     back to idle with outcome "completed";
//   - a session left running long enough (against shrunk, test-only
//     staleness thresholds — see index.html's window.__monitorTuning seam)
//     crosses the quiet and stalled tiers;
//   - opening a session's detail view (via a real row click) renders its
//     durable history, including a completed tool fold and a fold that is
//     still genuinely running at the moment it's observed;
//   - the composer's prompt.queued optimistic entry appears for a send into
//     a BUSY session and is replaced (not duplicated) once the durable
//     message lands; a send into an idle session runs a normal turn; a send
//     against an unknown session id surfaces the server's real non-2xx
//     error text inline; a composer submit renders the operator's own text
//     SYNCHRONOUSLY, before the POST even resolves, settling to exactly one
//     entry once the real message lands; a real, currently-running turn
//     with no content yet shows a quiet "Thinking…" pending indicator,
//     dismissed the instant real streaming content arrives; an idle-send's
//     optimistic operator entry precedes — in actual DOM order, not merely
//     "both exist" — that turn's own pending indicator and streaming
//     assistant reply, never the other way around;
//   - a durable message's reasoning part and text part (bundled together
//     ahead of a real tool call) render as two distinct, correctly-labeled
//     entries — never merged onto one DOM node showing the wrong label;
//   - killing the box's HTTP layer server-side flips the header to
//     "reconnecting…", and restarting it resumes the stream;
//   - a real provider stream failure renders a critical transcript error
//     entry with the chip settling to idle promptly (no poll dependency);
//   - detailState.liveEvents crosses a tuned cap and reconcileDetail trims
//     it back down; a reconnect gap (pollOnce advancing state.lastSeq past
//     what the page's own stream actually delivered) heals via the SAME
//     reconcileDetail, backfilling a turn the detail view never observed
//     live;
//   - embeddedConnectPlan's "frictionless local" behavior against the
//     box's REAL GET /monitor route on fresh page loads: an Unauthenticated
//     box auto-connects with zero typing; a "#t=<token>" capability URL
//     auto-connects a tokened box and scrubs the token from the visible
//     URL; a tokened box with no token anywhere falls back to a usable,
//     token-only panel (host absent).
//
// Dependency setup is automatic, not a documented manual prerequisite: if
// jsdom isn't already installed in this directory, the test runs `npm ci`
// (falling back to `npm install`) itself before driving real_e2e.mjs, using
// the package.json/package-lock.json committed alongside this file — same
// pattern as tools/hub/e2e/e2e_test.go, including the one skip condition
// (no Node toolchain on PATH at all — the one case where this repo's other
// required check, `node --test tools/monitor/*_test.mjs`, would ALSO be
// unrunnable).
func TestRealEndToEnd(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found on PATH — this environment could not run `node --test tools/monitor/*_test.mjs` either; skipping real end-to-end monitor verification")
	}
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not found on PATH (normally ships with node); skipping real end-to-end monitor verification")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine tools/monitor/e2e directory")
	}
	dir := filepath.Dir(thisFile)
	script := filepath.Join(dir, "real_e2e.mjs")

	if _, err := os.Stat(filepath.Join(dir, "node_modules", "jsdom")); err != nil {
		installDeps(t, npmPath, dir)
	}

	stub, err := Start()
	if err != nil {
		t.Fatalf("starting the real box/monitor stub servers: %v", err)
	}
	defer stub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, nodePath, script, stub.BoxBase, stub.MonitorBase, stub.Token, stub.UnauthBase)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	t.Logf("real_e2e.mjs runtime: %s", time.Since(start))
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
// reason to pretend everything passed. Copied from tools/hub/e2e/e2e_test.go.
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
			t.Fatalf("npm install failed too (%v); real end-to-end monitor verification requires network access to npm's registry on first run:\n%s", err, out.String())
		}
	}
	t.Log(out.String())
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "jsdom")); err != nil {
		t.Fatalf("jsdom still missing from %s/node_modules after npm install; something is wrong with the dependency install", dir)
	}
}
