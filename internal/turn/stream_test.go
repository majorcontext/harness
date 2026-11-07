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

func TestRunHoldsEveryCallAfterALateToolUntilTheModelCallEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, w := lateGate{newGate("l", true)}, newGate("w", false)
		close(l.open)
		close(w.open)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "l"), call("2", "w")}}
		m.hold = func() {
			synctest.Wait()
			if got := w.started(); len(got) != 0 {
				t.Errorf("w started %v before the late call that came first", got)
			}
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{l, w}, &recorder{}, turn.Limits{})
		if got := slices.Concat(l.started(), w.started()); !slices.Equal(got, []string{"1", "2"}) {
			t.Errorf("calls ran %v, want both once", got)
		}
	})
}

// parallelGate is a gate that is a Parallel tool. The key is its Key.
type parallelGate struct {
	*gate
	key string
}

func (parallelGate) Parallel() {}

func (g parallelGate) Key(protocol.ToolCall) string { return g.key }

type aloneParallelGate struct{ parallelGate }

func (aloneParallelGate) Alone() {}

func TestRunOverlapsParallelCallsWithDifferentKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := newGate("a", false), newGate("b", false)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "a"), call("2", "b")}}
		m.hold = func() {
			synctest.Wait()
			if len(a.started()) != 1 || len(b.started()) != 1 {
				t.Errorf("started a=%v b=%v, want both beside each other", a.started(), b.started())
			}
			close(a.open)
			close(b.open)
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{parallelGate{gate: a, key: "x"}, parallelGate{gate: b, key: "y"}}, &recorder{}, turn.Limits{})
	})
}

func TestRunOrdersParallelCallsWithTheSameKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := newGate("a", false), newGate("b", false)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "a"), call("2", "b")}}
		m.hold = func() {
			synctest.Wait()
			if len(a.started()) != 1 || len(b.started()) != 0 {
				t.Errorf("started a=%v b=%v, want only a while a runs", a.started(), b.started())
			}
			close(a.open)
			synctest.Wait()
			if got := b.started(); !slices.Equal(got, []string{"2"}) {
				t.Errorf("b started %v after a ended, want [2]", got)
			}
			close(b.open)
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{parallelGate{gate: a, key: "k"}, parallelGate{gate: b, key: "k"}}, &recorder{}, turn.Limits{})
	})
}

func TestRunHoldsAParallelCallBetweenExclusiveCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w1, p, w2 := newGate("w1", false), newGate("p", false), newGate("w2", false)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "w1"), call("2", "p"), call("3", "w2")}}
		m.hold = func() {
			synctest.Wait()
			if len(w1.started()) != 1 || len(p.started()) != 0 || len(w2.started()) != 0 {
				t.Errorf("started w1=%v p=%v w2=%v, want only w1", w1.started(), p.started(), w2.started())
			}
			close(w1.open)
			synctest.Wait()
			if len(p.started()) != 1 || len(w2.started()) != 0 {
				t.Errorf("started p=%v w2=%v, want p but not w2", p.started(), w2.started())
			}
			close(p.open)
			synctest.Wait()
			if len(w2.started()) != 1 {
				t.Errorf("w2 started %v after p ended, want one start", w2.started())
			}
			close(w2.open)
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{w1, parallelGate{gate: p}, w2}, &recorder{}, turn.Limits{})
	})
}

func TestRunRunsAToolThatIsAloneAndParallelAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p1, ap, p2 := newGate("p1", false), newGate("ap", false), newGate("p2", false)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "p1"), call("2", "ap"), call("3", "p2")}}
		m.hold = func() {
			synctest.Wait()
			if len(p1.started()) != 1 || len(ap.started()) != 0 || len(p2.started()) != 0 {
				t.Errorf("started p1=%v ap=%v p2=%v, want only p1", p1.started(), ap.started(), p2.started())
			}
			close(p1.open)
			synctest.Wait()
			if len(ap.started()) != 1 || len(p2.started()) != 0 {
				t.Errorf("started ap=%v p2=%v, want ap alone", ap.started(), p2.started())
			}
			close(ap.open)
			synctest.Wait()
			if len(p2.started()) != 1 {
				t.Errorf("p2 started %v after ap ended, want one start", p2.started())
			}
			close(p2.open)
		}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{parallelGate{gate: p1}, aloneParallelGate{parallelGate{gate: ap}}, parallelGate{gate: p2}}, &recorder{}, turn.Limits{})
	})
}

func TestRunFinishesAParallelCallOfTheStepThatAStopToolEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stop, other := newGate("stop", false), newGate("other", false)
		m := &streamModel{calls: []protocol.ToolCall{call("1", "stop"), call("2", "other")}}
		m.hold = func() {
			synctest.Wait()
			close(stop.open)
			synctest.Wait()
			close(other.open)
		}
		r := &recorder{}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, prompt(), toolList{parallelGate{gate: stop}, parallelGate{gate: other}}, r, turn.Limits{StopAfterTools: []string{"stop"}})
		if !slices.Equal(other.ends, []string{"2"}) {
			t.Errorf("other ended %v, want [2]: the stop tool must not cut a parallel call short", other.ends)
		}
		if len(r.items) != 3 {
			t.Errorf("recorded %d items, want the assistant message and both results, and no second model call", len(r.items))
		}
	})
}
