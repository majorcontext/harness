package turn_test

import (
	"context"
	"errors"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// recorder is the turn.Turn of one turn: it needs no turn ID, because it is
// bound to its turn.
type recorder struct {
	items    []eventlog.Message
	statuses []string
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
