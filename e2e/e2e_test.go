// Package e2e holds black-box tests that drive harness through its HTTP
// routes: the real `harness serve` binary as a subprocess, and harness.Runtime
// in process, against a scripted model server. A contract row runs on both
// hosts and compares its observation by its disposition in runtime_rows_test.go.
//
// These tests spawn real processes and issue real SIGKILLs, so
// testing/synctest does not apply. Where a test must wait for an
// out-of-process condition it polls on a short interval bounded by a deadline.
//
// The whole package is skipped under `go test -short` (it builds a binary and
// forks processes).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// harnessBin is the path to the harness binary built once by TestMain.
var harnessBin string

// TestMain builds the real harness binary a single time for the whole package
// run. It skips the build under -short, where every test skips anyway. The
// testing flags are already registered by the generated test main, so a
// flag.Parse here makes testing.Short() meaningful before m.Run.
func TestMain(m *testing.M) {
	flag.Parse()
	if err := os.Setenv("OPENAI_API_KEY", ""); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	setDefaultParallel(32)
	if !testing.Short() {
		finishCover, err := startCover()
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e: coverage setup:", err)
			os.Exit(1)
		}
		bin, cleanup, err := binaries()
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e: building harness:", err)
			os.Exit(1)
		}
		harnessBin = bin
		stop := guardProcessGroups()
		code := m.Run()
		stop()
		if reportLeakedGroups() && code == 0 {
			code = 1
		}
		cleanup()
		if err := finishCover(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: coverage report:", err)
			code = 1
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// buildHarness compiles ./cmd/harness and ./harnesstest/fakeclaude into a temp
// dir and returns the harness binary path plus a cleanup func. It runs from
// the repo root (the parent of this package's directory).
func buildHarness() (string, func(), error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	root := filepath.Dir(wd) // <root>/e2e -> <root>
	dir, err := os.MkdirTemp("", "harness-e2e-bin")
	if err != nil {
		return "", nil, err
	}
	bin := filepath.Join(dir, "harness")
	if err := goBuild(root, dir); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return bin, func() { os.RemoveAll(dir) }, nil
}

// lockedBuffer is a concurrency-safe buffer for capturing a subprocess's
// stderr while the test also reads it on failure.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serveProc is a running `harness serve` subprocess.
type serveProc struct {
	*procGroup
	t      *testing.T
	addr   string
	token  string
	stderr *lockedBuffer
}

// startServe launches `harness serve` on a free port with the given session
// dir and config, then waits (bounded) for /health. The process group is killed at
// test cleanup. Its working directory is a throwaway temp dir.
func startServe(t *testing.T, sessDir, configPath string) *serveProc {
	t.Helper()
	return startServeIn(t, sessDir, configPath, t.TempDir())
}

// startServeIn is startServe with an explicit working directory, so a test can
// place an AGENTS.md / .agents/skills tree the served sessions will discover
// (the engine sets each session's WorkDir to the serve process's cwd).
func startServeIn(t *testing.T, sessDir, configPath, workDir string) *serveProc {
	t.Helper()
	return startServeProc(t, freeAddr, workDir, map[string]string{
		"HARNESS_SESSION_DIR": sessDir,
		"HARNESS_CONFIG":      configPath,
		"ANTHROPIC_API_KEY":   "e2e-dummy-key",
	})
}

// freeAddr returns a localhost address that was free a moment ago. The port
// can be taken between closing the probe listener and the subprocess binding,
// so a caller that starts serve uses startServeProc, which starts again on a new
// port when the bind fails.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// cleanEnv builds a child environment from the parent's, stripping any keys we
// set (and proxy vars) so the test's own shell can't leak a real API key,
// config, or proxy into the subprocess, then applying overrides.
func cleanEnv(overrides map[string]string) []string {
	strip := map[string]bool{
		"HARNESS_RUN_TOKEN": true, "HARNESS_SESSION_DIR": true,
		"HARNESS_CONFIG": true, "ANTHROPIC_API_KEY": true,
		"OPENAI_API_KEY": true, "HTTP_PROXY": true, "HTTPS_PROXY": true,
		"http_proxy": true, "https_proxy": true,
		// HARNESS_UNAUTHENTICATED: stripped so a host that happens to
		// export it can't leak an accidental opt-in into a test that
		// never intends one (TestServeNonLoopbackNoTokenFailsClosed in
		// particular, which asserts the fail-closed path). The two
		// unauthenticated_test.go opt-in tests are the only place that
		// sets it, via their own overrides below.
		"HARNESS_UNAUTHENTICATED": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strip[k] {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// --- HTTP client helpers -----------------------------------------------

func (p *serveProc) do(method, path string, body any) (*http.Response, []byte) {
	p.t.Helper()
	resp, data, err := p.send(method, path, body)
	if err != nil {
		p.t.Fatalf("%s %s: %v\nserve stderr:\n%s", method, path, err, p.stderr.String())
	}
	return resp, data
}

// send fails at waitBound plus waitMargin, so a serve process that stops
// answering fails the call, not the whole test binary.
func (p *serveProc) send(method, path string, body any) (*http.Response, []byte, error) {
	p.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := encodeBody(body)
		if err != nil {
			p.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(p.t.Context(), waitBound+waitMargin)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+p.addr+path, rdr)
	if err != nil {
		p.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := wireClient(p.t, http.DefaultClient).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp, data, err
}

func skipShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e: skipped under -short (builds a binary and forks real subprocesses)")
	}
}

// writeGoalConfig writes a config pointing the anthropic provider at baseURL and
// naming a distinct evaluator model, so goal requests are enabled.
func writeGoalConfig(t *testing.T, baseURL string) string {
	t.Helper()
	return writeGoalConfigWith(t, baseURL, nil)
}

// writeGoalConfigWith is writeGoalConfig with top-level keys that replace or
// add to the base config.
func writeGoalConfigWith(t *testing.T, baseURL string, extra map[string]any) string {
	t.Helper()
	cfg := map[string]any{
		"model":                "anthropic/claude-fable-5",
		"goal_evaluator_model": "anthropic/eval-model",
		"providers": map[string]any{
			"anthropic": map[string]any{
				"api_key_env": "ANTHROPIC_API_KEY",
				"base_url":    baseURL,
			},
		},
	}
	maps.Copy(cfg, extra)
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func setDefaultParallel(n int) {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == "test.parallel" })
	if f := flag.Lookup("test.parallel"); f != nil && !set {
		_ = f.Value.Set(strconv.Itoa(n))
	}
}
