package e2e

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// initializeWith sends initialize with the client capabilities.
func initializeWith(t *testing.T, c *acpConn, caps map[string]any) {
	t.Helper()
	id := c.request("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": caps, "clientInfo": map[string]any{"name": "e2e", "version": "0"}})
	c.mustBeResult(c.response(id), "InitializeResponse")
}

var urlElicitationCaps = map[string]any{"elicitation": map[string]any{"url": map[string]any{}}}

// listedModels returns the model ids of the model option of a new session.
func listedModels(t *testing.T, c *acpConn) []string {
	t.Helper()
	id := c.request("session/new", map[string]any{"cwd": c.dir(""), "mcpServers": []any{}})
	res := c.mustBeResult(c.response(id), "NewSessionResponse")
	c.awaitMessage(func(m map[string]any) bool { return m["method"] == "session/update" })
	var ids []string
	opts, _ := res["configOptions"].([]any)
	for _, o := range opts {
		opt := o.(map[string]any)
		if opt["id"] != "model" {
			continue
		}
		for _, m := range opt["options"].([]any) {
			entry := m.(map[string]any)
			if v, ok := entry["value"].(string); ok {
				ids = append(ids, v)
				continue
			}
			for _, g := range entry["options"].([]any) {
				ids = append(ids, g.(map[string]any)["value"].(string))
			}
		}
	}
	return ids
}

func hasPrefixed(ids []string, prefix string) bool {
	return slices.ContainsFunc(ids, func(id string) bool { return strings.HasPrefix(id, prefix) })
}

// TestNodeWasmListsCodexModelsOnlyWithALogin pins the model list of a host that cannot start a process:
// codex models are listed when the host offers the login or a login file exists, and claude-code models never.
func TestNodeWasmListsCodexModelsOnlyWithALogin(t *testing.T) {
	skipShort(t)
	t.Parallel()
	rows := []struct {
		name      string
		caps      map[string]any
		file      bool
		wantCodex bool
	}{
		{"no login and no file", map[string]any{}, false, false},
		{"login offered", urlElicitationCaps, false, true},
		{"login file", map[string]any{}, true, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			nodePath(t)
			auth := startCodexAuth(t, codexAuth{})
			if row.file {
				auth.write(t, "seed", time.Hour, harnesstest.CodexInitialRefresh)
			}
			openai := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{Credentials: auth.accepts, AccountID: codexAccount})
			anthropic := harnesstest.NewAnthropic(t)
			cfg := codexConfig(openai.URL(), false, nil)
			auth.configure(cfg)
			cfg["model"] = "anthropic/claude-fable-5"
			cfg["providers"].(map[string]any)["anthropic"] = map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": anthropic.URL()}
			env := acpEnv{configPath: writeGoalConfigWith(t, "", cfg), sessDir: t.TempDir(), wasm: true}
			in, out, stop := startACPNodeOpts(t, nodeHostOptions{}.withBase(env))
			c := newACPConn(t, loadACPSchema(t), in, out)
			c.fake, c.env = anthropic, env
			c.dirs["main"] = t.TempDir()
			initializeWith(t, c, row.caps)
			ids := listedModels(t, c)
			if got := hasPrefixed(ids, "codex/"); got != row.wantCodex {
				t.Errorf("codex models listed = %v, want %v (%v)", got, row.wantCodex, ids)
			}
			if hasPrefixed(ids, "claude-code/") {
				t.Errorf("claude-code models are listed on a host that starts no process: %v", ids)
			}
			if !hasPrefixed(ids, "anthropic/") {
				t.Errorf("anthropic models are not listed: %v", ids)
			}
			c.finish(stop)
		})
	}
}

// TestNodeWasmOffersNoLoginForAnAuthFileWithoutAStoreDirectory pins that the login needs the store of the host,
// which holds the file under its lock: a codex_auth_file in the config does not stand for it.
func TestNodeWasmOffersNoLoginForAnAuthFileWithoutAStoreDirectory(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	fake := harnesstest.NewAnthropic(t)
	authFile := filepath.Join(t.TempDir(), "auth", "codex.json")
	env := acpEnv{configPath: writeGoalConfigWith(t, fake.URL(), map[string]any{"codex_auth_file": authFile}), wasm: true}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{ConfigPath: env.configPath, Env: map[string]string{"ANTHROPIC_API_KEY": acpDummyKey}})
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.fake, c.env = fake, env
	id := c.request("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": urlElicitationCaps, "clientInfo": map[string]any{"name": "e2e", "version": "0"}})
	res := c.mustBeResult(c.response(id), "InitializeResponse")
	if methods, _ := res["authMethods"].([]any); len(methods) != 0 {
		t.Errorf("authMethods = %v, want none without a store directory", methods)
	}
	c.mustBeError(c.response(c.request("authenticate", map[string]any{"methodId": "chatgpt"})), acpMethodNotFound)
	c.finish(stop)
}
