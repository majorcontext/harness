package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const noTool = "no such tool available: "

// probe is an embedder tool that records each call. It returns err, or a
// text that names it and its arguments. With hold, it reports the call on
// started and returns only after release closes or its ctx ends.
type probe struct {
	name    string
	err     error
	hold    bool
	started chan protocol.ToolCall
	release chan struct{}

	mu    sync.Mutex
	calls []string
}

func newProbe(name string, hold bool) *probe {
	return &probe{name: name, hold: hold, started: make(chan protocol.ToolCall, 1), release: make(chan struct{})}
}

func (p *probe) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: p.name, Description: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (p *probe) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, call.ID+" "+call.Name)
	p.mu.Unlock()
	if p.hold {
		p.started <- call
		select {
		case <-p.release:
		case <-ctx.Done():
			return protocol.ToolResult{}, context.Cause(ctx)
		}
	}
	return protocol.ToolResult{Text: p.name + " ran " + string(call.Arguments)}, p.err
}

func (p *probe) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func callStep(name string) harnesstest.Step {
	return harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{
		ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: name, Input: map[string]any{"x": 1}}}}}
}

func TestEmbedderTools(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed []string
		called  string
		want    string
		failed  bool
		tools   []string
		ran     []string
	}{
		{name: "a tool call runs the tool and the next request carries its result",
			called: "echo", want: `echo ran {"x":1}`, tools: []string{"echo", "secret"}, ran: []string{"call_1 echo"}},
		{name: "Restrict hides a tool and refuses a call to it", allowed: []string{"echo"},
			called: "secret", want: noTool + "secret", failed: true, tools: []string{"echo"}},
		{name: "an empty allow list hides every tool", allowed: []string{},
			called: "echo", want: noTool + "echo", failed: true},
		{name: "a tool error is an error result",
			called: "fail", want: "boom", failed: true, tools: []string{"echo", "fail", "secret"}, ran: []string{"call_1 fail"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			echo, secret, fail := newProbe("echo", false), newProbe("secret", false), newProbe("fail", false)
			fail.err = errors.New("boom")
			tools := []harness.Tool{echo, secret}
			if tc.called == "fail" {
				tools = append(tools, fail)
			}
			s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, callStep(tc.called),
				harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult(tc.called), Reply: harnesstest.Reply{Text: "done"}})
			r, st, _ := codexRuntime(t, s, false, false, "", tools...)
			sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5", AllowedTools: tc.allowed})
			if err != nil {
				t.Fatal(err)
			}
			converse(t, sess, "run")
			wantLog(t, st, 2, "input.admitted a", "turn.started a", "context.measured", "item.completed assistant call_1",
				"item.completed tool call_1 "+tc.want, "context.measured", "item.completed assistant done", "turn.ended completed")
			reqs := s.Requests()
			for i, req := range reqs {
				if !slices.Equal(req.Tools, tc.tools) {
					t.Errorf("request %d tools = %q, want %q", i, req.Tools, tc.tools)
				}
			}
			wire := tc.want
			if tc.failed {
				wire = "[tool error] " + wire
			}
			last := reqs[1].Messages[len(reqs[1].Messages)-1].Parts
			if len(last) != 1 || last[0].ToolUseID != "call_1" || last[0].Text != wire {
				t.Errorf("last message of the second request = %+v, want the result of call_1: %q", last, wire)
			}
			if got := slices.Concat(echo.Calls(), secret.Calls(), fail.Calls()); !slices.Equal(got, tc.ran) {
				t.Errorf("tool runs = %q, want %q", got, tc.ran)
			}
		})
	}
}

func TestInterruptStopsARunningTool(t *testing.T) {
	wait := newProbe("wait", true)
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, callStep("wait"))
	r, st, _ := codexRuntime(t, s, false, false, "", wait)
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Submit(bg, text("a", "run")); err != nil {
		t.Fatal(err)
	}
	<-wait.started
	if err := sess.Interrupt(bg, protocol.Interrupt{}); err != nil {
		t.Fatal(err)
	}
	wantLog(t, st, 2, "input.admitted a", "turn.started a", "context.measured", "item.completed assistant call_1",
		"item.completed tool call_1 "+interrupted, "turn.ended interrupted stopped")
}

// TestHandoffLetsARunningToolFinish runs on the fake backend: a synctest
// bubble cannot wait on the network I/O of the OpenAI server.
func TestHandoffLetsARunningToolFinish(t *testing.T) {
	eachStore(t, func(t *testing.T, openStore func() harness.Store) {
		bash := newProbe("bash", true)
		start := func(f *fake) *harness.Runtime {
			f.ownsLoop = false
			tools := []harness.Tool{bash, newProbe("hidden", false)}
			r, err := harness.NewWithBackend(harness.Options{Store: openStore(), Tools: tools}, f)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		f1, f2 := newFake(), newFake()
		r1 := start(f1)
		s, err := r1.Create(bg, protocol.CreateSession{ID: "s1", Model: "test/model", AllowedTools: []string{"bash"}})
		if err != nil {
			t.Fatal(err)
		}
		submit(t, s, text("a", "hi"))
		run := <-f1.runs
		two := callTool("c1")
		two.Parts = append(two.Parts, callTool("c2").Parts...)
		run.emit(two)
		run.end()
		closed := make(chan error)
		go func() { closed <- r1.Close(bg) }()
		synctest.Wait()
		close(bash.release)
		if err := <-closed; err != nil {
			t.Fatalf("Close: %v", err)
		}
		r2 := start(f2)
		open(t, r2)
		next := <-f2.runs
		for _, req := range []turn.Request{run.req, next.req} {
			if len(req.Tools) != 1 || req.Tools[0].Name != "bash" {
				t.Fatalf("Request.Tools = %+v, want bash", req.Tools)
			}
		}
		h := next.req.History
		if next.req.TurnID != run.req.TurnID || h[len(h)-2].Parts[0].CallID != "c1" {
			t.Fatalf("resumed Request = %+v, want turn %s again with the result of c1", next.req, run.req.TurnID)
		}
		next.emit(say("rest"))
		next.end()
		wantLog(t, openStore(), 2, "input.admitted a", "turn.started a", "item.completed assistant c1 c2",
			"item.completed tool c1 bash ran {}", "item.completed tool c2 "+cutOff, "turn.suspended handoff",
			"owner.acquired 1", "turn.resumed 1", "item.completed assistant rest", "turn.ended completed")
		if got := bash.Calls(); !slices.Equal(got, []string{"c1 bash"}) {
			t.Errorf("tool runs = %q, want c1 once", got)
		}
		closeRuntime(t, r2)
	})
}
