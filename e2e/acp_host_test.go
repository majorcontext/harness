package e2e

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/acp/cwdruntimes"
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

// acpRuntimes builds the Runtimes of the in-process host with the package that
// `harness acp` uses, over the user config of the scenario.
func acpRuntimes(env acpEnv) *cwdruntimes.Runtimes {
	return cwdruntimes.New(cwdruntimes.Options{
		Config: func(cwd string) (*config.Config, error) { return config.LoadProject(env.configPath, cwd) },
		Shared: func(*config.Config) (harness.Store, harness.Owner, error) {
			return store.NewDiskStore(env.sessDir), harness.NewLocalOwner(1), nil
		},
		Version: "in-process",
		Getenv:  acpGetenv,
	})
}

// startACPInProcess runs acp.Serve over two pipes in this process, so the
// race detector sees the adapter.
func startACPInProcess(t *testing.T, env acpEnv) (io.WriteCloser, io.Reader, func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	rts := acpRuntimes(env)
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
