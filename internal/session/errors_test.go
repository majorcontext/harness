package session

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/backend/modelapi"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/provider/anthropic"
)

// runOn runs session s1 on an Anthropic model served by s. Each compaction keeps one turn.
func runOn(t *testing.T, s *harnesstest.Server, lim turn.Limits) (*Actor, *memLog) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	log, b := &memLog{}, modelapi.New(&anthropic.Client{APIKey: "k", BaseURL: s.URL()}, 0)
	a, err := Create(ctx, Config{ID: "s1", Log: log, Ownership: owned{}, Backend: b, Limits: lim, Threshold: 0.8, KeepTurns: 1,
		Base: ctx, Go: wg.Go, Done: func() {}}, eventlog.SessionCreated{Model: "anthropic/claude-opus-5"})
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
	const nudge, overflowed = "auto-continue 1 of 1", "turn.ended failed turn: context overflow: context exhausted: prompt 205102 tokens > limit 200000"
	started := []string{"input.admitted", "turn.started"}
	for _, tc := range []struct {
		name   string
		before []string
		steps  []step
		want   []string
	}{
		{"a stalled stream runs the call again", nil,
			[]step{{Name: "stall", Reply: reply{Text: "partial", Block: true}}, answer("charlie")},
			[]string{"context.measured", "item.completed assistant re charlie", "turn.ended completed"}},
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
			[]step{answer("alpha"), answer("bravo"), overflow, summary, answer("charlie")},
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
			a, log := runOn(t, harnesstest.New(t, tc.steps...), turn.Limits{Retries: 1, Continuations: 1, Idle: time.Second})
			converse(t, a, tc.before...)
			head := uint64(len(lines(t, log, 0)))
			converse(t, a, "charlie")
			if got, want := lines(t, log, head), append(started, tc.want...); !slices.Equal(got, want) {
				t.Errorf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}
