package turn_test

import (
	"context"
	"slices"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

type tool string

func (t tool) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: string(t)} }
func (tool) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	return protocol.ToolResult{}, nil
}

type source struct{ ts turn.Toolset }

func (s source) Toolset(context.Context, []eventlog.Message, []string, string) turn.Toolset {
	return s.ts
}

type plain struct{}

func (plain) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }
func (plain) Run(context.Context, turn.Request, turn.Sink) (turn.Result, error) {
	return turn.Result{}, nil
}

type warmer struct {
	plain
	owns bool
	got  []turn.Request
	err  error
}

func (w *warmer) Capabilities(string) turn.Capabilities { return turn.Capabilities{OwnsLoop: w.owns} }
func (w *warmer) Warm(_ context.Context, req turn.Request) error {
	w.got = append(w.got, req)
	return w.err
}

func names(specs []protocol.ToolSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

func TestWarmDescribesTheFirstModelCall(t *testing.T) {
	w := &warmer{}
	src := source{turn.Toolset{Tools: []turn.Tool{tool("more")}, Prompt: "from the source"}}
	req := turn.Request{SessionID: "s1", Model: "codex/gpt-5", Instructions: "base"}
	if err := turn.Warm(context.Background(), w, req, []turn.Tool{tool("read")}, src); err != nil {
		t.Fatal(err)
	}
	if len(w.got) != 1 {
		t.Fatalf("Warm calls = %d, want 1", len(w.got))
	}
	got := w.got[0]
	if want := []string{"read", "more"}; !slices.Equal(names(got.Tools), want) {
		t.Errorf("tools = %v, want %v", names(got.Tools), want)
	}
	if want := "base\n\nfrom the source"; got.Instructions != want {
		t.Errorf("instructions = %q, want %q", got.Instructions, want)
	}
	if got.SessionID != "s1" || got.Model != "codex/gpt-5" {
		t.Errorf("request = %+v, want the session and model of req", got)
	}
}

func TestWarmReportsTheErrorOfTheBackend(t *testing.T) {
	w := &warmer{err: context.DeadlineExceeded}
	if err := turn.Warm(context.Background(), w, turn.Request{}, nil, nil); err != context.DeadlineExceeded {
		t.Errorf("Warm = %v, want the error of the backend", err)
	}
}

func TestWarmSkipsABackendThatCannotWarm(t *testing.T) {
	if err := turn.Warm(context.Background(), plain{}, turn.Request{}, nil, nil); err != nil {
		t.Errorf("Warm = %v, want nil", err)
	}
}

type gated struct {
	warmer
	can bool
}

func (g *gated) CanWarm(string) bool { return g.can }

type countSource struct{ calls int }

func (s *countSource) Toolset(context.Context, []eventlog.Message, []string, string) turn.Toolset {
	s.calls++
	return turn.Toolset{}
}

func TestWarmDiscoversNothingForAModelThatCannotWarm(t *testing.T) {
	g := &gated{}
	src := &countSource{}
	if err := turn.Warm(context.Background(), g, turn.Request{Model: "anthropic/claude"}, nil, src); err != nil {
		t.Fatal(err)
	}
	if src.calls != 0 || len(g.got) != 0 {
		t.Errorf("tool discovery = %d, Warm calls = %d, want none of either", src.calls, len(g.got))
	}
	g.can = true
	if err := turn.Warm(context.Background(), g, turn.Request{Model: "codex/gpt-5"}, nil, src); err != nil {
		t.Fatal(err)
	}
	if src.calls != 1 || len(g.got) != 1 {
		t.Errorf("tool discovery = %d, Warm calls = %d, want one of each", src.calls, len(g.got))
	}
}
