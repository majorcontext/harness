package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// TestPluginHelperProcess is not a real test. It is invoked as a subprocess
// (re-executing this test binary via os.Args[0]) by tests below that need a
// real plugin process speaking the protocol over stdio — the same
// self-exec trick os/exec's own tests use (TestHelperProcess), so no extra
// build step or on-disk fixture binary is needed. Guarded by
// GO_WANT_PLUGIN_HELPER so an ordinary `go test` run treats it as a no-op.
//
// It must never write anything but the plugin protocol to stdout, so on the
// helper path it calls os.Exit directly instead of returning — returning
// would let the testing framework print its own summary to stdout after
// plugin.Serve returns, corrupting the NDJSON stream for any harness process
// that had it running as a real plugin.
func TestPluginHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_PLUGIN_HELPER") != "1" {
		t.Skip("helper process, not a real test")
	}
	name := os.Getenv("PLUGIN_NAME")
	if name == "" {
		name = "testplug"
	}
	marker := os.Getenv("PLUGIN_MARKER")
	if spawnLog := os.Getenv("PLUGIN_SPAWN_LOG"); spawnLog != "" {
		if f, err := os.OpenFile(spawnLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, "spawned")
			f.Close()
		}
	}
	// When PLUGIN_ECHO_VAR names another env var, echo that var's value and
	// this process's cwd into the manifest's (otherwise-unused) Version
	// field, so a test proves plugin.ProbeSpec's spec.Env/spec.Dir actually
	// reached the spawned process — the manifest returned by `initialize`
	// is the only channel Probe/ProbeSpec observes back from the plugin.
	version := ""
	if echoVar := os.Getenv("PLUGIN_ECHO_VAR"); echoVar != "" {
		wd, _ := os.Getwd()
		if resolved, err := filepath.EvalSymlinks(wd); err == nil {
			wd = resolved
		}
		version = fmt.Sprintf("echo=%s;cwd=%s", os.Getenv(echoVar), wd)
	}
	err := plugin.Serve(plugin.Manifest{Name: name, Version: version}, &plugin.Hooks{
		SystemTransform: func(ctx context.Context, c *plugin.Client, _ *plugin.SystemTransformRequest) (*plugin.SystemTransformResponse, error) {
			// When PLUGIN_HTTP_PROBE_URL is set, prove
			// InitializeParams.HTTPHeaders (populated by the harness from
			// config plugin_http_headers) actually reaches this plugin: make
			// a real outbound request through c.HTTPClient(), which stamps
			// those headers automatically (see plugin/sdk.go), and let the
			// test's httptest server observe them.
			if probeURL := os.Getenv("PLUGIN_HTTP_PROBE_URL"); probeURL != "" {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
				if err != nil {
					return nil, err
				}
				resp, err := c.HTTPClient().Do(req)
				if err != nil {
					return nil, err
				}
				resp.Body.Close()
			}
			return &plugin.SystemTransformResponse{Segments: []string{marker}}, nil
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "plugin helper:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// helperPluginCommand returns a config.PluginSpec whose Command re-execs
// this test binary as a plugin process (see TestPluginHelperProcess). The
// env vars that select its behavior are set on the *current* process via
// t.Setenv, which the helper inherits either way (dial always appends
// spec.Env on top of os.Environ() — see plugin/host.go); tests that need to
// prove spec.Env specifically reaches the spawned process (as opposed to
// merely the harness's own inherited environment) build the plugin.Spec by
// hand instead — see TestProbeSpecEnvAndDirReachProbedProcess.
func helperPluginCommand(t *testing.T, name string) config.PluginSpec {
	t.Helper()
	return config.PluginSpec{
		Name:    name,
		Command: []string{os.Args[0], "-test.run=^TestPluginHelperProcess$"},
	}
}

func TestBuildPluginHostNoPlugins(t *testing.T) {
	host, err := buildPluginHost(context.Background(), nil, "v", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("buildPluginHost: %v", err)
	}
	if host != nil {
		t.Fatalf("buildPluginHost with no plugins configured = %v, want nil", host)
	}
	// pluginHooks must return a true nil interface, not a typed-nil
	// *plugin.Host wrapped in engine.Hooks (which would make every
	// `s.cfg.Hooks != nil` check in the engine true and then panic).
	if h := pluginHooks(host); h != nil {
		t.Fatalf("pluginHooks(nil) = %v, want nil interface", h)
	}
}

// TestPluginWiringEndToEnd proves the full wiring path: a plugin configured
// exactly as `config.Config.Plugins` would carry it, probed and cached by
// buildPluginHost exactly as run/serve do, wired into engine.Config.Hooks,
// and then a session created the way serveCmd's newSessionFn creates it
// actually dispatches a hook to the real (subprocess) plugin and observes
// its mutation in the request sent to the model.
func TestPluginWiringEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real plugin subprocess")
	}
	tmp := t.TempDir()
	t.Setenv("HARNESS_PLUGIN_CACHE", filepath.Join(tmp, "plugin_cache.json"))
	t.Setenv("GO_WANT_PLUGIN_HELPER", "1")
	t.Setenv("PLUGIN_NAME", "echoplug")
	marker := "hook-fired-42"
	t.Setenv("PLUGIN_MARKER", marker)

	cfg := &config.Config{
		Plugins: []config.PluginSpec{helperPluginCommand(t, "echoplug")},
	}

	ctx := context.Background()
	host, err := buildPluginHost(ctx, cfg.Plugins, "test-version", tmp, nil)
	if err != nil {
		t.Fatalf("buildPluginHost: %v", err)
	}
	if host == nil {
		t.Fatal("buildPluginHost returned nil host with plugins configured")
	}
	t.Cleanup(host.Close)

	prov := &scriptedProvider{name: "test"}
	model := message.ModelRef{Provider: "test", Model: "m1"}
	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:    provider.Registry{"test": prov},
			Model:        m,
			System:       []string{"base system"},
			WorkDir:      tmp,
			Instructions: &engine.InstructionsConfig{Disabled: true},
			SkillsDirs:   []string{},
			Hooks:        pluginHooks(host),
		}
	}
	newSession := newSessionFn(mkCfg, model, cfg, nil, func(string, int, *provider.Request) {})
	sess, err := newSession(message.ModelRef{}, tmp)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	if _, err := sess.Prompt(ctx, "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(prov.requests) == 0 {
		t.Fatal("provider never received a request")
	}
	found := false
	for _, seg := range prov.requests[0].System {
		if seg == marker {
			found = true
		}
	}
	if !found {
		t.Errorf("system segments = %v, want to contain plugin marker %q", prov.requests[0].System, marker)
	}
}

// TestPluginHTTPHeadersWiring proves scope item (4): config's
// plugin_http_headers reaches the plugin's InitializeParams.HTTPHeaders and
// is actually stamped on the plugin's outbound HTTP traffic. It does not add
// new stamping machinery — plugin.Client.HTTPClient() already does the
// stamping (see plugin/sdk.go); this only proves buildPluginHost passes the
// config value through plugin.Options.HTTPHeaders, which host.go already
// forwards into InitializeParams.
func TestPluginHTTPHeadersWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real plugin subprocess")
	}
	tmp := t.TempDir()
	t.Setenv("HARNESS_PLUGIN_CACHE", filepath.Join(tmp, "plugin_cache.json"))
	t.Setenv("GO_WANT_PLUGIN_HELPER", "1")
	t.Setenv("PLUGIN_NAME", "httpplug")

	gotHeaders := make(chan http.Header, 1)
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(probe.Close)
	t.Setenv("PLUGIN_HTTP_PROBE_URL", probe.URL)

	cfg := &config.Config{
		Plugins:           []config.PluginSpec{helperPluginCommand(t, "httpplug")},
		PluginHTTPHeaders: map[string]string{"X-Workspace": "acme-corp"},
	}

	ctx := context.Background()
	host, err := buildPluginHost(ctx, cfg.Plugins, "test-version", tmp, cfg.PluginHTTPHeaders)
	if err != nil {
		t.Fatalf("buildPluginHost: %v", err)
	}
	if host == nil {
		t.Fatal("buildPluginHost returned nil host with plugins configured")
	}
	t.Cleanup(host.Close)

	prov := &scriptedProvider{name: "test"}
	model := message.ModelRef{Provider: "test", Model: "m1"}
	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:    provider.Registry{"test": prov},
			Model:        m,
			System:       []string{"base system"},
			WorkDir:      tmp,
			Instructions: &engine.InstructionsConfig{Disabled: true},
			SkillsDirs:   []string{},
			Hooks:        pluginHooks(host),
		}
	}
	newSession := newSessionFn(mkCfg, model, cfg, nil, func(string, int, *provider.Request) {})
	sess, err := newSession(message.ModelRef{}, tmp)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	if _, err := sess.Prompt(ctx, "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Block directly on the channel the probe handler fills: the plugin's
	// system.transform hook (dispatched synchronously by Prompt above) makes
	// its outbound request before returning, so the value is already there;
	// if the wiring were broken the plugin never calls out and this blocks
	// until the test binary's own timeout catches the hang.
	h := <-gotHeaders
	if got := h.Get("X-Workspace"); got != "acme-corp" {
		t.Errorf("plugin outbound request X-Workspace header = %q, want %q (config plugin_http_headers -> InitializeParams.HTTPHeaders -> Client.HTTPClient())", got, "acme-corp")
	}
}

// TestBuildPluginHostCachesManifest proves buildPluginHost only spawns the
// plugin to probe its manifest once: a second call reading the same
// on-disk cache must not spawn it again.
func TestBuildPluginHostCachesManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real plugin subprocess")
	}
	tmp := t.TempDir()
	cachePath := filepath.Join(tmp, "plugin_cache.json")
	t.Setenv("HARNESS_PLUGIN_CACHE", cachePath)
	t.Setenv("GO_WANT_PLUGIN_HELPER", "1")
	t.Setenv("PLUGIN_NAME", "cacheplug")
	spawnLog := filepath.Join(tmp, "spawns.log")
	t.Setenv("PLUGIN_SPAWN_LOG", spawnLog)

	plugins := []config.PluginSpec{helperPluginCommand(t, "cacheplug")}

	host1, err := buildPluginHost(context.Background(), plugins, "v", tmp, nil)
	if err != nil {
		t.Fatalf("buildPluginHost (1st): %v", err)
	}
	host1.Close()
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("expected cache file to be written: %v", err)
	}
	spawnsAfterFirst := countLines(t, spawnLog)
	if spawnsAfterFirst == 0 {
		t.Fatal("expected the plugin to be probed (spawned) at least once")
	}

	host2, err := buildPluginHost(context.Background(), plugins, "v", tmp, nil)
	if err != nil {
		t.Fatalf("buildPluginHost (2nd): %v", err)
	}
	t.Cleanup(host2.Close)
	spawnsAfterSecond := countLines(t, spawnLog)
	if spawnsAfterSecond != spawnsAfterFirst {
		t.Errorf("2nd buildPluginHost spawned the plugin again: spawns %d -> %d, want unchanged (manifest cache hit)", spawnsAfterFirst, spawnsAfterSecond)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimRight(string(b), "\n"), "\n"))
}

// TestPluginProbeCmd proves `harness plugin probe` re-probes every
// configured plugin, prints its name and hook list, and refreshes the
// on-disk manifest cache.
func TestPluginProbeCmd(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real plugin subprocess")
	}
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_PLUGIN_CACHE", filepath.Join(tmp, "plugin_cache.json"))
	t.Setenv("GO_WANT_PLUGIN_HELPER", "1")
	t.Setenv("PLUGIN_NAME", "probeplug")

	configPath := filepath.Join(tmp, "config.json")
	cmdJSON := `["` + os.Args[0] + `", "-test.run=^TestPluginHelperProcess$"]`
	body := `{"plugins": [{"name": "probeplug", "command": ` + cmdJSON + `}]}`
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_CONFIG", configPath)

	stdout := captureStdout(t, func() {
		if err := pluginCmd([]string{"probe"}); err != nil {
			t.Fatalf("pluginCmd probe: %v", err)
		}
	})
	if !strings.Contains(stdout, "probeplug") {
		t.Errorf("probe output = %q, want it to name the plugin", stdout)
	}
	if !strings.Contains(stdout, "system.transform") {
		t.Errorf("probe output = %q, want it to list the system.transform hook", stdout)
	}
	if _, err := os.Stat(filepath.Join(tmp, "plugin_cache.json")); err != nil {
		t.Errorf("expected plugin probe to refresh the manifest cache: %v", err)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. Used for the plugin probe command's printed output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r) //nolint:errcheck
		done <- buf.String()
	}()
	fn()
	os.Stdout = orig
	w.Close()
	out := <-done
	r.Close()
	return out
}

// TestProbeSpecEnvAndDirReachProbedProcess proves finding (2): probing must
// use the full plugin.Spec (Env, Dir, Config), not just the bare command, so
// the cached manifest matches what the live, fully-configured plugin
// actually advertises. PLUGIN_PROBE_MARKER is set only via spec.Env (never
// via t.Setenv on this process), so it reaches the probed process if and
// only if buildPluginSpecs/plugin.ProbeSpec actually plumb spec.Env through
// — the old plugin.Probe(ctx, command), which spawned with only the
// harness's inherited environment, could never see it. spec.Dir similarly
// must reach the probed process's cwd.
func TestProbeSpecEnvAndDirReachProbedProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real plugin subprocess")
	}
	t.Setenv("GO_WANT_PLUGIN_HELPER", "1")
	t.Setenv("PLUGIN_NAME", "envplug")
	t.Setenv("PLUGIN_ECHO_VAR", "PLUGIN_PROBE_MARKER")

	dir := t.TempDir()
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	spec := plugin.Spec{
		Command: []string{os.Args[0], "-test.run=^TestPluginHelperProcess$"},
		Env:     []string{"PLUGIN_PROBE_MARKER=probe-only-env-9f2c"},
		Dir:     dir,
	}
	m, err := plugin.ProbeSpec(context.Background(), spec)
	if err != nil {
		t.Fatalf("ProbeSpec: %v", err)
	}
	if m.Name != "envplug" {
		t.Fatalf("manifest name = %q, want envplug", m.Name)
	}
	if want := "echo=probe-only-env-9f2c"; !strings.Contains(m.Version, want) {
		t.Errorf("manifest.Version = %q, want it to contain %q (spec.Env did not reach the probed process)", m.Version, want)
	}
	if want := "cwd=" + wantDir; !strings.Contains(m.Version, want) {
		t.Errorf("manifest.Version = %q, want it to contain %q (spec.Dir did not reach the probed process)", m.Version, want)
	}
}

// TestPluginCacheKeyChangesWithBinary proves the cache is keyed by binary
// content (a changed binary must not silently reuse a stale manifest), by
// hashing two different files and checking the keys differ.
func TestPluginBinaryHashDetectsChange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plug")
	if err := os.WriteFile(p, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	h1, err := pluginBinaryHash([]string{p}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	h2, err := pluginBinaryHash([]string{p}, "")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Errorf("hash unchanged after binary content changed: %q", h1)
	}
}

// TestPluginBinaryHashMatchesResolvedExecutable proves finding (1): the
// manifest-cache hash and the actual spawn must resolve a bare command name
// identically. The old implementation hashed whatever os.Stat found relative
// to the *harness process's current directory* before falling back to
// exec.LookPath, while a real spawn of a bare name (no path separator) always
// resolves via PATH — independent of cwd or spec.Dir. Put two different
// files named "plug" in play, one on PATH and one in the process's cwd, and
// prove the cache hashes the PATH one: the same file exec.Command would
// actually run.
func TestPluginBinaryHashMatchesResolvedExecutable(t *testing.T) {
	pathDir := t.TempDir()
	cwdDir := t.TempDir()

	pathBin := filepath.Join(pathDir, "plug")
	if err := os.WriteFile(pathBin, []byte("path-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwdBin := filepath.Join(cwdDir, "plug")
	if err := os.WriteFile(cwdBin, []byte("cwd-version"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", pathDir)
	t.Chdir(cwdDir)

	got, err := pluginBinaryHash([]string{"plug"}, "")
	if err != nil {
		t.Fatal(err)
	}
	want, err := pluginBinaryHash([]string{pathBin}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("pluginBinaryHash(bare name %q) = %s, want hash of PATH-resolved binary %s (hashed the cwd file instead — hash and spawn can diverge)", "plug", got, want)
	}
}

// TestPluginBinaryHashRelativeToDir proves the other half of finding (1): a
// command given as a Dir-relative path (containing a path separator) must
// hash the file that a real spawn resolves relative to spec.Dir — not
// relative to the harness process's own cwd, which is what a real spawn
// never consults for a relative Path once Dir is set (Cmd.Path: "If Path is
// relative, it is evaluated relative to Dir.").
func TestPluginBinaryHashRelativeToDir(t *testing.T) {
	specDir := t.TempDir()
	otherDir := t.TempDir()

	relBin := filepath.Join(specDir, "sub", "plug")
	if err := os.MkdirAll(filepath.Dir(relBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(relBin, []byte("dir-version"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A same-named-but-different file at the same relative path from the
	// harness's own cwd, so a cwd-relative resolution would (wrongly) hash
	// this one instead.
	decoy := filepath.Join(otherDir, "sub", "plug")
	if err := os.MkdirAll(filepath.Dir(decoy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decoy, []byte("decoy-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(otherDir)

	got, err := pluginBinaryHash([]string{"sub/plug"}, specDir)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pluginBinaryHash([]string{relBin}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("pluginBinaryHash(%q, dir=%q) = %s, want hash of spec.Dir-relative file %s (hashed the cwd-relative decoy instead)", "sub/plug", specDir, got, want)
	}
}
