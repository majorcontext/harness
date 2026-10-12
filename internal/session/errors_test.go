package session

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/internal/backend/modelapi"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/turn"
)

// runOn runs session s1 on an Anthropic model served by p. Each compaction keeps one turn.
func runOn(t *testing.T, p provider.Provider, lim turn.Limits) (*Actor, *memLog) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	log, b := &memLog{}, modelapi.New(p, 0)
	a, err := Create(ctx, Config{ID: "s1", Store: log, Ownership: owned{}, Backend: b, Limits: lim, Threshold: 0.8, KeepTurns: 1,
		Base: ctx, Go: wg.Go, Done: func(closeDone func()) { closeDone() }, Prompt: func() string { return "" }}, eventlog.SessionCreated{Model: "anthropic/claude-opus-5"}, nil)
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
	activity := modelCall{events: slices.Repeat([]provider.Event{{Type: provider.EventActivity}}, 15)}
	activity.events = append(activity.events, provider.Event{Type: provider.EventDone, StopReason: provider.StopEndTurn,
		Message: &message.Message{Role: message.RoleAssistant, Parts: []message.Part{&message.Text{Text: "written"}}}})
	synctest.Test(t, func(t *testing.T) {
		a, log := runOn(t, &wire{calls: []modelCall{activity}}, turn.Limits{Retries: 1, Idle: 500 * time.Millisecond})
		head := uint64(len(lines(t, log, 0)))
		converse(t, a, "charlie")
		want := []string{"input.admitted", "turn.started", "item.completed assistant written", "turn.ended completed"}
		if got := lines(t, log, head); !slices.Equal(got, want) {
			t.Errorf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}
