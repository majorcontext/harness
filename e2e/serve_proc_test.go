package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"maps"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/testpoll"
)

func newRunToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

const serveStartAttempts = 5

// startServeProc starts `harness serve` on an address from pickAddr and
// returns once that process answers an authenticated request. The port can be
// taken between pickAddr and the bind, and then another test's serve answers
// on it. Each process has its own run token, so only the process started here
// accepts it. A serve that exits on a taken port starts again on a new one.
func startServeProc(t *testing.T, pickAddr func(*testing.T) string, workDir string, env map[string]string, args ...string) *serveProc {
	t.Helper()
	var last *serveProc
	for range serveStartAttempts {
		addr := pickAddr(t)
		token := newRunToken(t)
		cmd := exec.Command(harnessBin, append([]string{"serve", "-addr", addr}, args...)...)
		cmd.Dir = workDir
		cmd.Env = cleanEnv(withToken(env, token))
		stderr := &lockedBuffer{}
		cmd.Stderr = stderr
		p := &serveProc{procGroup: startGroup(t, cmd), t: t, addr: addr, token: token, stderr: stderr}
		if p.waitOwned() {
			return p
		}
		last = p
	}
	t.Fatalf("serve never bound a free port in %d attempts\nstderr:\n%s", serveStartAttempts, last.stderr.String())
	return nil
}

func withToken(env map[string]string, token string) map[string]string {
	out := maps.Clone(env)
	out["HARNESS_RUN_TOKEN"] = token
	return out
}

func TestStartServeProcMovesOffATakenPort(t *testing.T) {
	skipShort(t)
	t.Parallel()
	first := startServe(t, t.TempDir(), writeGoalConfig(t, "http://127.0.0.1:1"))
	picks := []string{first.addr}
	pick := func(t *testing.T) string {
		if len(picks) > 0 {
			addr := picks[0]
			picks = picks[1:]
			return addr
		}
		return freeAddr(t)
	}
	second := startServeProc(t, pick, t.TempDir(), map[string]string{
		"HARNESS_SESSION_DIR": t.TempDir(),
		"HARNESS_CONFIG":      writeGoalConfig(t, "http://127.0.0.1:1"),
		"ANTHROPIC_API_KEY":   "e2e-dummy-key",
	})
	if second.addr == first.addr {
		t.Errorf("second serve reports the taken address %s, want a new one", first.addr)
	}
}

// waitOwned polls until the started process has bound its port, answers with
// its own token and has opened its stored sessions. It reports false when the process exited on
// a taken port, and fails the test on any other exit or on the deadline. Real
// cross-process startup: poll on a short interval bounded by a deadline
// (synctest N/A).
func (p *serveProc) waitOwned() bool {
	p.t.Helper()
	if !testpoll.UntilNoT(10*time.Second, func() bool {
		if !p.alive() {
			return true
		}
		if !strings.Contains(p.stderr.String(), `"serve start"`) {
			return false
		}
		req, err := http.NewRequest(http.MethodGet, "http://"+p.addr+"/sessions", nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+p.token)
		resp, err := wireClient(p.t, http.DefaultClient).Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK && strings.Contains(p.stderr.String(), `"opened stored sessions"`)
	}, 15*time.Millisecond) {
		p.t.Fatalf("serve did not answer on %s\nstderr:\n%s", p.addr, p.stderr.String())
	}
	if p.alive() {
		return true
	}
	if !strings.Contains(p.stderr.String(), "address already in use") {
		p.t.Fatalf("serve exited before it answered on %s\nstderr:\n%s", p.addr, p.stderr.String())
	}
	return false
}
