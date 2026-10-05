package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/eventlog"
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

// calls is one assistant message that calls tool once for each argument set.
func calls(tool string, args ...map[string]any) eventlog.Message {
	m := eventlog.Message{Role: eventlog.RoleAssistant}
	for i, a := range args {
		raw, _ := json.Marshal(a)
		m.Parts = append(m.Parts, eventlog.Part{Type: eventlog.PartToolCall, CallID: "g" + string(rune('0'+i)), Name: tool, Arguments: raw})
	}
	return m
}

// results returns the tool results in the newest request of session that has any.
func (f *family) results(session string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, req := range f.reqs {
		var got []string
		for _, m := range req.History {
			for _, p := range m.Parts {
				if p.Type == eventlog.PartToolResult && req.SessionID == session {
					got = append(got, p.Text)
				}
			}
		}
		if len(got) > 0 {
			out = got
		}
	}
	return out
}

func TestNewRejectsAToolThatTakesTheHistoryToolName(t *testing.T) {
	_, err := harness.New(harness.Options{Store: harness.NewMemStore(), Tools: []harness.Tool{newProbe("get_conversation_history", false)}})
	if !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("New = %v, want ErrInvalidRequest", err)
	}
}

func TestNewRejectsAToolThatTakesTheSessionInfoNameOnlyWithAWorkDir(t *testing.T) {
	for _, tc := range []struct {
		workDir string
		want    error
	}{{t.TempDir(), harness.ErrInvalidRequest}, {"", nil}} {
		_, err := harness.New(harness.Options{Store: harness.NewMemStore(), WorkDir: tc.workDir, Tools: []harness.Tool{newProbe("session_info", false)}})
		if !errors.Is(err, tc.want) {
			t.Errorf("New with WorkDir %q = %v, want %v", tc.workDir, err, tc.want)
		}
	}
}
