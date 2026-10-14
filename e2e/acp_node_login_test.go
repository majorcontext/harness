package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// nodeLoginHost is a Node wasm host whose ChatGPT issuer is a scripted double. The login file is the default
// one of the host: auth/codex.json in the store directory.
type nodeLoginHost struct {
	c      *acpConn
	issuer *harnesstest.CodexIssuer
	env    acpEnv
	stop   func()
}

func startNodeLogin(t *testing.T, pending int) *nodeLoginHost {
	t.Helper()
	nodePath(t)
	h := &nodeLoginHost{issuer: harnesstest.NewCodexIssuer(t, harnesstest.CodexIssuerOptions{DevicePending: pending})}
	fake := harnesstest.NewAnthropic(t)
	h.env = acpEnv{configPath: writeGoalConfigWith(t, fake.URL(), map[string]any{"codex_issuer": h.issuer.URL()}), sessDir: t.TempDir(), wasm: true}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{}.withBase(h.env))
	h.stop = stop
	h.c = newACPConn(t, loadACPSchema(t), in, out)
	h.c.fake, h.c.env = fake, h.env
	h.c.dirs["main"] = t.TempDir()
	return h
}

func (h *nodeLoginHost) loginFile() string { return filepath.Join(h.env.sessDir, "auth", "codex.json") }

// initialize sends initialize with the client capabilities and returns its result.
func (h *nodeLoginHost) initialize(t *testing.T, caps map[string]any) map[string]any {
	t.Helper()
	id := h.c.request("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": caps, "clientInfo": map[string]any{"name": "e2e", "version": "0"}})
	return h.c.mustBeResult(h.c.response(id), "InitializeResponse")
}

var urlElicitationCaps = map[string]any{"elicitation": map[string]any{"url": map[string]any{}}}

func authMethodIDs(res map[string]any) []string {
	var ids []string
	list, _ := res["authMethods"].([]any)
	for _, m := range list {
		ids = append(ids, m.(map[string]any)["id"].(string))
	}
	return ids
}

// elicitation waits for the URL elicitation of the sign-in and checks its fields.
func (h *nodeLoginHost) elicitation(t *testing.T, authID int) (elicitationID string) {
	t.Helper()
	f := h.c.awaitMessage(func(m map[string]any) bool { return m["method"] == "elicitation/create" && m["id"] != nil })
	h.c.asked = map[string]any{"login": f["id"]}
	p, _ := f["params"].(map[string]any)
	want := h.issuer.URL() + "/codex/device?user_code=" + harnesstest.CodexUserCode
	elicitationID, _ = p["elicitationId"].(string)
	switch {
	case p["mode"] != "url", p["url"] != want:
		t.Errorf("elicitation mode and url = %v %v, want url %s", p["mode"], p["url"], want)
	case p["requestId"] != float64(authID):
		t.Errorf("elicitation requestId = %v, want the authenticate request %d", p["requestId"], authID)
	case elicitationID == "":
		t.Error("elicitation has no elicitationId")
	case !strings.Contains(p["message"].(string), harnesstest.CodexUserCode):
		t.Errorf("elicitation message = %q, want it to name the code", p["message"])
	}
	if _, ok := p["sessionId"]; ok {
		t.Error("elicitation of a sign-in names a session")
	}
	return elicitationID
}

func TestNodeWasmDeviceLoginStoresTheLoginAndCompletesTheElicitation(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := startNodeLogin(t, 1)
	c := h.c
	res := h.initialize(t, urlElicitationCaps)
	if got := authMethodIDs(res); !slices.Equal(got, []string{"chatgpt"}) {
		t.Errorf("authMethods ids = %v, want [chatgpt]", got)
	}
	if caps, _ := res["agentCapabilities"].(map[string]any); caps["auth"] == nil {
		t.Error("the agent does not advertise logout")
	}
	id := c.request("authenticate", map[string]any{"methodId": "chatgpt"})
	elicitationID := h.elicitation(t, id)
	acpAnswerElicitation{as: "login", action: "accept"}.run(t, c)
	c.mustBeResult(c.response(id), "AuthenticateResponse")
	done := c.awaitMessage(func(m map[string]any) bool { return m["method"] == "elicitation/complete" })
	if p, _ := done["params"].(map[string]any); p["elicitationId"] != elicitationID {
		t.Errorf("completed elicitation = %v, want %s", p["elicitationId"], elicitationID)
	}
	complete, response := -1, -1
	for i, f := range c.wire {
		if f.Dir != "agent" {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(f.Frame, &m)
		switch {
		case m["method"] == "elicitation/complete":
			complete = i
		case m["id"] == float64(id) && m["result"] != nil:
			response = i
		}
	}
	if complete < 0 || response < complete {
		t.Errorf("the completion is at frame %d and the authenticate answer at %d, want the completion first", complete, response)
	}
	raw, err := os.ReadFile(h.loginFile())
	if err != nil {
		t.Fatalf("the sign-in stored no login: %v", err)
	}
	if !strings.Contains(string(raw), "access_token") {
		t.Errorf("login file = %s, want a login", raw)
	}
	var all strings.Builder
	for _, f := range c.wire {
		all.Write(f.Frame)
	}
	if holdsSecret(all.String(), h.issuer.Secrets()) {
		t.Error("a frame holds a token or a code")
	}
	c.finish(h.stop)
}

func TestNodeWasmDeviceLoginStopsOnCancelRequest(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := startNodeLogin(t, 1000)
	c := h.c
	h.initialize(t, urlElicitationCaps)
	id := c.request("authenticate", map[string]any{"methodId": "chatgpt"})
	h.elicitation(t, id)
	c.send(map[string]any{"jsonrpc": "2.0", "method": "$/cancel_request", "params": map[string]any{"requestId": float64(id)}})
	c.mustBeError(c.response(id), -32800)
	acpAwaitCancelRequest{as: "login"}.run(t, c)
	if _, err := os.Stat(h.loginFile()); err == nil {
		t.Error("a cancelled sign-in stored a login")
	}
	c.finish(h.stop)
}

func TestNodeWasmDeviceLoginEndsWhenTheClientDeclinesOrCancelsTheElicitation(t *testing.T) {
	skipShort(t)
	t.Parallel()
	for action, code := range map[string]int{"decline": -32603, "cancel": -32800} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			h := startNodeLogin(t, 1000)
			c := h.c
			h.initialize(t, urlElicitationCaps)
			id := c.request("authenticate", map[string]any{"methodId": "chatgpt"})
			h.elicitation(t, id)
			acpAnswerElicitation{as: "login", action: action}.run(t, c)
			c.mustBeError(c.response(id), code)
			if _, err := os.Stat(h.loginFile()); err == nil {
				t.Errorf("a %s sign-in stored a login", action)
			}
			c.finish(h.stop)
		})
	}
}

func TestNodeWasmOffersNoLoginWithoutURLElicitation(t *testing.T) {
	skipShort(t)
	t.Parallel()
	for name, caps := range map[string]map[string]any{
		"no capability": {},
		"form only":     {"elicitation": map[string]any{"form": map[string]any{}}},
		"url null":      {"elicitation": map[string]any{"url": nil}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := startNodeLogin(t, 0)
			c := h.c
			res := h.initialize(t, caps)
			if got := authMethodIDs(res); len(got) != 0 {
				t.Errorf("authMethods ids = %v, want none", got)
			}
			if agent, _ := res["agentCapabilities"].(map[string]any); agent["auth"] != nil {
				t.Error("the agent advertises logout without a login")
			}
			c.mustBeError(c.response(c.request("authenticate", map[string]any{"methodId": "chatgpt"})), acpMethodNotFound)
			c.mustBeError(c.response(c.request("logout", map[string]any{})), acpMethodNotFound)
			c.finish(h.stop)
		})
	}
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
			h := &nodeLoginHost{c: c}
			h.initialize(t, row.caps)
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
