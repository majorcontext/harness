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
	// afterRead, when set, runs after each Runtime.Read of the in-process host.
	afterRead func(rt *harness.Runtime, id string)
	// defaultDir is the work directory of the in-process host process.
	defaultDir string
	// login is the scripted login of the row. Nil: the host offers none.
	login *acpLoginRun
	// device is the scripted issuer of the device code login. Nil: none.
	device *deviceRun
	// logins and clock are the injected login state of the in-process host, and its clock. The binary host has its own.
	logins *harness.Logins
	clock  *fakeClock
	// injectedElicitations maps a step name to the elicitationId of the URL elicitation of an injected sign-in.
	injectedElicitations map[string]string
	// jsTool gives the wasm hosts the page tool js_echo.
	jsTool bool
	// memoryStore marks a host whose sessions the test cannot read: the browser host keeps them in the Worker.
	memoryStore bool
	// wasm marks a wasm host, which offers a login only to a client with elicitation.url.
	wasm bool
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
		Version:    "in-process",
		Getenv:     acpGetenv,
		DefaultDir: env.defaultDir,
		Logins:     env.logins,
	})
}

// startACPInProcess runs acp.Serve over two pipes in this process, so the
// race detector sees the adapter.
func startACPInProcess(t *testing.T, env acpEnv) (io.WriteCloser, io.Reader, func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	var rts acp.Runtimes = acpRuntimes(env)
	if env.afterRead != nil {
		rts = hookRuntimes{acpRuntimes(env), env.afterRead}
	}
	go func() {
		err := acp.Serve(t.Context(), rts, inR, outW, acp.Config{Version: "in-process", MCP: true, List: true, AllowedTools: env.allowed, Auth: env.login.auth(env)})
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

// hookRuntimes wraps the Runtimes of the in-process host so that each Read of a
// session calls after.
type hookRuntimes struct {
	*cwdruntimes.Runtimes
	after func(rt *harness.Runtime, id string)
}

func (h hookRuntimes) For(ctx context.Context, cwd string, opts acp.RuntimeOptions) (acp.Runtime, error) {
	rt, err := h.Runtimes.For(ctx, cwd, opts)
	if err != nil {
		return nil, err
	}
	return hookRuntime{rt, h.after}, nil
}

type hookRuntime struct {
	acp.Runtime
	after func(rt *harness.Runtime, id string)
}

func (h hookRuntime) Read(ctx context.Context, id string) (harness.Reader, error) {
	r, err := h.Runtime.Read(ctx, id)
	if err == nil {
		h.after(h.Runtime.(*harness.Runtime), id)
	}
	return r, err
}
