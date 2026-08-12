//go:build live

// Real-model reasoning-effort verification that runs FROM THIS MACHINE through
// harness's production path — the actual POST /session/{id}/thinking endpoint ->
// Session.SetEffort -> provider transcode -> a real model call -> assert a
// Reasoning part surfaces. No hand-crafted wire shapes; the request is whatever
// the production adapter builds.
//
// It uses whatever real provider credentials exist in THIS environment:
//   - OPENAI_API_KEY -> the native openai adapter (Responses API, reasoning.effort)
//     against api.openai.com. This is the fully-asserted path: OpenAI is not
//     IP-gated, and the openai adapter surfaces reasoning as a message.Reasoning
//     part (openai.go).
//   - OPENROUTER_API_KEY -> the openai-compat adapter against openrouter.ai.
//     REPORT-ONLY: the compat adapter surfaces reasoning from EITHER wire field
//     — Bifrost/DeepSeek `reasoning_content` or OpenRouter `reasoning`
//     (openaicompat.go) — so OpenRouter's `reasoning` IS visible here (see the
//     gemini-2.5-flash row below). These rows log actual behavior but do not
//     hard-assert, and they do NOT validate the box's Bifrost-specific per-model
//     mapping (that needs harness pointed at Bifrost from an allowlisted
//     position).
//
// Run:
//
//	HARNESS_LIVE=1 go test -tags live -run TestThinkingRealModelLive -v ./server/
//
// Optional overrides: OPENAI_REASON_MODEL (default "gpt-5"),
// OPENAI_PLAIN_MODEL (default "gpt-4o-mini").
//
// RESULTS (2026-08-11, run from this machine through the production path):
//
//	openai/gpt-5 (native Responses reasoning.effort)
//	  minimal/low/medium/high  reasoned=true   (asserted)
//	openai/gpt-4o-mini         off             reasoned=false  (asserted)
//	anthropic/claude-haiku-4-5 (NATIVE anthropic thinking.budget_tokens —
//	  the exact path the box uses for Claude, via api.anthropic.com)
//	  off                      reasoned=false  (asserted)
//	  minimal/low/medium/high  reasoned=true   (asserted)
//	openrouter/google/gemini-2.5-flash  medium  reasoned=true  (report)
//	openrouter/anthropic/claude-opus-4.8 medium reasoned=false (report) —
//	  OpenRouter returns no `reasoning` for Claude from a plain
//	  reasoning_effort (it needs a Claude-specific reasoning param the box
//	  does NOT use); the box drives Claude through the native anthropic path
//	  above, which IS verified here.
//	openrouter/deepseek/deepseek-r1-0528 medium  404 on this account (report)
//
// All three deployed families — Claude (native anthropic), GPT-5 (native
// openai), Gemini (openai-compat) — surface reasoning through the real
// endpoint for their mapped levels; off surfaces none.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
	"github.com/majorcontext/harness/provider/openai"
	"github.com/majorcontext/harness/provider/openaicompat"
)

// newRealModelHarness builds a server whose registry uses this machine's real
// provider keys. Providers with no key are omitted; a case naming a missing
// provider skips.
func newRealModelHarness(t *testing.T) *harness {
	t.Helper()
	reg := provider.Registry{}
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		reg["openai"] = &openai.Client{APIKey: k}
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		reg["anthropic"] = &anthropic.Client{APIKey: k}
	}
	if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		reg["openrouter"] = &openaicompat.Client{Family: "openrouter", APIKey: k, BaseURL: "https://openrouter.ai/api/v1"}
	}
	dir := t.TempDir()
	var srv *Server
	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:  reg,
			Model:      m,
			SessionDir: dir,
			MaxTokens:  20000,
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

// providerConfigured reports whether reg has the model's provider key.
func (h *harness) providerConfigured(model string) bool {
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	ref, err := message.ParseModelRef(model)
	if err != nil {
		return false
	}
	// Peek at a session's registry via a throwaway resolve is overkill; instead
	// check the env keys the builder used.
	switch ref.Provider {
	case "openai":
		return os.Getenv("OPENAI_API_KEY") != ""
	case "anthropic":
		return os.Getenv("ANTHROPIC_API_KEY") != ""
	case "openrouter":
		return os.Getenv("OPENROUTER_API_KEY") != ""
	}
	return false
}

// runOne drives the production path for one (model, level) and returns whether a
// Reasoning part surfaced, plus the turn outcome.
func (h *harness) runOne(t *testing.T, model, level string) (reasoned bool, outcome, errMsg string) {
	t.Helper()
	id := h.createSession(model)
	resp, data := h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": level})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set thinking %q: %d: %s", level, resp.StatusCode, data)
	}
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "What is 17 times 23? Explain your reasoning in one line."}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt: %d: %s", resp.StatusCode, data)
	}
	h.waitIdleLive(id)
	h.srv.mu.Lock()
	lt := h.srv.lastTurn[id]
	h.srv.mu.Unlock()
	if lt != nil {
		outcome, errMsg = lt.outcome, lt.error
	}
	return h.hasReasoning(id), outcome, errMsg
}

// TestThinkingRealModelLive is the production-path matrix. The openai rows are
// hard-asserted; the openrouter rows are report-only (see the file header).
func TestThinkingRealModelLive(t *testing.T) {
	if os.Getenv("HARNESS_LIVE") == "" {
		t.Skip("HARNESS_LIVE unset")
	}
	reasonModel := envOr("OPENAI_REASON_MODEL", "gpt-5")
	plainModel := envOr("OPENAI_PLAIN_MODEL", "gpt-4o-mini")
	claudeModel := envOr("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001")

	type row struct {
		model  string
		level  string
		assert string // "reason", "noreason", or "report"
	}
	rows := []row{
		{"openai/" + reasonModel, "minimal", "reason"},
		{"openai/" + reasonModel, "low", "reason"},
		{"openai/" + reasonModel, "medium", "reason"},
		{"openai/" + reasonModel, "high", "reason"},
		{"openai/" + plainModel, "off", "noreason"}, // non-reasoning model, no reasoning param
		// Native anthropic adapter (api.anthropic.com, thinking.budget_tokens) —
		// the exact path the box uses for Claude. Thinking surfaces as a
		// Reasoning part for the non-off levels; off sends no thinking block.
		{"anthropic/" + claudeModel, "off", "noreason"},
		{"anthropic/" + claudeModel, "minimal", "reason"},
		{"anthropic/" + claudeModel, "low", "reason"},
		{"anthropic/" + claudeModel, "medium", "reason"},
		{"anthropic/" + claudeModel, "high", "reason"},
		// OpenRouter reasoning models via the openai-compat adapter (now reads
		// the `reasoning` field): report actual behavior. A real Claude and a
		// real Gemini through the production path.
		{"openrouter/anthropic/claude-opus-4.8", "medium", "report"},
		{"openrouter/google/gemini-2.5-flash", "medium", "report"},
		{"openrouter/deepseek/deepseek-r1-0528", "medium", "report"},
	}

	fmt.Println("=== REAL-MODEL THINKING MATRIX (production path) ===")
	for _, r := range rows {
		t.Run(r.model+"/"+r.level, func(t *testing.T) {
			h := newRealModelHarness(t)
			if !h.providerConfigured(r.model) {
				t.Skipf("no key for %s", r.model)
			}
			reasoned, outcome, errMsg := h.runOne(t, r.model, r.level)
			fmt.Printf("ROW\t%s\t%s\treasoned=%v\toutcome=%s\t%s\n", r.model, r.level, reasoned, outcome, errMsg)
			switch r.assert {
			case "reason":
				if outcome != "completed" {
					t.Fatalf("%s @ %s: outcome %s %s (provider rejected the level)", r.model, r.level, outcome, errMsg)
				}
				if !reasoned {
					t.Errorf("%s @ %s: expected reasoning evidence, none surfaced", r.model, r.level)
				}
			case "noreason":
				if outcome != "completed" {
					t.Fatalf("%s @ %s: outcome %s %s", r.model, r.level, outcome, errMsg)
				}
				if reasoned {
					t.Errorf("%s @ %s: expected NO reasoning, but a Reasoning part surfaced", r.model, r.level)
				}
			}
		})
	}
}

// TestThinkingHighThenOffLive verifies the high->off downgrade does NOT wedge a
// real session on BOTH reasoning adapters. A high turn stores reasoning
// (anthropic thinking blocks / openai reasoning items); switching to off and
// prompting again must still complete — the adapter strips the stored parts when
// the request enables no reasoning, so the API never sees a reasoning part with
// reasoning disabled (which would 400 every later turn). Drives the real
// production path against api.anthropic.com and api.openai.com.
func TestThinkingHighThenOffLive(t *testing.T) {
	if os.Getenv("HARNESS_LIVE") == "" {
		t.Skip("HARNESS_LIVE unset")
	}
	cases := []struct {
		name, keyEnv, model string
	}{
		{"anthropic", "ANTHROPIC_API_KEY", "anthropic/" + envOr("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001")},
		{"openai", "OPENAI_API_KEY", "openai/" + envOr("OPENAI_REASON_MODEL", "gpt-5")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if os.Getenv(c.keyEnv) == "" {
				t.Skipf("%s unset", c.keyEnv)
			}
			h := newRealModelHarness(t)
			id := h.createSession(c.model)
			prompt := func(text string) {
				resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
					"parts": []map[string]string{{"type": "text", "text": text}},
				})
				if resp.StatusCode != http.StatusAccepted {
					t.Fatalf("prompt: %d %s", resp.StatusCode, data)
				}
				h.waitIdleLive(id)
			}
			// Turn 1: high — stores reasoning on THIS session.
			if resp, data := h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": "high"}); resp.StatusCode != http.StatusOK {
				t.Fatalf("set high: %d %s", resp.StatusCode, data)
			}
			prompt("What is 17 times 23? Explain briefly.")
			if !h.hasReasoning(id) {
				t.Fatalf("high turn produced no stored reasoning — cannot exercise the downgrade")
			}
			// Turn 2 on the SAME session: off — must complete (no wedge).
			if resp, data := h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": "off"}); resp.StatusCode != http.StatusOK {
				t.Fatalf("set off: %d %s", resp.StatusCode, data)
			}
			prompt("Now just say the number.")
			h.srv.mu.Lock()
			lt := h.srv.lastTurn[id]
			h.srv.mu.Unlock()
			if lt == nil || lt.outcome != "completed" {
				got := "nil"
				if lt != nil {
					got = lt.outcome + " " + lt.error
				}
				t.Fatalf("%s high->off second turn not completed (got %s) — stored reasoning wedged the session", c.name, got)
			}
			fmt.Printf("ROW\thigh->off downgrade\t%s\tsecond-turn=completed (no wedge)\n", c.name)
		})
	}
}

// hasToolCall reports whether id's history holds any assistant tool call yet —
// the signal that round 1 has emitted its tool_use and is now executing it.
func (h *harness) hasToolCall(id string) bool {
	h.srv.mu.Lock()
	st := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if st == nil {
		return false
	}
	for _, m := range st.sess.History() {
		for _, p := range m.Parts {
			if _, ok := p.(*message.ToolCall); ok {
				return true
			}
		}
	}
	return false
}

// TestEnableMidToolRoundLive exercises the ENABLE direction (Finding 1): a plain
// off->high via POST /thinking WHILE a turn is mid-tool-call. runAgenticLoop
// rebuilds the request with the fresh effort on the next tool round, so the
// round-2 request enables thinking over the round-1 tool_use that carries no
// thinking block. It drives the real production path and REPORTS whether the API
// tolerates the shape or rejects it (the documented KNOWN LIMITATION). It never
// hard-fails on a reject — that reject IS the limitation — but it fails if the
// mid-round injection could not be set up.
func TestEnableMidToolRoundLive(t *testing.T) {
	if os.Getenv("HARNESS_LIVE") == "" {
		t.Skip("HARNESS_LIVE unset")
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY unset")
	}
	model := "anthropic/" + envOr("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001")
	h := newRealModelHarness(t)
	id := h.createSession(model)
	// effort starts unset (off). Round 1 runs thinking-off and makes a slow tool
	// call, opening a window to enable thinking before round 2.
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "Use the bash tool to run exactly: sleep 5; echo TOKEN123 . Then tell me what it printed."}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt: %d %s", resp.StatusCode, data)
	}
	// Wait for round 1's tool_use to land (bash is now sleeping), then enable
	// thinking so round 2 rebuilds with it.
	deadline := time.Now().Add(30 * time.Second)
	for !h.hasToolCall(id) {
		if time.Now().After(deadline) {
			t.Skip("model made no tool call — cannot set up the mid-round enable")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if resp, data := h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": "high"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("set high: %d %s", resp.StatusCode, data)
	}
	h.waitIdleLive(id)
	h.srv.mu.Lock()
	lt := h.srv.lastTurn[id]
	h.srv.mu.Unlock()
	outcome, errMsg := "nil", ""
	if lt != nil {
		outcome, errMsg = lt.outcome, lt.error
	}
	fmt.Printf("ROW\tenable-mid-tool-round\tanthropic\toutcome=%s\t%s\n", outcome, errMsg)
	if outcome != "completed" {
		t.Logf("KNOWN LIMITATION confirmed: enable-mid-tool-round rejected (outcome=%s %s)", outcome, errMsg)
	} else {
		t.Logf("enable-mid-tool-round TOLERATED by the API this run (outcome=completed)")
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var _ = json.Marshal
