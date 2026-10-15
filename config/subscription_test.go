package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathWith puts a program named claude, or nothing, on a PATH of its own.
func pathWith(t *testing.T, claude bool) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	if claude {
		if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func loginFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codex.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultModelFollowsTheSubscriptionInUse(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.json")
	rows := []struct {
		name   string
		cfg    Config
		claude bool
		login  string
		want   string
		off    bool
	}{
		{"codex login file and claude, subscription default off", Config{}, true, `{"a":1}`, DefaultModel, true},
		{"claude only, subscription default off", Config{}, true, "-", DefaultModel, true},
		{"codex login file", Config{}, true, `{"a":1}`, SubscriptionCodexModel, false},
		{"empty codex login file", Config{}, false, "", DefaultModel, false},
		{"empty codex login file with claude", Config{}, true, "", SubscriptionClaudeModel, false},
		{"claude on PATH", Config{}, true, "-", SubscriptionClaudeModel, false},
		{"neither", Config{}, false, "-", DefaultModel, false},
		{"user or project model", Config{Model: "openai/gpt-5.5"}, true, `{"a":1}`, "openai/gpt-5.5", false},
		{"codex entry replaced by an API key entry", Config{Providers: map[string]Provider{"codex": {Type: TypeOpenAI, APIKeyEnv: "K"}}}, false, `{"a":1}`, DefaultModel, false},
		{"claude-code entry replaced by a non-CLI entry", Config{Providers: map[string]Provider{"claude-code": {Type: TypeOpenAI, APIKeyEnv: "K"}}}, true, "-", DefaultModel, false},
		{"claude-code entry names a program that is missing", Config{Providers: map[string]Provider{"claude-code": {Type: TypeClaudeCodeCLI, BinaryPath: "/nonexistent/claude"}}}, true, "-", DefaultModel, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			pathWith(t, r.claude)
			cfg := r.cfg
			cfg.SubscriptionDefault = !r.off
			switch r.login {
			case "-":
				cfg.CodexAuthFile = missing
			default:
				cfg.CodexAuthFile = loginFile(t, r.login)
			}
			if got := cfg.ResolveModel(""); got != r.want {
				t.Errorf("ResolveModel(\"\") = %q, want %q", got, r.want)
			}
		})
	}
}

func TestDefaultModelKeepsTheAPIDefaultWhenItsKeyIsSet(t *testing.T) {
	pathWith(t, true)
	cfg := Config{SubscriptionDefault: true, CodexAuthFile: loginFile(t, `{"a":1}`)}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if got := cfg.ResolveModel(""); got != DefaultModel {
		t.Errorf("with ANTHROPIC_API_KEY = %q, want %q", got, DefaultModel)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if got := cfg.ResolveModel(""); got != SubscriptionCodexModel {
		t.Errorf("without ANTHROPIC_API_KEY = %q, want %q", got, SubscriptionCodexModel)
	}
	cfg.Providers = map[string]Provider{"anthropic": {APIKeyEnv: "WORK_KEY"}}
	t.Setenv("WORK_KEY", "sk-work")
	if got := cfg.ResolveModel(""); got != DefaultModel {
		t.Errorf("with the api_key_env of the anthropic entry = %q, want %q", got, DefaultModel)
	}
}

func TestDefaultModelNeedsNoLoginFileKey(t *testing.T) {
	pathWith(t, false)
	if got := (&Config{}).ResolveModel(""); got != DefaultModel {
		t.Errorf("no codex_auth_file and no claude = %q, want %q", got, DefaultModel)
	}
	if got := (*Config)(nil).ResolveModel(""); got != DefaultModel {
		t.Errorf("nil config = %q, want %q", got, DefaultModel)
	}
}

func TestExplicitModelBeatsTheSubscriptionDefault(t *testing.T) {
	pathWith(t, true)
	cfg := Config{SubscriptionDefault: true, CodexAuthFile: loginFile(t, `{"a":1}`), Aliases: map[string]string{"fast": "openai/gpt-5.5"}}
	if got := cfg.ResolveModel("anthropic/claude-sonnet-5-5"); got != "anthropic/claude-sonnet-5-5" {
		t.Errorf("flag model = %q", got)
	}
	if got := cfg.ResolveModel("fast"); got != "openai/gpt-5.5" {
		t.Errorf("alias = %q", got)
	}
	if err := cfg.ApplyEnv(func(k string) string {
		if k == "HARNESS_MODEL" {
			return "openai/gpt-5.5"
		}
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ResolveModel(""); got != "openai/gpt-5.5" {
		t.Errorf("env model = %q", got)
	}
}

func TestDefaultModelDoesNotChangeTheConfig(t *testing.T) {
	pathWith(t, true)
	cfg := Config{SubscriptionDefault: true, CodexAuthFile: loginFile(t, `{"a":1}`)}
	_ = cfg.ResolveModel("")
	if cfg.Model != "" {
		t.Errorf("Model = %q, want it unset", cfg.Model)
	}
}

func TestSetModelWritesTheModelAndKeepsEveryOtherKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"codex_issuer":"http://127.0.0.1:1","aliases":{"a":"openai/x"},"web":{"fetch":true}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	set, err := SetModel(path, SubscriptionCodexModel)
	if err != nil || !set {
		t.Fatalf("SetModel = %v, %v, want true", set, err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != SubscriptionCodexModel || cfg.CodexIssuer != "http://127.0.0.1:1" || cfg.Aliases["a"] != "openai/x" || cfg.Web.Fetch == nil || !*cfg.Web.Fetch {
		t.Errorf("config after SetModel = %+v", cfg)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want the old mode 0640", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want the config alone", len(entries))
	}
}

func TestSetModelCreatesAMissingFileWithMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "config.json")
	if set, err := SetModel(path, SubscriptionClaudeModel); err != nil || !set {
		t.Fatalf("SetModel = %v, %v, want true", set, err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Model != SubscriptionClaudeModel {
		t.Fatalf("Load = %+v, %v", cfg, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSetModelKeepsAModelThatIsSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const body = `{"model":"openai/gpt-5.5"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if set, err := SetModel(path, SubscriptionCodexModel); err != nil || set {
		t.Fatalf("SetModel = %v, %v, want false and no error", set, err)
	}
	if got, _ := os.ReadFile(path); string(got) != body {
		t.Errorf("file = %q, want it untouched", got)
	}
}

func TestSetModelLeavesAFileThatDoesNotParse(t *testing.T) {
	for name, body := range map[string]string{"bad JSON": `{"model":`, "unknown key": `{"nope":1}`} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			set, err := SetModel(path, SubscriptionCodexModel)
			if err == nil || set || !strings.Contains(err.Error(), path) {
				t.Fatalf("SetModel = %v, %v, want an error that names the file", set, err)
			}
			if got, _ := os.ReadFile(path); string(got) != body {
				t.Errorf("file = %q, want it untouched", got)
			}
		})
	}
}

func TestSetModelKeepsTheTextOfTheFile(t *testing.T) {
	rows := []struct{ name, in, want string }{
		{"indented", "{\n  \"max_tokens\": 1,\n  \"web\": {\"fetch\": true}\n}\n", "{\n  \"model\": \"M\",\n  \"max_tokens\": 1,\n  \"web\": {\"fetch\": true}\n}\n"},
		{"one line", `{"max_tokens":1}`, `{"model": "M","max_tokens":1}`},
		{"empty object", "{}\n", "{\"model\": \"M\"}\n"},
		{"empty file", " \n", "{\n  \"model\": \"M\"\n}\n"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(r.in), 0o600); err != nil {
				t.Fatal(err)
			}
			if set, err := SetModel(path, "M"); err != nil || !set {
				t.Fatalf("SetModel = %v, %v", set, err)
			}
			if got, _ := os.ReadFile(path); string(got) != r.want {
				t.Errorf("file = %q, want %q", got, r.want)
			}
		})
	}
}

func TestSetModelReplacesAnEmptyModelKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"model":"","aliases":{"a":"openai/x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if set, err := SetModel(path, SubscriptionClaudeModel); err != nil || !set {
		t.Fatalf("SetModel = %v, %v", set, err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Model != SubscriptionClaudeModel || cfg.Aliases["a"] != "openai/x" {
		t.Errorf("config = %+v, %v", cfg, err)
	}
}

func TestSetModelWritesThroughASymlinkAndRefusesADanglingOne(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(`{"web":{"fetch":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if set, err := SetModel(link, "M"); err != nil || !set {
		t.Fatalf("SetModel through a link = %v, %v", set, err)
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Error("the link became a regular file")
	}
	if cfg, _ := Load(target); cfg == nil || cfg.Model != "M" {
		t.Errorf("target = %+v, want the model", cfg)
	}
	dangling := filepath.Join(dir, "dangling.json")
	if err := os.Symlink(filepath.Join(dir, "missing.json"), dangling); err != nil {
		t.Fatal(err)
	}
	if set, err := SetModel(dangling, "M"); err == nil || set {
		t.Fatalf("SetModel on a dangling link = %v, %v, want an error", set, err)
	}
	if info, _ := os.Lstat(dangling); info.Mode()&os.ModeSymlink == 0 {
		t.Error("the dangling link became a regular file")
	}
}
