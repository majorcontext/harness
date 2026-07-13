package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// newRequestHarness builds a harness whose engine sessions wire OnRequest to
// srv.OnRequest (the production wiring), with a fixed base system prompt and
// instructions/skills discovery disabled so the assembled system is
// deterministic. Optional cfg mutators customize each session's engine.Config.
func newRequestHarness(t *testing.T, prov provider.Provider, mutate ...func(*engine.Config)) *harness {
	t.Helper()
	const token = "secret-run-token"
	dir := t.TempDir()
	model := message.ModelRef{Provider: prov.Name(), Model: "m1"}
	var srv *Server
	mkCfg := func(m message.ModelRef) engine.Config {
		if m.IsZero() {
			m = model
		}
		cfg := engine.Config{
			Providers:    provider.Registry{prov.Name(): prov},
			Model:        m,
			System:       []string{"base"},
			SessionDir:   dir,
			Instructions: &engine.InstructionsConfig{Disabled: true},
			SkillsDirs:   []string{},
			OnEvent:      func(ev engine.Event) { srv.Publish(ev) },
		}
		for _, m := range mutate {
			m(&cfg)
		}
		return cfg
	}
	wire := func(cfg engine.Config, build func(engine.Config) (*engine.Session, error)) (*engine.Session, error) {
		var sess *engine.Session
		cfg.OnRequest = func(turn int, req *provider.Request) { srv.OnRequest(sess.ID, turn, req) }
		var err error
		sess, err = build(cfg)
		return sess, err
	}
	opts := Options{
		SessionDir:        dir,
		RunToken:          token,
		Version:           "9.9.9",
		HeartbeatInterval: 20 * time.Millisecond,
		NewSession: func(m message.ModelRef, workDir string, parentSession string) (*engine.Session, error) {
			cfg := mkCfg(m)
			cfg.WorkDir = workDir
			cfg.ParentSession = parentSession
			return wire(cfg, func(c engine.Config) (*engine.Session, error) { return engine.NewSession(c), nil })
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return wire(mkCfg(message.ModelRef{}), func(c engine.Config) (*engine.Session, error) { return engine.LoadSession(c, id) })
		},
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

func findRequestMeta(evs []Event) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Type == "request.meta" {
			out = append(out, ev)
		}
	}
	return out
}

func TestRequestMetaJournaled(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newRequestHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	evs := sse.collectUntilIdle(t)

	metas := findRequestMeta(evs)
	if len(metas) != 1 {
		t.Fatalf("request.meta events = %d, want 1", len(metas))
	}
	rm := metas[0]
	if rm.Seq == 0 {
		t.Error("request.meta missing seq (must be durable)")
	}
	if rm.SessionID != id {
		t.Errorf("session_id = %q, want %q", rm.SessionID, id)
	}
	if rm.Model != (message.ModelRef{Provider: "test", Model: "m1"}) {
		t.Errorf("model = %v", rm.Model)
	}
	if rm.SystemHash == "" {
		t.Error("system_hash empty")
	}
	if rm.Segments != 1 {
		t.Errorf("segments = %d, want 1 (base only)", rm.Segments)
	}
	if rm.SystemLen == 0 {
		t.Error("system_len = 0")
	}
	if rm.Messages != 1 {
		t.Errorf("messages = %d, want 1 (user)", rm.Messages)
	}
	if !containsName(rm.Tools, "session_info") || !containsName(rm.Tools, "bash") {
		t.Errorf("tools = %v, want to include session_info and bash", rm.Tools)
	}
	// First appearance of this hash carries the full system.
	if len(rm.System) != 1 || rm.System[0] != "base" {
		t.Errorf("system = %v, want [base] on first request.meta", rm.System)
	}
}

func TestRequestMetaFullSystemOnFirstOnly(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one"), asstTurn("two")}}
	h := newRequestHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "a"}},
	})
	first := findRequestMeta(sse.collectUntilIdle(t))
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "b"}},
	})
	second := findRequestMeta(sse.collectUntilIdle(t))

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("request.meta counts = %d then %d, want 1 each", len(first), len(second))
	}
	if len(first[0].System) == 0 {
		t.Error("first request.meta must carry the full system")
	}
	if first[0].SystemHash != second[0].SystemHash {
		t.Errorf("unchanged system produced different hashes: %q vs %q", first[0].SystemHash, second[0].SystemHash)
	}
	// Same hash as the previous request: the full system is omitted.
	if len(second[0].System) != 0 {
		t.Errorf("second request.meta must omit the unchanged system, got %v", second[0].System)
	}
}

// TestRequestMetaFullSystemOnChange verifies that when the assembled system
// actually changes (here via a hook that emits a fresh segment each turn) the
// full system reappears in request.meta.
func TestRequestMetaFullSystemOnChange(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one"), asstTurn("two")}}
	hooks := &varyingHooks{}
	h := newRequestHarness(t, prov, func(c *engine.Config) { c.Hooks = hooks })
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "a"}},
	})
	first := findRequestMeta(sse.collectUntilIdle(t))
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "b"}},
	})
	second := findRequestMeta(sse.collectUntilIdle(t))

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("request.meta counts = %d then %d, want 1 each", len(first), len(second))
	}
	if first[0].SystemHash == second[0].SystemHash {
		t.Fatal("expected a changed system to produce a different hash")
	}
	if len(second[0].System) == 0 {
		t.Error("a changed system must re-include the full system in request.meta")
	}
}

func TestRequestEndpoint(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newRequestHarness(t, prov)
	id := h.createSession("test/m1")

	// Never-prompted session: 404 (nothing assembled yet).
	resp, _ := h.do("GET", "/session/"+id+"/request", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pre-prompt /request status = %d, want 404", resp.StatusCode)
	}
	// Unknown session: 404.
	resp, _ = h.do("GET", "/session/ses_nope/request", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown /request status = %d, want 404", resp.StatusCode)
	}

	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	sse.collectUntilIdle(t)

	resp, data := h.do("GET", "/session/"+id+"/request", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/request status = %d: %s", resp.StatusCode, data)
	}
	var rq struct {
		Model    message.ModelRef `json:"model"`
		System   []string         `json:"system"`
		Tools    []string         `json:"tools"`
		Messages int              `json:"messages"`
		Params   struct {
			MaxTokens int `json:"max_tokens"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &rq); err != nil {
		t.Fatalf("decode /request: %v (%s)", err, data)
	}
	if rq.Model != (message.ModelRef{Provider: "test", Model: "m1"}) {
		t.Errorf("model = %v", rq.Model)
	}
	if len(rq.System) != 1 || rq.System[0] != "base" {
		t.Errorf("system = %v, want [base]", rq.System)
	}
	if !containsName(rq.Tools, "session_info") {
		t.Errorf("tools = %v, want session_info", rq.Tools)
	}
	if rq.Messages != 1 {
		t.Errorf("messages = %d, want 1", rq.Messages)
	}
	if rq.Params.MaxTokens != 8192 {
		t.Errorf("params.max_tokens = %d, want 8192 (engine default)", rq.Params.MaxTokens)
	}
}

func TestRequestMetaReplayed(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newRequestHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	sse.collectUntilIdle(t)
	sse.stop()

	// A fresh replay from the start must include the durable request.meta.
	replay := h.openSSE("?from=0", "")
	var got bool
	for !got {
		ev := replay.nextEvent(t)
		if ev.Type == "request.meta" && ev.SessionID == id {
			got = true
		}
		if ev.Type == "session.status" && ev.Status == "idle" {
			break
		}
	}
	if !got {
		t.Fatal("request.meta not present in replay")
	}
}

func containsName(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// varyingHooks emits a distinct system-transform segment on every call so the
// assembled system changes turn to turn.
type varyingHooks struct{ n int }

func (h *varyingHooks) ChatParams(_ context.Context, req *plugin.ChatParamsRequest) plugin.ChatParams {
	return req.Params
}
func (h *varyingHooks) SystemTransform(_ context.Context, _ *plugin.SystemTransformRequest) []string {
	h.n++
	return []string{fmt.Sprintf("dynamic-segment-%d", h.n)}
}
func (h *varyingHooks) ShellEnv(_ context.Context, _ *plugin.ShellEnvRequest) map[string]string {
	return nil
}
func (h *varyingHooks) ToolExecuteBefore(_ context.Context, _ *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string) {
	return nil, ""
}
func (h *varyingHooks) ToolExecuteAfter(_ context.Context, req *plugin.ToolExecuteAfterRequest) message.Parts {
	return req.Output
}
func (h *varyingHooks) ExecuteTool(_ context.Context, _ *plugin.ToolExecuteRequest) (*plugin.ToolExecuteResponse, error) {
	return nil, nil
}
func (h *varyingHooks) Emit(_ []plugin.Event)   {}
func (h *varyingHooks) Tools() []plugin.ToolDef { return nil }

func TestRequestSnapshotEvictedWithSession(t *testing.T) {
	// Snapshots hold full system copies; eviction must release them
	// (review finding on #22). The hash entry survives deliberately so
	// hash-on-change journaling stays correct across eviction cycles.
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("a"), asstTurn("b"),
	}}
	h := newRequestHarness(t, prov)
	h.srv.opts.MaxResident = 1

	sidA := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+sidA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "one"}},
	})
	sse.collectUntilIdle(t)

	sidB := h.createSession("test/m1")
	h.do("POST", "/session/"+sidB+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "two"}},
	})
	sse2 := h.openSSE("?from=0&session="+sidB, "")
	sse2.collectUntilIdle(t)

	h.srv.mu.Lock()
	_, snapA := h.srv.lastRequest[sidA]
	_, hashA := h.srv.lastReqHash[sidA]
	_, residentA := h.srv.sessions[sidA]
	h.srv.mu.Unlock()
	if residentA {
		t.Fatal("session A should have been evicted (MaxResident=1)")
	}
	if snapA {
		t.Error("evicted session A still holds a request snapshot")
	}
	if !hashA {
		t.Error("session A hash entry should survive eviction")
	}

	resp, _ := h.do("GET", "/session/"+sidA+"/request", nil)
	if resp.StatusCode != 404 {
		t.Errorf("evicted session /request = %d, want 404", resp.StatusCode)
	}
}
