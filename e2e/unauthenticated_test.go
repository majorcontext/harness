package e2e

import (
	"context"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/testpoll"
)

// TestServeNonLoopbackNoTokenFailsClosed proves resolveUnauthenticated's
// fail-closed guard survives in the real `harness serve` binary: a
// non-loopback bind with no HARNESS_RUN_TOKEN and no -unauthenticated opt-in
// must still exit with an error, exactly as it did before this opt-in
// existed. This is the RED case the task's opt-in must never break — it is
// run against the real serveCmd/server.New entry point (a compiled
// subprocess), not a hand-built check.
func TestServeNonLoopbackNoTokenFailsClosed(t *testing.T) {
	skipShort(t)
	addr := freeAddrOnHost(t, "0.0.0.0")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, harnessBin, "serve", "-addr", addr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Dir = t.TempDir()
	cmd.Env = cleanEnv(map[string]string{
		"HARNESS_SESSION_DIR": t.TempDir(),
		// Point at a file that does not exist so config loads empty and
		// this test never reads a real operator's ~/.harness/config.json.
		"HARNESS_CONFIG": t.TempDir() + "/config.json",
	})
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("serve exited 0 on a non-loopback bind with no token and no -unauthenticated opt-in; want a non-zero exit. output:\n%s", out)
	}
	if !strings.Contains(string(out), "HARNESS_RUN_TOKEN is required") {
		t.Errorf("serve's error output does not name the missing token; got:\n%s", out)
	}
}

// TestServeNonLoopbackUnauthenticatedFlagStartsUnauthenticated proves the
// new opt-in path end to end against the real binary: -unauthenticated on a
// non-loopback bind, with HARNESS_RUN_TOKEN unset, starts the server and
// serves a real API call with no Authorization header at all, and logs the
// loud non-loopback warning. This also doubles as evidence the opt-in is
// never inferred: the ONLY difference from the fail-closed test above is the
// -unauthenticated flag.
func TestServeNonLoopbackUnauthenticatedFlagStartsUnauthenticated(t *testing.T) {
	skipShort(t)
	stderr, dialAddr := startNonLoopbackServe(t, map[string]string{
		"HARNESS_SESSION_DIR": t.TempDir(),
		"HARNESS_CONFIG":      writeGoalConfig(t, "http://127.0.0.1:1"),
		"ANTHROPIC_API_KEY":   "e2e-dummy-key",
	}, "-unauthenticated")

	// A real API call with NO Authorization header must succeed — proof
	// this is actually running unauthenticated, not merely that /health
	// (already unauthenticated on every box) answered.
	resp, err := http.Post("http://"+dialAddr+"/sessions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /sessions: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /sessions with no Authorization header = %d, want 201 (unauthenticated); body: %s", resp.StatusCode, body)
	}

	if !strings.Contains(stderr.String(), "serving unauthenticated on a non-loopback bind") {
		t.Errorf("expected the loud non-loopback-unauthenticated warning on stderr, got:\n%s", stderr.String())
	}
}

// TestServeHarnessUnauthenticatedEnvStartsUnauthenticated is the env-var twin
// of the flag test above: HARNESS_UNAUTHENTICATED=1 (with no -unauthenticated
// flag) opts in exactly the same way, against the real binary.
func TestServeHarnessUnauthenticatedEnvStartsUnauthenticated(t *testing.T) {
	skipShort(t)
	_, dialAddr := startNonLoopbackServe(t, map[string]string{
		"HARNESS_SESSION_DIR":     t.TempDir(),
		"HARNESS_CONFIG":          writeGoalConfig(t, "http://127.0.0.1:1"),
		"HARNESS_UNAUTHENTICATED": "1",
		"ANTHROPIC_API_KEY":       "e2e-dummy-key",
	})

	resp, err := http.Post("http://"+dialAddr+"/sessions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /sessions: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /sessions with no Authorization header = %d, want 201 (unauthenticated)", resp.StatusCode)
	}
}

// startNonLoopbackServe starts `harness serve` on a free 0.0.0.0 port with no
// run token and returns its stderr and the loopback address that reaches it
// once it is healthy. The port can be taken between freeAddrOnHost and the
// bind, and /health answers without a token, so a health answer does not show
// that the started process owns the port. Ownership is the process logging
// that it started; on Linux a taken port fails the bind. A serve that exits on a taken port starts again on a new
// one.
func startNonLoopbackServe(t *testing.T, env map[string]string, args ...string) (*lockedBuffer, string) {
	t.Helper()
	var last *lockedBuffer
	for range serveStartAttempts {
		addr := freeAddrOnHost(t, "0.0.0.0")
		cmd := exec.Command(harnessBin, append([]string{"serve", "-addr", addr}, args...)...)
		cmd.Dir = t.TempDir()
		cmd.Env = cleanEnv(env)
		stderr := &lockedBuffer{}
		cmd.Stderr = stderr
		g := startGroup(t, cmd)
		// Real cross-process startup: poll on a short interval bounded by a
		// deadline (synctest N/A).
		if !testpoll.UntilNoT(10*time.Second, func() bool {
			return !g.alive() || strings.Contains(stderr.String(), `"serve start"`)
		}, 15*time.Millisecond) {
			t.Fatalf("serve neither started nor exited on %s\nstderr:\n%s", addr, stderr.String())
		}
		if !g.alive() {
			if !strings.Contains(stderr.String(), "address already in use") {
				t.Fatalf("serve exited before it started on %s\nstderr:\n%s", addr, stderr.String())
			}
			last = stderr
			continue
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("splitting %q: %v", addr, err)
		}
		dialAddr := "127.0.0.1:" + port
		waitHealthyAt(t, dialAddr, stderr)
		return stderr, dialAddr
	}
	t.Fatalf("serve never bound a free port in %d attempts\nstderr:\n%s", serveStartAttempts, last.String())
	return nil, ""
}

// freeAddrOnHost returns a host:port address on the given host that was free
// a moment ago — the same reserve-then-release pattern freeAddr uses for
// "localhost", generalized to a non-loopback host like "0.0.0.0" so a test
// can drive serveCmd's non-loopback path against a real listener. Uses
// "tcp4" rather than the dual-stack default "tcp": on some platforms a
// dual-stack listener on "0.0.0.0:0" reports its own Addr() back as
// "[::]:port", silently swapping the literal IPv4 wildcard address this
// test means to exercise for an IPv6 one.
func freeAddrOnHost(t *testing.T, host string) string {
	t.Helper()
	l, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("reserving port on %s: %v", host, err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// waitHealthyAt polls GET /health on dialAddr until 200 or a deadline. Real
// cross-process startup: poll on a short interval bounded by a deadline
// (synctest N/A) — mirrors serveProc.waitHealthy, generalized to a caller
// that has not built a serveProc (the non-loopback tests above dial a
// different address than the one passed to -addr).
func waitHealthyAt(t *testing.T, dialAddr string, stderr *lockedBuffer) {
	t.Helper()
	if !testpoll.UntilNoT(10*time.Second, func() bool {
		resp, err := http.Get("http://" + dialAddr + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		return false
	}, 15*time.Millisecond) {
		extra := ""
		if stderr != nil {
			extra = "\nstderr:\n" + stderr.String()
		}
		t.Fatalf("serve did not become healthy on %s%s", dialAddr, extra)
	}
}
