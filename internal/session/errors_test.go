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
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
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
	a, err := Create(ctx, Config{ID: "s1", Log: log, Ownership: owned{}, Backend: b, Limits: lim, Threshold: 0.8, KeepTurns: 1,
		Base: ctx, Go: wg.Go, Done: func() {}, Prompt: func() string { return "" }}, eventlog.SessionCreated{Model: "anthropic/claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
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
			f = append(f, string(e.StopReason), e.Error)
		case eventlog.GoalEvaluated:
			f = append(f, string(e.Verdict))
		case eventlog.GoalChanged:
			f = append(f, string(e.State))
		}
		out = append(out, strings.TrimSpace(strings.Join(f, " ")))
	}
	return out
}

func TestModelCallErrors(t *testing.T) {
	type step = harnesstest.Step
	type reply = harnesstest.Reply
	answer := func(in string) step {
		return step{Name: in, Match: harnesstest.LastUserText(in), Reply: reply{Text: "re " + in}}
	}
	cut := func(name, after string, calls ...harnesstest.ToolCall) step {
		return step{Name: name, Match: harnesstest.LastUserText(after), Reply: reply{Text: name, ToolCalls: calls, StopReason: "max_tokens"}}
	}
	overflow := step{Name: "overflow", Match: harnesstest.LastUserText("charlie"),
		Reply: reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}}
	summary := step{Name: "summary", Match: harnesstest.SystemContains("You are summarizing a prefix"), Reply: reply{Text: "sum"}}
	summarized := step{Name: "charlie", Reply: reply{Text: "re charlie"}, Match: func(r harnesstest.Request) bool {
		return r.Messages[0].Parts[0].Text == turn.SummaryBanner+"sum" && harnesstest.LastUserText("charlie")(r)
	}}
	const nudge, overflowed = "auto-continue 1 of 1", "turn.ended failed turn: context overflow: context exhausted: prompt 205102 tokens > limit 200000"
	started := []string{"input.admitted", "turn.started"}
	for _, tc := range []struct {
		name   string
		before []string
		steps  []step
		want   []string
	}{
		{"a response cut off at max_tokens continues", nil,
			[]step{cut("first half", "charlie"), {Name: "rest", Match: harnesstest.LastUserText(nudge), Reply: reply{Text: "second half"}}},
			[]string{"context.measured", "item.completed assistant first half", "context.measured", "item.completed assistant second half",
				"turn.ended completed"}},
		{"a tool call of a cut-off response does not run", nil,
			[]step{cut("call", "charlie", harnesstest.ToolCall{ID: "c1", Name: "write_file"}), {Name: "rest", Match: harnesstest.LastUserText(nudge), Reply: reply{Text: "ok"}}},
			[]string{"context.measured", "item.completed assistant call c1", "item.completed tool c1 not run: the response was cut off at its output limit", "context.measured",
				"item.completed assistant ok", "turn.ended completed"}},
		{"a cut-off response past the continuations fails the turn", nil,
			[]step{cut("first half", "charlie"), cut("second half", nudge)},
			[]string{"context.measured", "item.completed assistant first half", "context.measured", "item.completed assistant second half",
				"turn.ended failed turn: the response reached max_tokens after 1 continuations"}},
		{"a context overflow compacts and runs the turn once more", []string{"alpha", "bravo"},
			[]step{answer("alpha"), answer("bravo"), overflow, summary, summarized},
			[]string{"compaction.applied", "context.measured", "item.completed assistant re charlie", "turn.ended completed"}},
		{"a context overflow with no turn to fold fails the turn", nil, []step{overflow}, []string{overflowed}},
		{"a context overflow after the compaction fails the turn", []string{"alpha", "bravo"},
			[]step{answer("alpha"), answer("bravo"), overflow, summary, {Name: "again", Reply: overflow.Reply}},
			[]string{"compaction.applied", overflowed}},
		{"a provider usage limit ends the turn with its cause", nil,
			[]step{{Name: "limit", Reply: reply{HTTPStatus: 400, ErrorMessage: "You have reached your specified API usage limits."}}},
			[]string{"turn.ended failed provider_exhausted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := harnesstest.New(t, tc.steps...)
			a, log := runOn(t, &anthropic.Client{APIKey: "k", BaseURL: s.URL()}, turn.Limits{Retries: 1, Continuations: 1})
			converse(t, a, tc.before...)
			head := uint64(len(lines(t, log, 0)))
			converse(t, a, "charlie")
			if got, want := lines(t, log, head), append(started, tc.want...); !slices.Equal(got, want) {
				t.Errorf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// modelCall is one model call of a wire: its events, one each tick, then a clean
// end, or with hang a wait for the end of the call.
type modelCall struct {
	events []provider.Event
	hang   bool
	err    error
}

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
	return &stream{ctx: ctx, c: c}, c.err
}

type stream struct {
	ctx context.Context
	c   modelCall
}

func (s *stream) Next() (provider.Event, error) {
	if len(s.c.events) == 0 && !s.c.hang {
		return provider.Event{}, io.EOF
	}
	tick := time.NewTimer(100 * time.Millisecond)
	defer tick.Stop()
	select {
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	case <-tick.C:
	}
	if len(s.c.events) == 0 {
		<-s.ctx.Done()
		return provider.Event{}, s.ctx.Err()
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
	overflow := modelCall{err: &provider.Error{Kind: provider.ErrKindContextOverflow, Raw: "too long"}}
	const notRun, noTool = "item.completed tool c1 not run: the response was cut off at its output limit", "item.completed tool c1 no such tool available: write_file"
	for _, tc := range []struct {
		name          string
		continuations int
		before        []string
		calls         []modelCall
		want          []string
	}{
		{"a stalled stream runs the call again", 1, nil,
			[]modelCall{{events: []provider.Event{{Type: provider.EventTextDelta, Text: "partial"}}, hang: true}, say("re charlie", provider.StopEndTurn)},
			[]string{"item.completed assistant re charlie", "turn.ended completed"}},
		{"tool arguments that stream past the idle limit keep the call alive", 1, nil, []modelCall{activity},
			[]string{"item.completed assistant written", "turn.ended completed"}},
		{"a tool call with arguments cut off at max_tokens does not run", 1, nil,
			[]modelCall{write(`{"path":"a","content":"abc`, provider.StopMaxTokens), say("ok", provider.StopEndTurn)},
			[]string{"item.completed assistant c1", notRun, "item.completed assistant ok", "turn.ended completed"}},
		{"a response that is not cut off keeps the continuations spent", 1, nil,
			[]modelCall{say("a", provider.StopMaxTokens), write("{}", provider.StopToolUse), say("b", provider.StopMaxTokens)},
			[]string{"item.completed assistant a", "item.completed assistant c1", noTool, "item.completed assistant b",
				"turn.ended failed turn: the response reached max_tokens after 1 continuations"}},
		{"negative continuations end the turn at the cut", -1, nil, []modelCall{say("a", provider.StopMaxTokens)},
			[]string{"item.completed assistant a", "turn.ended completed"}},
		{"a stalled summary fails the overflowed turn", 1, []string{"alpha", "bravo"},
			[]modelCall{say("re alpha", provider.StopEndTurn), say("re bravo", provider.StopEndTurn), overflow, {hang: true}},
			[]string{"turn.ended failed turn: context overflow: too long"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, log := runOn(t, &wire{calls: tc.calls}, turn.Limits{Retries: 1, Continuations: tc.continuations, Idle: 500 * time.Millisecond})
				converse(t, a, tc.before...)
				head := uint64(len(lines(t, log, 0)))
				converse(t, a, "charlie")
				if got, want := lines(t, log, head), append([]string{"input.admitted", "turn.started"}, tc.want...); !slices.Equal(got, want) {
					t.Errorf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
				}
			})
		})
	}
}
