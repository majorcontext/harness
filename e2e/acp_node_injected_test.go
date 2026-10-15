package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// initializeWith sends initialize with the client capabilities.
func initializeWith(t *testing.T, c *acpConn, caps map[string]any) map[string]any {
	t.Helper()
	id := c.request("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": caps, "clientInfo": map[string]any{"name": "e2e", "version": "0"}})
	return c.mustBeResult(c.response(id), "InitializeResponse")
}

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

// injectedNodeConfig is the user config of a page: a codex entry with auth injected and neither base_url nor login_url,
// which proxy.providers of harnessConnect supplies.
func injectedNodeConfig() map[string]any {
	cfg := codexConfig("", false, nil)
	entry := cfg["providers"].(map[string]any)["codex"].(map[string]any)
	delete(entry, "api_key_env")
	delete(entry, "base_url")
	entry["auth"], entry["models_from"] = "injected", "chatgpt"
	return cfg
}

// TestNodeWasmProxyProvidersSignsInThroughTheProbe pins the Injected auth section for the wasm host: proxy.providers
// sets base_url and login_url of an injected entry, the module offers injected-codex to a client with elicitation.url,
// and authenticate ends when the status probe of the server answers 200.
func TestNodeWasmProxyProvidersSignsInThroughTheProbe(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	openai := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{SignedOut: true}})
	env := acpEnv{configPath: writeGoalConfigWith(t, "", injectedNodeConfig()), sessDir: t.TempDir(), wasm: true}
	providers := map[string]any{"codex": map[string]any{"baseURL": openai.URL() + "/backend-api/codex", "loginURL": injectedLoginURL}}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{Connect: map[string]any{"proxy": map[string]any{"providers": providers}}}.withBase(env))
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.fake, c.env = openai.Server, env
	c.dirs["main"] = t.TempDir()
	for _, step := range []acpStep{
		acpInit{version: 1, want: 1, elicitationURL: true, methods: []string{"injected-codex"}},
		acpAuthStart{id: "auth", method: "injected-codex"},
		acpInjectedElicit{as: "login", auth: "auth"},
	} {
		step.run(t, c)
	}
	openai.SignIn()
	for _, step := range []acpStep{
		acpAnswerElicitation{as: "login", action: "accept"},
		acpAuthDone{id: "auth"},
		acpInjectedComplete{as: "login"},
	} {
		step.run(t, c)
	}
	acpCall{method: "authenticate", params: map[string]any{"methodId": "chatgpt"}, code: acpInvalidParams}.run(t, c)
	c.finish(stop)
}

// startChatGPTNode starts the wasm host over a config with an auth chatgpt codex entry and a valid login file that
// codex_auth_file names. model is the default model of the config.
func startChatGPTNode(t *testing.T, model string) (*acpConn, *harnesstest.OpenAI, func()) {
	t.Helper()
	auth := startCodexAuth(t, codexAuth{label: "seed", lifetime: time.Hour})
	openai := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{Credentials: auth.accepts, AccountID: codexAccount})
	anthropic := harnesstest.NewAnthropic(t)
	cfg := codexConfig(openai.URL(), false, nil)
	auth.configure(cfg)
	cfg["model"] = model
	cfg["providers"].(map[string]any)["anthropic"] = map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": anthropic.URL()}
	env := acpEnv{configPath: writeGoalConfigWith(t, "", cfg), sessDir: t.TempDir(), wasm: true}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{}.withBase(env))
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.fake, c.env = anthropic, env
	c.dirs["main"] = t.TempDir()
	return c, openai, func() { c.finish(stop) }
}

// TestNodeWasmListsNoChatGPTModels pins the WebAssembly section: an entry with auth chatgpt is not listed, even when
// codex_auth_file names a valid login.
func TestNodeWasmListsNoChatGPTModels(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	c, _, finish := startChatGPTNode(t, "anthropic/claude-fable-5")
	initializeWith(t, c, map[string]any{})
	if ids := listedModels(t, c); hasPrefixed(ids, "codex/") {
		t.Errorf("codex models are listed on a host with no login file: %v", ids)
	} else if hasPrefixed(ids, "claude-code/") {
		t.Errorf("claude-code models are listed on a host that starts no process: %v", ids)
	}
	finish()
}

// TestNodeWasmRefusesASessionOnAChatGPTEntry pins the WebAssembly section: a request on an entry with auth chatgpt
// makes session/new fail with login_required before any request is sent, and reads no login file.
func TestNodeWasmRefusesASessionOnAChatGPTEntry(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	c, openai, finish := startChatGPTNode(t, "codex/gpt-5.6-sol")
	initializeWith(t, c, map[string]any{})
	acpNew{as: "a", code: acpAuthRequired}.run(t, c)
	if n := len(openai.WireEvents()); n != 0 {
		t.Errorf("the model server got %d requests, want none", n)
	}
	finish()
}
