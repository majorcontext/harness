package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// requireWindowHarness builds a server whose sessions run with
// engine.Config.RequireContextWindow — the product default `harness serve`
// sets (config key context_window_required) — over two providers: one whose
// models the context-window registry knows, one whose models it cannot.
func requireWindowHarness(t *testing.T) *harness {
	t.Helper()
	const token = "secret-run-token"
	dir := t.TempDir()
	known := &scriptedProvider{name: "openai"}
	unknown := &scriptedProvider{name: "openrouter"}
	srv := newServer(t, dir, known, 0, func(o *Options) {
		o.NewSession = func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
			if m.IsZero() {
				m = message.ModelRef{Provider: "openai", Model: "gpt-5.6-sol"}
			}
			return engine.NewSession(engine.Config{
				Providers:            provider.Registry{"openai": known, "openrouter": unknown},
				Model:                m,
				SessionDir:           dir,
				WorkDir:              workDir,
				ParentSession:        parentSession,
				RequireContextWindow: true,
			}), nil
		}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

// TestCreateRefusesModelWithNoKnownContextWindow: a session that can never
// run one prompt must not look created. Before this, POST /session happily
// returned 201 for a model the registry does not know, and that session ran
// with no context management at all until it died of context exhaustion.
func TestCreateRefusesModelWithNoKnownContextWindow(t *testing.T) {
	h := requireWindowHarness(t)

	resp, data := h.do("POST", "/session", map[string]any{"model": "openrouter/anthropic/claude-opus-4.1"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /session = %d, want 400: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "openrouter/anthropic/claude-opus-4.1") {
		t.Errorf("error body = %s, want it to name the offending model ref", data)
	}
	if !strings.Contains(string(data), "context window") {
		t.Errorf("error body = %s, want it to say what is missing", data)
	}

	// Nothing was created.
	resp, data = h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session = %d: %s", resp.StatusCode, data)
	}
	var list []sessionJSON
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("listing = %s, want no session created by the refused request", data)
	}

	// A model the registry knows is unaffected.
	resp, data = h.do("POST", "/session", map[string]any{"model": "openai/gpt-5.6-sol"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /session (known model) = %d, want 201: %s", resp.StatusCode, data)
	}
}

// TestSetModelRefusesModelWithNoKnownContextWindow covers the other point
// where a session starts calling a model. The refusal must precede the
// swap: SetModel persists a durable model record, so a rejected ref must
// never reach it.
func TestSetModelRefusesModelWithNoKnownContextWindow(t *testing.T) {
	h := requireWindowHarness(t)
	resp, data := h.do("POST", "/session", map[string]any{"model": "openai/gpt-5.6-sol"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /session = %d: %s", resp.StatusCode, data)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}

	resp, data = h.do("POST", "/session/"+created.ID+"/model", map[string]any{"model": "openrouter/anthropic/claude-opus-4.1"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /session/{id}/model = %d, want 400: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "openrouter/anthropic/claude-opus-4.1") {
		t.Errorf("error body = %s, want it to name the offending model ref", data)
	}

	// The swap did not happen.
	resp, data = h.do("GET", "/session/"+created.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session/{id} = %d: %s", resp.StatusCode, data)
	}
	if got := decodeSession(t, data).Model.String(); got != "openai/gpt-5.6-sol" {
		t.Errorf("model = %q, want the refused swap to have changed nothing", got)
	}
}
