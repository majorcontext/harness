package e2e

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// subscriptionHost is a cliHost whose user config sets no model. The config holds the anthropic double and extra.
func subscriptionHost(t *testing.T, extra map[string]any) *cliHost {
	t.Helper()
	h := newCLIHost(t, nil, replyText("ok"))
	cfg := map[string]any{"providers": map[string]any{"anthropic": map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": h.fake.URL()}}}
	for k, v := range extra {
		cfg[k] = v
	}
	writeJSON(t, h.config, cfg)
	return h
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// claudeOnPath puts the Claude Code double as `claude` on the only directory of PATH.
func claudeOnPath(t *testing.T, h *cliHost) {
	t.Helper()
	bin := filepath.Join(h.dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fakeClaudePath(), filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	h.env["PATH"] = bin
}

// sessionLog returns the session log text that names the model of the run.
func sessionLog(t *testing.T, h *cliHost) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(h.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(filepath.Dir(p)) == "bin" {
			return err
		}
		raw, err := os.ReadFile(p)
		if err == nil {
			b.Write(raw)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestContractCLIRunDefaultsToTheCodexLoginWhenNoModelIsSet(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := subscriptionHost(t, map[string]any{"providers": map[string]any{
		"codex": map[string]any{"type": "openai", "auth": "chatgpt", "base_url": "http://127.0.0.1:1", "responses_path": "/responses"},
	}})
	h.env = map[string]string{"PATH": t.TempDir(), "ANTHROPIC_API_KEY": ""}
	writeJSON(t, filepath.Join(filepath.Dir(h.config), "auth", "codex.json"), map[string]string{"account_id": "a"})
	_, _, _ = h.run("run", "-p", "hi")
	if log := sessionLog(t, h); !strings.Contains(log, "codex/gpt-") || strings.Contains(log, "anthropic/claude-fable-5") {
		t.Errorf("the run did not pick the codex model:\n%s", log)
	}
}

func TestContractCLIRunDefaultsToClaudeCodeWhenTheProgramIsOnPath(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := subscriptionHost(t, nil)
	h.env = map[string]string{"ANTHROPIC_API_KEY": ""}
	claudeOnPath(t, h)
	_, _, _ = h.run("run", "-p", "hi")
	if log := sessionLog(t, h); !strings.Contains(log, "claude-code/sonnet") || strings.Contains(log, "anthropic/claude-fable-5") {
		t.Errorf("the run did not pick claude-code/sonnet:\n%s", log)
	}
}

func TestContractCLIRunPrefersTheCodexLoginToClaudeCode(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := subscriptionHost(t, map[string]any{"providers": map[string]any{
		"codex": map[string]any{"type": "openai", "auth": "chatgpt", "base_url": "http://127.0.0.1:1", "responses_path": "/responses"},
	}})
	h.env = map[string]string{"ANTHROPIC_API_KEY": ""}
	claudeOnPath(t, h)
	writeJSON(t, filepath.Join(filepath.Dir(h.config), "auth", "codex.json"), map[string]string{"account_id": "a"})
	_, _, _ = h.run("run", "-p", "hi")
	if log := sessionLog(t, h); !strings.Contains(log, "codex/gpt-") || strings.Contains(log, "claude-code/sonnet") {
		t.Errorf("the run did not prefer the codex model:\n%s", log)
	}
}

func TestContractCLIRunKeepsTheAPIDefaultWhenAKeyIsSet(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := subscriptionHost(t, nil)
	h.env = map[string]string{}
	claudeOnPath(t, h)
	writeJSON(t, filepath.Join(filepath.Dir(h.config), "auth", "codex.json"), map[string]string{"account_id": "a"})
	out, errOut, code := h.run("run", "-p", "hi")
	if code != 0 || out != "ok\n" {
		t.Fatalf("run = %d %q %q, want 0 and ok", code, out, errOut)
	}
	if log := sessionLog(t, h); !strings.Contains(log, "anthropic/claude-fable-5") || strings.Contains(log, "claude-code/sonnet") || strings.Contains(log, "codex/gpt-") {
		t.Errorf("a set ANTHROPIC_API_KEY did not keep the API default:\n%s", log)
	}
}

func TestContractCLIRunKeepsTheBuiltInDefaultWithNoSubscription(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := subscriptionHost(t, nil)
	h.env = map[string]string{"PATH": t.TempDir()}
	out, errOut, code := h.run("run", "-p", "hi")
	if code != 0 || out != "ok\n" {
		t.Fatalf("run = %d %q %q, want 0 and ok", code, out, errOut)
	}
	if log := sessionLog(t, h); !strings.Contains(log, "anthropic/claude-fable-5") {
		t.Errorf("the run did not use the built-in default:\n%s", log)
	}
}

func TestContractCLIRunModelFlagAndConfigBeatTheSubscription(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := subscriptionHost(t, map[string]any{"model": "anthropic/claude-opus-5"})
	h.env = map[string]string{}
	claudeOnPath(t, h)
	if _, errOut, code := h.run("run", "-p", "hi"); code != 0 {
		t.Fatalf("run = %d\n%s", code, errOut)
	}
	if log := sessionLog(t, h); !strings.Contains(log, "anthropic/claude-opus-5") || strings.Contains(log, "claude-code/sonnet") {
		t.Errorf("the config model lost to the subscription:\n%s", log)
	}
	h2 := subscriptionHost(t, nil)
	h2.env = map[string]string{}
	claudeOnPath(t, h2)
	if _, errOut, code := h2.run("run", "-model", "anthropic/claude-opus-5", "-p", "hi"); code != 0 {
		t.Fatalf("run -model = %d\n%s", code, errOut)
	}
	if log := sessionLog(t, h2); !strings.Contains(log, "anthropic/claude-opus-5") || strings.Contains(log, "claude-code/sonnet") {
		t.Errorf("the flag lost to the subscription:\n%s", log)
	}
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, raw)
	}
	return m
}

func TestContractCLILoginCodexSavesTheDefaultModel(t *testing.T) {
	skipShort(t)
	t.Parallel()
	issuer := harnesstest.NewCodexIssuer(t, harnesstest.CodexIssuerOptions{})
	h := newCLIHost(t, nil, replyText("unused"))
	writeJSON(t, h.config, map[string]any{"codex_issuer": issuer.URL(), "aliases": map[string]string{"a": "openai/x"}})
	out, errOut, code := h.run("login", "codex", "--device")
	if code != 0 {
		t.Fatalf("login = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "Default model set to codex/gpt-") || !strings.Contains(out, " in "+h.config+".") {
		t.Errorf("login output names no default model:\n%s", out)
	}
	cfg := readConfig(t, h.config)
	if m, _ := cfg["model"].(string); !strings.HasPrefix(m, "codex/gpt-") || cfg["codex_issuer"] != issuer.URL() || cfg["aliases"] == nil {
		t.Errorf("config after login = %v, want the codex model and the other keys", cfg)
	}
	if info, _ := os.Stat(h.config); info.Mode().Perm() != 0o644 {
		t.Errorf("config mode = %v, want the old mode 0644", info.Mode().Perm())
	}
	out, _, _ = h.run("run", "-p", "hi")
	_ = out
	if log := sessionLog(t, h); !strings.Contains(log, "codex/gpt-") {
		t.Errorf("a run after the login did not use the saved model:\n%s", log)
	}
}

func TestContractCLILoginCodexKeepsAModelThatIsSet(t *testing.T) {
	skipShort(t)
	t.Parallel()
	issuer := harnesstest.NewCodexIssuer(t, harnesstest.CodexIssuerOptions{})
	h := newCLIHost(t, nil, replyText("unused"))
	writeJSON(t, h.config, map[string]any{"codex_issuer": issuer.URL(), "model": "openai/gpt-5.5"})
	out, errOut, code := h.run("login", "codex", "--device")
	if code != 0 || strings.Contains(out, "Default model") {
		t.Fatalf("login = %d\n%s%s", code, out, errOut)
	}
	if m := readConfig(t, h.config)["model"]; m != "openai/gpt-5.5" {
		t.Errorf("model = %v, want the model that the user set", m)
	}
}

func TestContractCLILoginStatusWritesNoConfig(t *testing.T) {
	skipShort(t)
	t.Parallel()
	issuer := harnesstest.NewCodexIssuer(t, harnesstest.CodexIssuerOptions{})
	h := newCLIHost(t, nil, replyText("unused"))
	writeJSON(t, h.config, map[string]any{"codex_issuer": issuer.URL()})
	if _, errOut, code := h.run("login", "codex", "--device"); code != 0 {
		t.Fatalf("login = %d\n%s", code, errOut)
	}
	writeJSON(t, h.config, map[string]any{"codex_issuer": issuer.URL()})
	if _, errOut, code := h.run("login", "codex", "--status"); code != 0 {
		t.Fatalf("status = %d\n%s", code, errOut)
	}
	if _, ok := readConfig(t, h.config)["model"]; ok {
		t.Error("--status wrote a model")
	}
}

func TestContractCLILoginClaudeCodeSavesTheDefaultModelAfterASignIn(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := newCLIHost(t, nil)
	writeJSON(t, h.config, map[string]any{"providers": map[string]any{"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": fakeClaudePath()}}})
	state := filepath.Join(h.dir, "state")
	h.env = map[string]string{"FAKE_CLAUDE_AUTH": state}
	if err := os.WriteFile(state, []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := h.run("login", "claude-code", "--status"); code != 1 {
		t.Fatalf("status before login = %d, want 1", code)
	}
	if _, ok := readConfig(t, h.config)["model"]; ok {
		t.Error("--status wrote a model")
	}
	out, errOut, code := h.run("login", "claude-code")
	if code != 0 || !strings.Contains(out, "Default model set to claude-code/sonnet in "+h.config+".") {
		t.Fatalf("login = %d\n%s%s", code, out, errOut)
	}
	cfg := readConfig(t, h.config)
	if cfg["model"] != "claude-code/sonnet" || cfg["providers"] == nil {
		t.Errorf("config after login = %v, want the model and the providers", cfg)
	}
}

func TestContractCLILoginClaudeCodeSavesNoModelWhenTheCLIIsNotSignedIn(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := newCLIHost(t, nil)
	writeJSON(t, h.config, map[string]any{"providers": map[string]any{"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": fakeClaudePath()}}})
	state := filepath.Join(h.dir, "state")
	h.env = map[string]string{"FAKE_CLAUDE_AUTH": state, "FAKE_CLAUDE_AUTH_LOGIN_KEEPS_OUT": "1"}
	if err := os.WriteFile(state, []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := h.run("login", "claude-code"); code != 0 {
		t.Fatalf("login = %d, want 0", code)
	}
	if _, ok := readConfig(t, h.config)["model"]; ok {
		t.Error("login wrote a model although status reports loggedIn false")
	}
}
