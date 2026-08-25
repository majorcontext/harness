//go:build live

// Live reasoning-effort tests. They drive the REAL server endpoint
// (POST /session/{id}/thinking, then a real prompt) against a real provider
// gateway (Bifrost), NOT a hand-built request — the same path a boxes user
// takes. They are build-tagged `live` so `go test -race ./...` (CI, no keys)
// never runs them.
//
// Run (from inside the boxes cluster, or any host on Bifrost's allowlisted
// egress IP — Bifrost authorizes by SOURCE IP, so an off-cluster host gets a
// Cloudflare Access 302 and the test SKIPS):
//
//	HARNESS_LIVE=1 go test -tags live -run TestThinkingLive ./server/
//
// Env:
//
//	HARNESS_LIVE=1          required, or every case skips
//	BIFROST_API_KEY=...     optional; defaults to the ip-bypass placeholder
//	BIFROST_BASE=...        optional; defaults to https://bifrost.meetneptune.dev
//
// The base URLs mirror the box's own .harness.json exactly (see the boxes
// images/shared/box-runtime.sh harness_write_box_config): the "anthropic"
// native route at BASE/anthropic, the "bifrost" openai-compat route at BASE/v1,
// and the "openai" native Responses route at BASE (the adapter appends
// /v1/responses).
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/internal/testpoll"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
	"github.com/majorcontext/harness/provider/openai"
	"github.com/majorcontext/harness/provider/openaicompat"
)

const liveToken = "live-run-token"

// liveEnabled reports whether the live suite should run, skipping otherwise.
func liveEnabled(t *testing.T) string {
	t.Helper()
	if os.Getenv("HARNESS_LIVE") == "" {
		t.Skip("HARNESS_LIVE unset; skipping live reasoning-effort test")
	}
	base := os.Getenv("BIFROST_BASE")
	if base == "" {
		base = "https://bifrost.meetneptune.dev"
	}
	return strings.TrimRight(base, "/")
}

// newLiveHarness builds a real server wired to Bifrost's three provider routes,
// exactly as a box's .harness.json does.
func newLiveHarness(t *testing.T, base string) *harness {
	t.Helper()
	key := os.Getenv("BIFROST_API_KEY")
	if key == "" {
		key = "ip-bypass-no-key"
	}
	dir := t.TempDir()
	reg := provider.Registry{
		anthropic.Family: &anthropic.Client{APIKey: key, BaseURL: base + "/anthropic"},
		openai.Family:    &openai.Client{APIKey: key, BaseURL: base},
		"bifrost":        &openaicompat.Client{Family: "bifrost", APIKey: key, BaseURL: base + "/v1"},
	}
	var srv *Server
	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:  reg,
			Model:      m,
			SessionDir: dir,
			MaxTokens:  20000, // room above the high thinking budget
			OnEvent:    func(ev engine.Event) { srv.Publish(ev) },
		}
	}
	opts := Options{
		SessionDir: dir,
		RunToken:   liveToken,
		Version:    "live",
		NewSession: func(m message.ModelRef, workDir, parent string) (*engine.Session, error) {
			cfg := mkCfg(m)
			cfg.WorkDir = workDir
			cfg.ParentSession = parent
			return engine.NewSession(cfg), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(mkCfg(message.ModelRef{}), id)
		},
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: liveToken, srv: srv, ts: ts}
}

// waitIdleLive waits until GET /session reports idle. The turn it waits
// for is a real call to a remote model over the network, so the state it
// observes is genuinely out of this process and no in-process channel can
// report it: this goes through testpoll, the shared cross-process poll
// helper (see that package's doc comment).
func (h *harness) waitIdleLive(id string) {
	h.t.Helper()
	testpoll.Until(h.t, 90*time.Second, fmt.Sprintf("session %s never went idle", id), func() bool {
		_, data := h.do("GET", "/session/"+id, nil)
		var s struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(data, &s)
		return s.Status == "idle"
	}, 500*time.Millisecond) // a real remote-model turn: poll coarsely
}

// hasReasoning reports whether any returned message carries a Reasoning part —
// the cross-provider evidence that the model actually reasoned.
func (h *harness) hasReasoning(id string) bool {
	h.t.Helper()
	h.srv.mu.Lock()
	sess := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if sess == nil {
		return false
	}
	for _, m := range sess.sess.History() {
		for _, p := range m.Parts {
			if _, ok := p.(*message.Reasoning); ok {
				return true
			}
		}
	}
	return false
}

// liveCase is one (model, level) probe row.
type liveCase struct {
	model       string
	level       string
	wantReason  bool // expect reasoning evidence when accepted
	description string
}

// TestThinkingLive drives representative (model, level) pairs through the real
// endpoint and prompt path, asserting the provider ACCEPTED the request (the
// turn completed, not errored) and, where expected, that the model actually
// reasoned. The exhaustive per-model matrix lives on the boxes side
// (internal/api/bifrost_models_live_test.go); this is the harness-side proof
// that the endpoint plus each adapter produce a request the provider accepts.
func TestThinkingLive(t *testing.T) {
	base := liveEnabled(t)
	cases := []liveCase{
		{"anthropic/bedrock_mantle/anthropic.claude-sonnet-5", "medium", true, "Claude extended thinking"},
		{"anthropic/bedrock_mantle/anthropic.claude-sonnet-5", "off", false, "Claude thinking disabled"},
		{"openai/bedrock_mantle/openai.gpt-5.5", "high", true, "GPT-5 responses reasoning.effort"},
		{"bifrost/vertex/gemini-2.5-flash", "medium", true, "Gemini thinking via openai-compat"},
	}
	for _, c := range cases {
		t.Run(c.description+"/"+c.level, func(t *testing.T) {
			h := newLiveHarness(t, base)
			id := h.createSession(c.model)

			resp, data := h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": c.level})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("set thinking %q: status %d: %s", c.level, resp.StatusCode, data)
			}

			resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
				"parts": []map[string]string{{"type": "text", "text": "In one sentence, what is 17 * 23?"}},
			})
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("prompt: status %d: %s", resp.StatusCode, data)
			}
			h.waitIdleLive(id)

			// Acceptance: the last turn must be "completed", never an error —
			// an unsupported level surfaces as a provider 400 => turn error.
			h.srv.mu.Lock()
			lt := h.srv.lastTurn[id]
			h.srv.mu.Unlock()
			if lt == nil || lt.outcome != "completed" {
				got := "nil"
				if lt != nil {
					got = lt.outcome + " " + lt.error
				}
				t.Fatalf("%s @ %q: turn not completed (got %s) — provider rejected the level", c.model, c.level, got)
			}
			if c.wantReason && !h.hasReasoning(id) {
				t.Errorf("%s @ %q: accepted but no reasoning evidence", c.model, c.level)
			}
			if !c.wantReason && h.hasReasoning(id) {
				t.Errorf("%s @ %q: expected NO reasoning, but found a Reasoning part", c.model, c.level)
			}
		})
	}
}
