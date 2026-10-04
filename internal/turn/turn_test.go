package turn_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// recorder is the turn.Turn of one turn: it needs no turn ID, because it is
// bound to its turn.
type recorder struct {
	items    []eventlog.Message
	statuses []string
	waits    []time.Duration
	ended    []error
	steer    [][]eventlog.Message
}

func (r *recorder) Item(m eventlog.Message) error { r.items = append(r.items, m); return nil }
func (*recorder) Delta(string, turn.Delta)        {}
func (*recorder) Alive()                          {}
func (*recorder) Telemetry(turn.Telemetry)        {}
func (r *recorder) Steer() ([]eventlog.Message, error) {
	if len(r.steer) == 0 {
		return nil, nil
	}
	in := r.steer[0]
	r.steer = r.steer[1:]
	return in, nil
}
func (*recorder) State(string) ([]byte, error)   { return nil, nil }
func (*recorder) SaveState(string, []byte) error { return nil }
func (*recorder) Compacted(string) error         { return nil }
func (r *recorder) Status(f protocol.StatusFrame) {
	r.statuses = append(r.statuses, string(f.Status))
	if f.Status == protocol.StatusRetrying {
		r.waits = append(r.waits, time.Until(f.NextAt))
	}
}
func (*recorder) CompactTurn(context.Context) ([]eventlog.Message, bool, error) {
	return nil, false, nil
}
func (r *recorder) Ended(err error) { r.ended = append(r.ended, err) }

// model is a native backend: one scripted reply for each model call.
type model struct {
	replies  []eventlog.Message
	errs     []error
	requests []turn.Request
}

func (m *model) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (m *model) Run(_ context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	m.requests = append(m.requests, req)
	i := len(m.requests) - 1
	if i < len(m.errs) && m.errs[i] != nil {
		return turn.Result{}, m.errs[i]
	}
	return turn.Result{}, out.Item(m.replies[i])
}

func say(text string) eventlog.Message {
	return eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
}

func TestRunReportsItsItemsAndEndThroughOneTurn(t *testing.T) {
	m, r := &model{replies: []eventlog.Message{say("hi")}}, &recorder{}
	ctx := context.Background()
	turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, nil, nil, r, turn.Limits{})
	if len(r.items) != 1 || r.items[0].Parts[0].Text != "hi" {
		t.Fatalf("items = %+v, want the reply", r.items)
	}
	if len(r.ended) != 1 || r.ended[0] != nil {
		t.Fatalf("Ended calls = %v, want one with no error", r.ended)
	}
}

func TestRunReportsAFailureToEnded(t *testing.T) {
	boom := errors.New("boom")
	m, r := &model{errs: []error{boom}}, &recorder{}
	ctx := context.Background()
	turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, nil, nil, r, turn.Limits{})
	if len(r.ended) != 1 || !errors.Is(r.ended[0], boom) {
		t.Fatalf("Ended calls = %v, want boom", r.ended)
	}
}

func TestRetryBackoffDoublesFromOneSecondToAnEightSecondCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const calls = 6
		m := &model{replies: make([]eventlog.Message, calls+1), errs: make([]error, calls)}
		for i := range m.errs {
			m.errs[i] = turn.ErrRetryable
		}
		m.replies[calls] = say("ok")
		r := &recorder{}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, nil, nil, r, turn.Limits{Retries: calls})
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
		if len(r.waits) != len(want) {
			t.Fatalf("waits = %v, want %v", r.waits, want)
		}
		for i := range want {
			if lo, hi := want[i]*9/10, want[i]*11/10; r.waits[i] < lo || r.waits[i] > hi {
				t.Errorf("wait %d = %v, want %v within the jitter", i+1, r.waits[i], want[i])
			}
		}
	})
}
