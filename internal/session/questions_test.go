package session

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// askBackend reports Request.Questions of each turn on asked.
type askBackend struct{ asked chan bool }

func (askBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{OwnsLoop: true} }

func (b askBackend) Run(_ context.Context, req turn.Request, _ turn.Sink) (turn.Result, error) {
	b.asked <- req.Questions
	return turn.Result{}, nil
}

func TestOnlyASessionWithAUserAndNoGoalMayAsk(t *testing.T) {
	queued := *firstInput()
	for _, tc := range []struct {
		name   string
		option bool
		events []eventlog.Event
		want   bool
	}{
		{"the embedder answers", true, []eventlog.Event{eventlog.SessionCreated{Model: "m/m"}}, true},
		{"the embedder does not answer", false, []eventlog.Event{eventlog.SessionCreated{Model: "m/m"}}, false},
		{"a child session has no user", true, []eventlog.Event{eventlog.SessionCreated{Model: "m/m", ParentID: "p"}}, false},
		{"a goal session has no user at the turn", true, []eventlog.Event{eventlog.SessionCreated{Model: "m/m"}, eventlog.GoalSet{Condition: "done", MaxTurns: 3}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := askBackend{make(chan bool, 1)}
				log := encode(t, append(tc.events, eventlog.OwnerAcquired{Epoch: 1}, queued)...)
				cfg := actorConfig(t, log, owned{}, b)
				cfg.AskUserQuestion = tc.option
				a, err := Open(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				a.Run()
				if got := <-b.asked; got != tc.want {
					t.Errorf("Request.Questions = %t, want %t", got, tc.want)
				}
			})
		})
	}
}
