package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/store"
)

// acpEnv is what a scenario gives its host: the user config, which points the
// provider at the scripted model, the directory of the session logs, and the
// default tool set of the in-process adapter.
type acpEnv struct {
	configPath string
	sessDir    string
	allowed    []string
}

const acpDummyKey = "e2e-dummy-key"

func acpGetenv(string) string { return acpDummyKey }

// acpTestRuntimes is the Runtimes of the in-process host. It builds one
// harness.Runtime for each cwd with the project config of that cwd, over one
// store and one owner, as `harness acp` does.
type acpTestRuntimes struct {
	env   acpEnv
	store harness.Store
	owner harness.Owner
	mu    sync.Mutex
	rts   map[string]*harness.Runtime
}

func newACPTestRuntimes(env acpEnv) *acpTestRuntimes {
	return &acpTestRuntimes{env: env, store: store.NewDiskStore(env.sessDir), owner: harness.NewLocalOwner(1), rts: map[string]*harness.Runtime{}}
}

func (r *acpTestRuntimes) For(_ context.Context, cwd string) (acp.Runtime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rt := r.rts[cwd]; rt != nil {
		return rt, nil
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: cwd %q is not a directory", harness.ErrInvalidRequest, cwd)
	}
	cfg, err := config.LoadProject(r.env.configPath, cwd)
	if err != nil {
		return nil, err
	}
	rt, err := harness.New(harness.Options{Store: r.store, Owner: r.owner, Config: *cfg, WorkDir: cwd, Getenv: acpGetenv})
	if err != nil {
		return nil, err
	}
	r.rts[cwd] = rt
	return rt, nil
}

func (r *acpTestRuntimes) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for _, rt := range r.rts {
		if err := rt.Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// startACPInProcess runs acp.Serve over two pipes in this process, so the
// race detector sees the adapter.
func startACPInProcess(t *testing.T, env acpEnv) (io.WriteCloser, io.Reader, func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	rts := newACPTestRuntimes(env)
	go func() {
		err := acp.Serve(t.Context(), rts, inR, outW, acp.Config{Version: "in-process", MCP: true, AllowedTools: env.allowed})
		_ = outW.Close()
		_ = inR.CloseWithError(io.ErrClosedPipe)
		done <- err
	}()
	stop := func() {
		t.Helper()
		if err := <-done; err != nil {
			t.Errorf("acp.Serve ended with %v", err)
		}
	}
	return inW, outR, stop
}

// acpTakeover opens the session in another runtime over the same session
// directory, which fences the owner that holds it.
func acpTakeover(t *testing.T, env acpEnv, cwd, id string) {
	t.Helper()
	cfg, err := config.LoadProject(env.configPath, cwd)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := harness.New(harness.Options{Store: store.NewDiskStore(env.sessDir), Config: *cfg, WorkDir: cwd, Getenv: acpGetenv})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	if _, err := rt.Open(t.Context(), id); err != nil {
		t.Fatalf("taking over session %s: %v", id, err)
	}
}

// writeACPDir writes the files of a scenario directory.
func writeACPDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
