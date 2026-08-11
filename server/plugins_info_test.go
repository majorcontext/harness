package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// pluginsHarness builds a server whose sessions carry the given plugin.Host,
// so GET /session/{id} reports the configured plugins. A nil host wires no
// hooks, exercising the plugin-less path.
func pluginsHarness(t *testing.T, host *plugin.Host) *harness {
	t.Helper()
	const token = "secret-run-token"
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.NewSession = func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
			if m.IsZero() {
				m = message.ModelRef{Provider: prov.Name(), Model: "m1"}
			}
			cfg := engine.Config{
				Providers:     provider.Registry{prov.Name(): prov},
				Model:         m,
				SessionDir:    dir,
				WorkDir:       workDir,
				ParentSession: parentSession,
			}
			// Leave cfg.Hooks a nil interface when no host is configured,
			// exactly as production does — a typed-nil *plugin.Host would make
			// the interface non-nil and defeat every s.cfg.Hooks == nil guard.
			if host != nil {
				cfg.Hooks = host
			}
			return engine.NewSession(cfg), nil
		}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

// sessionPluginsJSON decodes just the plugins array from a GET /session
// response.
type sessionPluginsJSON struct {
	Plugins []plugin.Info `json:"plugins"`
}

// TestGetSessionReportsConfiguredPlugins verifies GET /session/{id} surfaces
// the configured plugins — name, spawn state, tools, hooks — through the real
// handler (httptest), not a hand-built struct.
func TestGetSessionReportsConfiguredPlugins(t *testing.T) {
	spec := plugin.NewTestSpec("guard", &plugin.Hooks{
		ChatParams: func(context.Context, *plugin.Client, *plugin.ChatParamsRequest) (*plugin.ChatParamsResponse, error) {
			return nil, nil
		},
		Tools: []plugin.Tool{{
			Def: plugin.ToolDef{Name: "scan_file", Description: "d", InputSchema: json.RawMessage(`{}`)},
			Execute: func(context.Context, *plugin.Client, json.RawMessage) (message.Parts, error) {
				return nil, nil
			},
		}},
	})
	host, err := plugin.NewHost(plugin.Options{}, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)

	h := pluginsHarness(t, host)
	id := h.createSession("")

	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session status %d: %s", resp.StatusCode, data)
	}
	var got sessionPluginsJSON
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding session %q: %v", data, err)
	}
	if len(got.Plugins) != 1 {
		t.Fatalf("plugins = %+v, want exactly one", got.Plugins)
	}
	p := got.Plugins[0]
	if p.Name != "guard" {
		t.Errorf("plugin name = %q, want guard", p.Name)
	}
	// Lazy spawn: the GET must not have spawned the plugin process.
	if p.State != plugin.PluginNotSpawned {
		t.Errorf("plugin state = %q, want %q", p.State, plugin.PluginNotSpawned)
	}
	// Surplus direction: tools and hooks are actually listed, not just the name.
	if len(p.Tools) != 1 || p.Tools[0] != "scan_file" {
		t.Errorf("plugin tools = %v, want [scan_file]", p.Tools)
	}
	if len(p.Hooks) != 1 || p.Hooks[0] != "chat.params" {
		t.Errorf("plugin hooks = %v, want [chat.params]", p.Hooks)
	}
}

// TestGetSessionPluginlessEmptyArray verifies a session with no plugin host
// reports plugins as an empty JSON array (never null), and the handler does
// not panic reading it.
func TestGetSessionPluginlessEmptyArray(t *testing.T) {
	h := pluginsHarness(t, nil)
	id := h.createSession("")

	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session status %d: %s", resp.StatusCode, data)
	}
	// Assert the literal array, not the decoded slice: null decodes to an
	// empty slice too, so only the raw bytes prove [] versus null.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decoding session %q: %v", data, err)
	}
	pluginsField, ok := raw["plugins"]
	if !ok {
		t.Fatalf("session JSON has no plugins field: %s", data)
	}
	if string(pluginsField) != "[]" {
		t.Errorf("plugins field = %s, want []", pluginsField)
	}
}
