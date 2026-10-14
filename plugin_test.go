package harness_test

import (
	"cmp"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// pluginFixture builds the wire-level plugin of the contract suite.
func pluginFixture(t *testing.T, cfg string) []config.PluginSpec {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pluginfixture")
	if out, err := exec.Command("go", "build", "-o", bin, "./harnesstest/pluginfixture").CombinedOutput(); err != nil {
		t.Fatalf("go build pluginfixture: %v\n%s", err, out)
	}
	return []config.PluginSpec{{Name: "fixture", Command: []string{bin}, Config: []byte(cfg)}}
}

func pluginRuntime(t *testing.T, dir string, plugins []config.PluginSpec, tools []harness.Tool, steps ...harnesstest.Step) (*harness.Runtime, *harnesstest.OpenAI) {
	t.Helper()
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...)
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	r, err := harness.New(harness.Options{Getenv: os.Getenv, Store: store.NewMemStore(), Tools: tools, WorkDir: dir, Config: config.Config{
		Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
			BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}},
			"claude-code": {Type: config.TypeClaudeCodeCLI}},
		Plugins: plugins}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	return r, s
}

func TestCreateStartsThePlugins(t *testing.T) {
	fixture := pluginFixture(t, `{}`)
	for _, tc := range []struct {
		name    string
		plugins []config.PluginSpec
		tools   []harness.Tool
		model   string
		allowed []string
		want    string
	}{
		{name: "a missing executable fails", plugins: []config.PluginSpec{{Name: "gone", Command: []string{"/nonexistent/plugin"}}}, want: "plugin gone"},
		{name: "a manifest of another name fails", plugins: []config.PluginSpec{{Name: "other", Command: fixture[0].Command}}, want: `manifest name "fixture"`},
		{name: "a tool name that an embedder tool has fails", plugins: fixture, tools: []harness.Tool{newProbe("fixture_echo", false)}, want: `tool name "fixture_echo"`},
		{name: "a plugin tool with the name of a built-in tool fails", plugins: []config.PluginSpec{{Name: "fixture", Command: fixture[0].Command, Config: []byte(`{"extra_tool":"AskUserQuestion"}`)}},
			model: "claude-code/opus", want: `built-in tool`},
		{name: "a backend that owns the loop allows a plugin tool", plugins: fixture, model: "claude-code/opus", allowed: []string{"fixture_echo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := pluginRuntime(t, "", tc.plugins, tc.tools)
			_, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: cmp.Or(tc.model, "codex/gpt-5"), AllowedTools: tc.allowed})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("Create = %v, want an error that names %q", err, tc.want)
			}
		})
	}
}

func TestCreateAfterCloseDoesNotProbeThePlugins(t *testing.T) {
	r, _ := pluginRuntime(t, "", []config.PluginSpec{{Name: "gone", Command: []string{"/nonexistent/plugin"}}}, nil)
	closeRuntime(t, r)
	if _, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"}); !errors.Is(err, harness.ErrDraining) {
		t.Errorf("Create after Close = %v, want ErrDraining", err)
	}
}

func TestInvalidManifestIsNotCached(t *testing.T) {
	fixture := pluginFixture(t, `{"deny_hook":"tool.execute.after"}`)
	cache := filepath.Join(t.TempDir(), "plugins.json")
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{})
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	r, err := harness.New(harness.Options{Getenv: os.Getenv, Store: store.NewMemStore(), Config: config.Config{
		PluginCache: cache, Plugins: fixture,
		Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY", BaseURL: s.URL()}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime(t, r)
	if _, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"}); err == nil || !strings.Contains(err.Error(), "deny_on_failure is valid only for") {
		t.Fatalf("Create = %v, want the manifest error", err)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache file stat = %v, want no cache file for an invalid manifest", err)
	}
}
