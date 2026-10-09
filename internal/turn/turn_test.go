package turn_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	started  int
	deltas   []string
	// folded is the history that CompactTurn returns. nil: nothing folds.
	folded []eventlog.Message
}

func (r *recorder) Item(m eventlog.Message) error { r.items = append(r.items, m); return nil }
func (r *recorder) Delta(id string, _ turn.Delta) { r.deltas = append(r.deltas, id) }
func (r *recorder) Started() string {
	r.started++
	return fmt.Sprintf("item_%d", r.started)
}
func (*recorder) Attach(string, []byte) (eventlog.Part, error) {
	return eventlog.Part{}, nil
}
func (*recorder) Alive()                   {}
func (*recorder) Telemetry(turn.Telemetry) {}
func (r *recorder) Steer() ([]eventlog.Message, error) {
	if len(r.steer) == 0 {
		return nil, nil
	}
	in := r.steer[0]
	r.steer = r.steer[1:]
	return in, nil
}
func (*recorder) Settings() (string, eventlog.Settings)     { return "", eventlog.Settings{} }
func (*recorder) Pin([]turn.Notice, int) []turn.Pin         { return nil }
func (*recorder) Ask(string, string, json.RawMessage) error { return nil }
func (*recorder) Resolution(string) (eventlog.RequestResolved, bool) {
	return eventlog.RequestResolved{}, false
}
func (*recorder) State(string) (turn.Snapshot, error)   { return turn.Snapshot{}, nil }
func (*recorder) SaveState(string, turn.Snapshot) error { return nil }
func (*recorder) Compacted(string) error                { return nil }
func (r *recorder) Status(f protocol.StatusFrame) {
	r.statuses = append(r.statuses, string(f.Status))
	if f.Status == protocol.StatusRetrying {
		r.waits = append(r.waits, time.Until(f.NextAt))
	}
}
func (r *recorder) CompactTurn(context.Context) ([]eventlog.Message, bool, error) {
	return r.folded, r.folded != nil, nil
}
func (r *recorder) Ended(err error) { r.ended = append(r.ended, err) }

// model is a native backend: one scripted reply for each model call.
type model struct {
	replies  []eventlog.Message
	results  []turn.Result
	errs     []error
	requests []turn.Request
	// streams makes each call stream one delta before it fails or answers.
	streams bool
}

func (m *model) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (m *model) Run(_ context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	m.requests = append(m.requests, req)
	i := len(m.requests) - 1
	if m.streams {
		out.Delta("backend", turn.Delta{Type: eventlog.PartText, Text: "t"})
	}
	if i < len(m.errs) && m.errs[i] != nil {
		return turn.Result{}, m.errs[i]
	}
	var res turn.Result
	if i < len(m.results) {
		res = m.results[i]
	}
	return res, out.Item(m.replies[i])
}

func say(text string) eventlog.Message {
	return eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
}

func callTool(id string) eventlog.Message {
	return eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartToolCall, CallID: id, Name: "x"}}}
}

func steerInput(text string) eventlog.Message {
	return eventlog.Message{Role: eventlog.RoleUser, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
}

func TestRunTakesNoSteerInputWhenAMaxTokensStopEndsTheTurn(t *testing.T) {
	m := &model{replies: []eventlog.Message{callTool("c1")}, results: []turn.Result{{MaxTokens: true}}}
	r := &recorder{steer: [][]eventlog.Message{{steerInput("later")}}}
	ctx := context.Background()
	turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, nil, r, turn.Limits{})
	if len(m.requests) != 1 || len(r.ended) != 1 || r.ended[0] != nil {
		t.Fatalf("calls = %d, ended = %v, want one call and a clean end", len(m.requests), r.ended)
	}
	if len(r.steer) != 1 {
		t.Fatalf("a steer input was taken by a turn that makes no further model call")
	}
}

func TestRunTakesNoSteerInputWhenTheContinuationLimitFailsTheTurn(t *testing.T) {
	m := &model{
		replies: []eventlog.Message{callTool("c1"), callTool("c2")},
		results: []turn.Result{{MaxTokens: true}, {MaxTokens: true}},
	}
	r := &recorder{steer: [][]eventlog.Message{nil, {steerInput("later")}}}
	ctx := context.Background()
	turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, nil, r, turn.Limits{Continuations: 1})
	if len(m.requests) != 2 || len(r.ended) != 1 || r.ended[0] == nil {
		t.Fatalf("calls = %d, ended = %v, want two calls and a failed turn", len(m.requests), r.ended)
	}
	if len(r.steer) != 1 {
		t.Fatalf("a steer input was taken by a turn that makes no further model call")
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
		turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, nil, r, turn.Limits{Retries: calls})
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

func TestRunStartsOneItemForEachCallThatStreams(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		lim  turn.Limits
		r    *recorder
	}{
		{"a retried call", turn.ErrRetryable, turn.Limits{Retries: 1}, &recorder{}},
		{"a call that overflows the context", turn.ErrContextOverflow, turn.Limits{}, &recorder{folded: []eventlog.Message{steerInput("summary")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := &model{replies: []eventlog.Message{{}, say("ok")}, errs: []error{tc.err}, streams: true}
				ctx := context.Background()
				turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, nil, tc.r, tc.lim)
				if len(tc.r.ended) != 1 || tc.r.ended[0] != nil {
					t.Fatalf("Ended calls = %v, want one with no error", tc.r.ended)
				}
				if want := []string{"item_1", "item_2"}; !slices.Equal(tc.r.deltas, want) {
					t.Fatalf("deltas named %v, want %v: the failed call abandoned item_1", tc.r.deltas, want)
				}
			})
		})
	}
}
