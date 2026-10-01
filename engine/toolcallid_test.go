package engine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

type idRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *idRecorder) record(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, ToolCallID(ctx))
}

func (r *idRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func (r *idRecorder) wantOnly(t *testing.T, want string) {
	t.Helper()
	got := r.seen()
	if len(got) != 1 || got[0] != want {
		t.Errorf("recorded tool call ids = %q, want exactly [%q]", got, want)
	}
}

func (r *idRecorder) tool() Tool {
	return Tool{
		Def: provider.ToolDef{Name: "probe", Description: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(ctx context.Context, _ *Session, _ json.RawMessage) (message.Parts, error) {
			r.record(ctx)
			return message.Parts{&message.Text{Text: "ok"}}, nil
		},
	}
}

func idTestConfig(prov provider.Provider) Config {
	return Config{
		Providers: provider.Registry{prov.Name(): prov},
		Model:     modelFor(prov.Name()),
	}
}

func callThenDone(name string) [][]provider.Event {
	return [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("call_1", name, `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}
}

func TestToolCallIDInConfigTool(t *testing.T) {
	rec := &idRecorder{}
	cfg := idTestConfig(scriptedTurns("p", callThenDone("probe")))
	cfg.Tools = []Tool{rec.tool()}
	if _, err := NewSession(cfg).Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	rec.wantOnly(t, "call_1")
}

type idMCP struct {
	minimalFakeMCPRegistry
	rec *idRecorder
}

func (m idMCP) CallTool(ctx context.Context, _ string, _ json.RawMessage) (message.Parts, bool, error) {
	m.rec.record(ctx)
	return message.Parts{&message.Text{Text: "ok"}}, false, nil
}

func TestToolCallIDInMCPTool(t *testing.T) {
	rec := &idRecorder{}
	cfg := idTestConfig(scriptedTurns("p", callThenDone("mcp__svc__ping")))
	cfg.MCP = idMCP{rec: rec}
	if _, err := NewSession(cfg).Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	rec.wantOnly(t, "call_1")
}

type idHooks struct {
	fakeHooks
	rec *idRecorder
}

func (h *idHooks) ToolExecuteBefore(ctx context.Context, req *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string) {
	h.rec.record(ctx)
	return nil, ""
}

func TestToolCallIDInHooks(t *testing.T) {
	rec := &idRecorder{}
	probe := &probeTool{}
	cfg := idTestConfig(scriptedTurns("p", callThenDone("probe")))
	cfg.Tools = []Tool{probe.tool()}
	cfg.Hooks = &idHooks{rec: rec}
	if _, err := NewSession(cfg).Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	rec.wantOnly(t, "call_1")
}

func TestToolCallIDStableAcrossResume(t *testing.T) {
	st := NewMemStore()
	rec := &idRecorder{}
	prov := scriptedTurns("p", doneTurn("a")).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 3, rec.tool())
	cfg.ResumeRerunTools = true
	id := crashAfter(t, st, cfg, userMsg("u1", "q"), toolUseMsg("a1", "call_1"))

	s := reloadSession(t, st, cfg, id)
	if _, err := s.ResumeTurn(context.Background()); err != nil {
		t.Fatalf("ResumeTurn: %v", err)
	}
	rec.wantOnly(t, "call_1")
}

func TestToolCallIDEmptyOutsideToolCall(t *testing.T) {
	if got := ToolCallID(context.Background()); got != "" {
		t.Errorf("ToolCallID(Background) = %q, want empty", got)
	}
}
