package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/backend/modelapi"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
)

// runOn runs session s1 on an Anthropic model served by p. Each compaction keeps one turn.
func runOn(t *testing.T, p provider.Provider, lim turn.Limits) (*Actor, *memLog) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	log, b := &memLog{}, modelapi.New(p, 0)
	a, err := Create(ctx, Config{ID: "s1", Store: log, Ownership: owned{}, Backend: b, Limits: lim, Threshold: 0.8, KeepTurns: 1,
		Base: ctx, Go: wg.Go, Done: func() {}, Prompt: func() string { return "" }}, eventlog.SessionCreated{Model: "anthropic/claude-opus-5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Run()
	return a, log
}

// converse submits each text as an input and waits for the turn that it starts to end.
func converse(t *testing.T, a *Actor, texts ...string) {
	t.Helper()
	for _, txt := range texts {
		in := eventlog.InputAdmitted{InputID: txt, Delivery: eventlog.DeliveryQueue, Source: "user",
			Parts: []eventlog.Part{{Type: eventlog.PartText, Text: txt}}}
		if _, _, err := a.Submit(context.Background(), in, ""); err != nil {
			t.Fatal(err)
		}
		for v := a.View(); v.Session.TurnID != "" && !v.Stopped; v = a.View() {
			<-v.changed
		}
	}
}

// lines renders each record of l after seq as its kind and its message or end.
func lines(t *testing.T, l *memLog, after uint64) []string {
	t.Helper()
	l.mu.Lock()
	recs := slices.Clone(l.recs[after:])
	l.mu.Unlock()
	var out []string
	for _, r := range recs {
		env, err := eventlog.Decode(r.Data)
		if err != nil {
			t.Fatal(err)
		}
		f := []string{env.Event.Kind()}
		switch e := env.Event.(type) {
		case eventlog.ItemCompleted:
			f = append(f, e.Message.Role)
			for _, p := range e.Message.Parts {
				f = append(f, strings.TrimSpace(p.CallID+" "+p.Text))
			}
		case eventlog.TurnEnded:
			f = append(f, string(e.StopReason), string(e.Cause), e.Error)
		case eventlog.GoalEvaluated:
			f = append(f, string(e.Verdict))
		case eventlog.GoalChanged:
			f = append(f, string(e.State))
		}
		out = append(out, strings.Join(slices.DeleteFunc(f, func(s string) bool { return s == "" }), " "))
	}
	return out
}

// lastMessageContains matches a request whose last message holds substr in a
// text part, also in an engine context part.
func lastMessageContains(substr string) harnesstest.Matcher {
	return func(r harnesstest.Request) bool {
		for _, p := range r.Messages[len(r.Messages)-1].Parts {
			if p.Kind == "text" && strings.Contains(p.Text, substr) {
				return true
			}
		}
		return false
	}
}

func TestModelCallErrors(t *testing.T) {
	type step = harnesstest.Step
	type reply = harnesstest.Reply
	cut := func(name, after string, calls ...harnesstest.ToolCall) step {
		return step{Name: name, Match: lastMessageContains(after), Reply: reply{Text: name, ToolCalls: calls, StopReason: "max_tokens"}}
	}
	const nudge = "auto-continue 1 of 1"
	started := []string{"input.admitted", "turn.started"}
	for _, tc := range []struct {
		name  string
		steps []step
		want  []string
	}{
		{"a response cut off at max_tokens continues",
			[]step{cut("first half", "charlie"), {Name: "rest", Match: lastMessageContains(nudge), Reply: reply{Text: "second half"}}},
			[]string{"context.measured", "item.completed assistant first half", "context.measured", "item.completed assistant second half",
				"turn.ended completed"}},
		{"a tool call of a cut-off response does not run",
			[]step{cut("call", "charlie", harnesstest.ToolCall{ID: "c1", Name: "write_file"}), {Name: "rest", Match: lastMessageContains(nudge), Reply: reply{Text: "ok"}}},
			[]string{"context.measured", "item.completed assistant call c1", "item.completed tool c1 not run: the response was cut off at its output limit", "context.measured",
				"item.completed assistant ok", "turn.ended completed"}},
		{"a cut-off response past the continuations fails the turn",
			[]step{cut("first half", "charlie"), cut("second half", nudge)},
			[]string{"context.measured", "item.completed assistant first half", "context.measured", "item.completed assistant second half",
				"turn.ended failed turn: the response reached max_tokens after 1 continuations"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := harnesstest.New(t, tc.steps...)
			a, log := runOn(t, &anthropic.Client{APIKey: "k", BaseURL: s.URL()}, turn.Limits{Retries: 1, Continuations: 1})
			head := uint64(len(lines(t, log, 0)))
			converse(t, a, "charlie")
			if got, want := lines(t, log, head), append(started, tc.want...); !slices.Equal(got, want) {
				t.Errorf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// modelCall is one model call of a wire: its events, one each tick, then a clean end.
type modelCall struct{ events []provider.Event }

// wire is a provider that answers each model call with the next call.
type wire struct {
	calls []modelCall
	mu    sync.Mutex
}

func (*wire) Name() string { return "anthropic" }

func (w *wire) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.calls) == 0 {
		return nil, errors.New("wire: no call left")
	}
	c := w.calls[0]
	w.calls = w.calls[1:]
	return &stream{ctx: ctx, c: c}, nil
}

type stream struct {
	ctx context.Context
	c   modelCall
}

func (s *stream) Next() (provider.Event, error) {
	if len(s.c.events) == 0 {
		return provider.Event{}, io.EOF
	}
	tick := time.NewTimer(100 * time.Millisecond)
	defer tick.Stop()
	select {
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	case <-tick.C:
	}
	ev := s.c.events[0]
	s.c.events = s.c.events[1:]
	return ev, nil
}

func (*stream) Close() error { return nil }

func TestModelStreams(t *testing.T) {
	done := func(stop provider.StopReason, parts ...message.Part) provider.Event {
		return provider.Event{Type: provider.EventDone, StopReason: stop, Message: &message.Message{Role: message.RoleAssistant, Parts: parts}}
	}
	say := func(text string, stop provider.StopReason) modelCall {
		return modelCall{events: []provider.Event{done(stop, &message.Text{Text: text})}}
	}
	write := func(args string, stop provider.StopReason) modelCall {
		return modelCall{events: []provider.Event{done(stop, &message.ToolCall{CallID: "c1", Name: "write_file", Arguments: json.RawMessage(args)})}}
	}
	activity := modelCall{events: slices.Repeat([]provider.Event{{Type: provider.EventActivity}}, 15)}
	activity.events = append(activity.events, done(provider.StopEndTurn, &message.Text{Text: "written"}))
	const notRun, noTool = "item.completed tool c1 not run: the response was cut off at its output limit", "item.completed tool c1 no such tool available: write_file"
	for _, tc := range []struct {
		name          string
		continuations int
		calls         []modelCall
		want          []string
	}{
		{"tool arguments that stream past the idle limit keep the call alive", 1, []modelCall{activity},
			[]string{"item.completed assistant written", "turn.ended completed"}},
		{"a tool call with arguments cut off at max_tokens does not run", 1,
			[]modelCall{write(`{"path":"a","content":"abc`, provider.StopMaxTokens), say("ok", provider.StopEndTurn)},
			[]string{"item.completed assistant c1", notRun, "item.completed assistant ok", "turn.ended completed"}},
		{"a response that is not cut off keeps the continuations spent", 1,
			[]modelCall{say("a", provider.StopMaxTokens), write("{}", provider.StopToolUse), say("b", provider.StopMaxTokens)},
			[]string{"item.completed assistant a", "item.completed assistant c1", noTool, "item.completed assistant b",
				"turn.ended failed turn: the response reached max_tokens after 1 continuations"}},
		{"negative continuations end the turn at the cut", -1, []modelCall{say("a", provider.StopMaxTokens)},
			[]string{"item.completed assistant a", "turn.ended completed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, log := runOn(t, &wire{calls: tc.calls}, turn.Limits{Retries: 1, Continuations: tc.continuations, Idle: 500 * time.Millisecond})
				head := uint64(len(lines(t, log, 0)))
				converse(t, a, "charlie")
				if got, want := lines(t, log, head), append([]string{"input.admitted", "turn.started"}, tc.want...); !slices.Equal(got, want) {
					t.Errorf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
				}
			})
		})
	}
}
