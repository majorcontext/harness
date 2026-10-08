package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func pluginTools(t *testing.T, h host, cfg map[string]any) []string {
	t.Helper()
	d, fake := startOn(t, h, runtimeWorkdir(t, nil), cfg, replyText("ok"))
	id := d.Create(t)
	d.Submit(t, id, "go")
	d.WaitIdle(t, id)
	return fake.Requests()[0].Tools
}

func rewriteFile(t *testing.T, path, from, to string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), from) {
		t.Fatalf("%s does not hold %q:\n%s", path, from, b)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(b), from, to)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestContractPluginManifestCache(t *testing.T) {
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_cached_manifest_replaces_the_probe_until_the_plugin_changes", func(t *testing.T) {
			cfg := pluginBoxesConfig(t)
			cache := filepath.Join(t.TempDir(), "plugin_cache.json")
			cfg["plugin_cache"] = cache
			script := cfg["plugins"].([]any)[0].(map[string]any)["command"].([]string)[1]

			if got := pluginTools(t, h, cfg); !slices.Contains(got, "fixture_echo") {
				t.Fatalf("the first session has the tools %v, want the tools of the probed manifest", got)
			}
			rewriteFile(t, cache, "fixture_echo", "fixture_cached")
			if got := pluginTools(t, h, cfg); !slices.Contains(got, "fixture_cached") || slices.Contains(got, "fixture_echo") {
				t.Errorf("a plugin that did not change has the tools %v, want the tools of the cached manifest", got)
			}
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(script, later, later); err != nil {
				t.Fatal(err)
			}
			if got := pluginTools(t, h, cfg); !slices.Contains(got, "fixture_cached") {
				t.Errorf("a plugin whose script was touched has the tools %v, want the tools of the cached manifest", got)
			}
			if err := os.WriteFile(script, []byte("// the script changed, and so did its size\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := pluginTools(t, h, cfg); !slices.Contains(got, "fixture_echo") || slices.Contains(got, "fixture_cached") {
				t.Errorf("a plugin whose script changed has the tools %v, want the tools of a new probe", got)
			}
			if b, _ := os.ReadFile(cache); strings.Contains(string(b), "fixture_cached") {
				t.Errorf("the cache keeps the stale manifest:\n%s", b)
			}
		})
	})
}

func TestContractCLIPluginProbeRefreshesTheCache(t *testing.T) {
	skipShort(t)
	t.Parallel()
	cfg := pluginBoxesConfig(t)
	cache := filepath.Join(t.TempDir(), "plugin_cache.json")
	cfg["plugin_cache"] = cache
	h := newCLIHost(t, cfg)
	if _, errOut, code := h.run("plugin", "probe"); code != 0 {
		t.Fatalf("plugin probe = %d\n%s", code, errOut)
	}
	rewriteFile(t, cache, "fixture_echo", "fixture_cached")
	if _, errOut, code := h.run("plugin", "probe"); code != 0 {
		t.Fatalf("plugin probe = %d\n%s", code, errOut)
	}
	if b, _ := os.ReadFile(cache); strings.Contains(string(b), "fixture_cached") || !strings.Contains(string(b), "fixture_echo") {
		t.Errorf("plugin probe left the cache as\n%s", b)
	}
}
