package turn_test

import (
	"context"
	"slices"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

type marking struct {
	name string
	log  *[]string
}

func (m marking) Before(_ context.Context, c protocol.ToolCall) (protocol.ToolCall, string) {
	*m.log = append(*m.log, m.name+".before")
	return c, ""
}

func (m marking) After(_ context.Context, _ protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	*m.log = append(*m.log, m.name+".after")
	r.Text += m.name
	return r
}

type denying struct{ log *[]string }

func (d denying) Before(_ context.Context, c protocol.ToolCall) (protocol.ToolCall, string) {
	*d.log = append(*d.log, "deny.before")
	return c, "denied"
}

func (denying) After(_ context.Context, _ protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	return r
}

type ran struct{ calls *int }

func (ran) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: "x"} }
func (t ran) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	*t.calls++
	return protocol.ToolResult{Text: "ok"}, nil
}

func runOne(t *testing.T, calls *int, src turn.Source) eventlog.Message {
	t.Helper()
	m, r := &model{replies: []eventlog.Message{callTool("c1"), say("done")}}, &recorder{}
	ctx := context.Background()
	turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, turn.Sources{turn.Fixed{ran{calls}}, src}, r, turn.Limits{})
	if len(r.items) != 3 {
		t.Fatalf("items = %+v, want the call, its result, and the answer", r.items)
	}
	return r.items[1]
}

func TestSourcesChainTheHooksOfEverySourceInOrder(t *testing.T) {
	var log []string
	calls := 0
	res := runOne(t, &calls, turn.Sources{
		source{turn.Toolset{Hooks: marking{"A", &log}}},
		source{turn.Toolset{Hooks: marking{"B", &log}}},
	})
	if want := []string{"A.before", "B.before", "A.after", "B.after"}; !slices.Equal(log, want) {
		t.Errorf("hooks ran %v, want %v", log, want)
	}
	if got := res.Parts[0].Text; got != "okAB" {
		t.Errorf("result %q, want okAB", got)
	}
}

func TestSourcesStopTheChainAtAHookThatDenies(t *testing.T) {
	var log []string
	calls := 0
	res := runOne(t, &calls, turn.Sources{
		source{turn.Toolset{Hooks: denying{&log}}},
		source{turn.Toolset{Hooks: marking{"B", &log}}},
	})
	if want := []string{"deny.before"}; !slices.Equal(log, want) || calls != 0 {
		t.Errorf("hooks ran %v and the tool ran %d times, want only the deny and no tool run", log, calls)
	}
	if got := res.Parts[0].Text; got != "denied" || !res.Parts[0].IsError {
		t.Errorf("result %q, want the deny as an error", got)
	}
}
