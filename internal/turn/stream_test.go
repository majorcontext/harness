package turn_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// gate is a tool whose calls wait for open before they end.
type gate struct {
	name     string
	readOnly bool
	open     chan struct{}

	mu     sync.Mutex
	starts []string
	ends   []string
	active int
	peak   int
	ctxErr []error
}

func newGate(name string, readOnly bool) *gate {
	return &gate{name: name, readOnly: readOnly, open: make(chan struct{})}
}

func (g *gate) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: g.name, ReadOnly: g.readOnly} }

func (g *gate) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	g.mu.Lock()
	g.starts = append(g.starts, c.ID)
	g.active++
	g.peak = max(g.peak, g.active)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active--
		g.mu.Unlock()
	}()
	select {
	case <-g.open:
	case <-ctx.Done():
		g.mu.Lock()
		g.ctxErr = append(g.ctxErr, ctx.Err())
		g.mu.Unlock()
		return protocol.ToolResult{}, ctx.Err()
	}
	g.mu.Lock()
	g.ends = append(g.ends, c.ID)
	g.mu.Unlock()
	return protocol.ToolResult{Text: "result " + c.ID}, nil
}

func (g *gate) started() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.starts)
}

type toolList []turn.Tool

func (l toolList) Toolset(context.Context, []protocol.LogMessage, []string, string) turn.Toolset {
	return turn.Toolset{Tools: l}
}

// streamModel streams each of its calls, then runs hold, then ends the call with an item that carries them.
type streamModel struct {
	calls []protocol.ToolCall
	hold  func()
	err   error
}

func (*streamModel) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (m *streamModel) Run(_ context.Context, req turn.Request, out turn.Sink) error {
	if len(req.History) > 1 {
		return out.Item(say("done"))
	}
	for _, c := range m.calls {
		out.(turn.CallSink).Call(c)
	}
	if m.hold != nil {
		m.hold()
	}
	if m.err != nil {
		return m.err
	}
	var parts []protocol.LogPart
	for _, c := range m.calls {
		parts = append(parts, protocol.LogPart{Type: protocol.LogPartToolCall, CallID: c.ID, Name: c.Name, Arguments: c.Arguments})
	}
	return out.Item(protocol.LogMessage{Role: protocol.RoleAssistant, Parts: parts})
}

func call(id, name string) protocol.ToolCall {
	return protocol.ToolCall{ID: id, Name: name, Arguments: []byte(`{}`)}
}

func prompt() turn.Request {
	return turn.Request{Model: "test/model", History: []protocol.LogMessage{steerInput("go")}}
}

func TestRunStartsAToolCallBeforeTheModelCallEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newGate("a", false)
		var seen []string
		m := &streamModel{calls: []protocol.ToolCall{call("1", "a")}}
		m.hold = func() {
			synctest.Wait()
			seen = a.started()
			close(a.open)
		}
		r := &recorder{}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{a}, r, turn.Limits{})
		if !slices.Equal(seen, []string{"1"}) {
			t.Errorf("calls started while the model call streamed = %v, want [1]", seen)
		}
		if got := a.started(); !slices.Equal(got, []string{"1"}) {
			t.Errorf("the tool ran %v, want exactly once", got)
		}
		if len(r.ended) != 1 || r.ended[0] != nil {
			t.Fatalf("Ended = %v, want one with no error", r.ended)
		}
	})
}

func TestRunNeverOverlapsCallsThatAreNotReadOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := newGate("a", false), newGate("b", false)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "a"), call("2", "b")}}
		m.hold = func() {
			synctest.Wait()
			if got := b.started(); len(got) != 0 {
				t.Errorf("b started %v while a ran", got)
			}
			close(a.open)
			synctest.Wait()
			if got := b.started(); !slices.Equal(got, []string{"2"}) {
				t.Errorf("b started %v after a ended, want [2]", got)
			}
			close(b.open)
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{a, b}, &recorder{}, turn.Limits{})
	})
}

func TestRunOverlapsReadOnlyCallsButNotAnExclusiveOneBetweenThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r1, w, r2 := newGate("r1", true), newGate("w", false), newGate("r2", true)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "r1"), call("2", "w"), call("3", "r2")}}
		m.hold = func() {
			synctest.Wait()
			if len(r1.started()) != 1 || len(w.started()) != 0 || len(r2.started()) != 0 {
				t.Errorf("started r1=%v w=%v r2=%v, want only r1", r1.started(), w.started(), r2.started())
			}
			close(r1.open)
			synctest.Wait()
			if len(w.started()) != 1 || len(r2.started()) != 0 {
				t.Errorf("started w=%v r2=%v, want w alone", w.started(), r2.started())
			}
			close(w.open)
			close(r2.open)
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{r1, w, r2}, &recorder{}, turn.Limits{})
	})
	synctest.Test(t, func(t *testing.T) {
		a, b := newGate("a", true), newGate("b", true)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "a"), call("2", "b")}}
		m.hold = func() {
			synctest.Wait()
			if len(a.started()) != 1 || len(b.started()) != 1 {
				t.Errorf("started a=%v b=%v, want both", a.started(), b.started())
			}
			close(a.open)
			close(b.open)
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{a, b}, &recorder{}, turn.Limits{})
	})
}

func TestRunRecordsResultsInCallOrderWhateverOrderTheyEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := newGate("a", true), newGate("b", true)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "a"), call("2", "b")}}
		m.hold = func() {
			synctest.Wait()
			close(b.open)
			synctest.Wait()
			close(a.open)
		}
		r := &recorder{}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{a, b}, r, turn.Limits{})
		var got []string
		for _, it := range r.items {
			for _, p := range it.Parts {
				if p.Type == protocol.LogPartToolResult {
					got = append(got, p.CallID)
				}
			}
		}
		if !slices.Equal(got, []string{"1", "2"}) {
			t.Errorf("results recorded for %v, want [1 2]", got)
		}
	})
}

func TestRunCancelsAndWaitsForAStartedCallWhenTheModelCallFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newGate("a", false)
		boom := errors.New("stream broke")
		m := &streamModel{calls: []protocol.ToolCall{call("1", "a")}, err: boom}
		m.hold = synctest.Wait
		r := &recorder{}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{a}, r, turn.Limits{})
		if len(r.ended) != 1 || !errors.Is(r.ended[0], boom) {
			t.Fatalf("Ended = %v, want the stream error", r.ended)
		}
		if len(a.ctxErr) != 1 || !errors.Is(a.ctxErr[0], context.Canceled) {
			t.Errorf("the running tool saw %v, want a canceled context", a.ctxErr)
		}
		if len(a.ends) != 0 || len(r.items) != 0 {
			t.Errorf("tool ends %v and items %d, want none recorded", a.ends, len(r.items))
		}
	})
}

func TestRunDoesNotRunAgainAToolThatStartedBeforeARetryableFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newGate("a", false)
		close(a.open)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "a")}, err: turn.ErrRetryable, hold: synctest.Wait}
		r := &recorder{}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{a}, r, turn.Limits{Retries: 2})
		if got := a.started(); len(got) != 1 {
			t.Errorf("the tool ran %v, want once", got)
		}
		if len(r.ended) != 1 || !errors.Is(r.ended[0], turn.ErrRetryable) {
			t.Errorf("Ended = %v, want the retryable error", r.ended)
		}
	})
}

type askTool struct{ ran *bool }

func (askTool) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: turn.AskTool} }
func (a askTool) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	*a.ran = true
	return protocol.ToolResult{Text: "asked"}, nil
}

func TestRunDoesNotStartTheQuestionToolWhileTheModelCallStreams(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := false
		m := &streamModel{calls: []protocol.ToolCall{call("1", turn.AskTool)}}
		m.hold = func() {
			synctest.Wait()
			if ran {
				t.Error("the question tool ran while the model call streamed")
			}
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{askTool{&ran}}, &recorder{}, turn.Limits{})
		if ran {
			t.Error("the question tool ran as a plain call; it opens a request")
		}
	})
}

type lateGate struct{ *gate }

func (lateGate) Late() {}

func TestRunStartsALateToolOnlyAfterTheModelCallEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := lateGate{newGate("l", true)}
		close(l.open)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "l")}}
		m.hold = func() {
			synctest.Wait()
			if got := l.started(); len(got) != 0 {
				t.Errorf("the late tool started %v while the model call streamed", got)
			}
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{l}, &recorder{}, turn.Limits{})
		if got := l.started(); !slices.Equal(got, []string{"1"}) {
			t.Errorf("the late tool ran %v, want once after the model call", got)
		}
	})
}
