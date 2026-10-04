package session

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// heldBackend signals ran when a turn starts, then waits for gate. With
// stubborn, it ignores the end of the turn context, as an external harness
// does during its grace.
type heldBackend struct {
	ran      chan struct{}
	gate     chan struct{}
	stubborn bool
}

func newHeldBackend(stubborn bool) *heldBackend {
	return &heldBackend{ran: make(chan struct{}, 1), gate: make(chan struct{}), stubborn: stubborn}
}

func (*heldBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (b *heldBackend) Run(ctx context.Context, _ turn.Request, out turn.Sink) (turn.Result, error) {
	b.ran <- struct{}{}
	done := ctx.Done()
	if b.stubborn {
		done = nil
	}
	select {
	case <-b.gate:
	case <-done:
		return turn.Result{}, context.Cause(ctx)
	}
	return turn.Result{}, out.Item(eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "ok"}}})
}

// lease is an Ownership that the test revokes by closing lost.
type lease struct {
	lost     chan struct{}
	released atomic.Bool
}

func (*lease) Epoch() uint64           { return 1 }
func (l *lease) Lost() <-chan struct{} { return l.lost }
func (l *lease) Release()              { l.released.Store(true) }

func actorConfig(t *testing.T, log Storage, own Ownership, b turn.Backend) Config {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	return Config{ID: "s1", Store: log, Ownership: own, Backend: b, Base: ctx, Go: wg.Go, Done: func() {},
		Prompt: func() string { return "" }}
}

func firstInput() *eventlog.InputAdmitted {
	return &eventlog.InputAdmitted{InputID: "in1", Delivery: eventlog.DeliveryQueue, Source: "user",
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "hi"}}}
}

func encode(t *testing.T, events ...eventlog.Event) *memLog {
	t.Helper()
	l := &memLog{}
	for i, e := range events {
		data, err := eventlog.Envelope{Seq: uint64(i) + 1, Time: time.Now(), Event: e}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		l.recs = append(l.recs, eventlog.Record{Seq: uint64(i) + 1, Data: data})
	}
	return l
}

func TestNoTurnRunsBeforeRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(t *testing.T, b turn.Backend) (*Actor, error)
	}{
		{"Create with a first input", func(t *testing.T, b turn.Backend) (*Actor, error) {
			cfg := actorConfig(t, &memLog{}, owned{}, b)
			return Create(context.Background(), cfg, eventlog.SessionCreated{Model: "m/m"}, firstInput())
		}},
		{"Open of a suspended turn", func(t *testing.T, b turn.Backend) (*Actor, error) {
			log := encode(t, eventlog.SessionCreated{Model: "m/m"}, eventlog.OwnerAcquired{Epoch: 1}, *firstInput(),
				eventlog.TurnStarted{TurnID: "turn_1", InputIDs: []string{"in1"}}, eventlog.TurnSuspended{TurnID: "turn_1", Cause: eventlog.CauseHandoff})
			return Open(context.Background(), actorConfig(t, log, owned{}, b))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := newHeldBackend(false)
				a, err := tc.start(t, b)
				if err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				select {
				case <-b.ran:
					t.Fatal("the turn ran before Run")
				default:
				}
				a.Run()
				<-b.ran
				close(b.gate)
			})
		})
	}
}

func TestAStoppedActorReleasesAfterItsTurnExits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, own := newHeldBackend(true), &lease{lost: make(chan struct{})}
		a, err := Create(context.Background(), actorConfig(t, &memLog{}, own, b), eventlog.SessionCreated{Model: "m/m"}, firstInput())
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		<-b.ran
		close(own.lost)
		synctest.Wait()
		select {
		case <-a.Done():
			t.Fatal("Done closed while the turn still runs")
		default:
		}
		if own.released.Load() {
			t.Fatal("the ownership was released while the turn still runs")
		}
		close(b.gate)
		<-a.Done()
		if !own.released.Load() {
			t.Fatal("the ownership was not released after the turn exited")
		}
	})
}

func TestAppendedSeesTheStateThatTheAppendProduced(t *testing.T) {
	cfg := actorConfig(t, &memLog{}, owned{}, newHeldBackend(false))
	var heads []uint64
	cfg.Appended = func(_ []eventlog.Event, st *eventlog.State) { heads = append(heads, st.Head()) }
	if _, err := Create(context.Background(), cfg, eventlog.SessionCreated{Model: "m/m"}, nil); err != nil {
		t.Fatal(err)
	}
	if want := []uint64{2}; !slices.Equal(heads, want) {
		t.Errorf("heads seen by Appended = %v, want %v", heads, want)
	}
}

func TestReadGivesTheLiveStateToTheCallerAndStopsWithTheActor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, own := newHeldBackend(false), &lease{lost: make(chan struct{})}
		a, err := Create(context.Background(), actorConfig(t, &memLog{}, own, b), eventlog.SessionCreated{Model: "m/m"}, firstInput())
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		<-b.ran
		var turnID string
		if err := a.Read(context.Background(), func(st *eventlog.State) { t, _ := st.Turn(); turnID = t.ID }); err != nil || turnID == "" {
			t.Errorf("Read during a turn = %v, turn %q, want the running turn", err, turnID)
		}
		close(own.lost)
		synctest.Wait()
		if err := a.Read(context.Background(), func(*eventlog.State) { t.Error("Read ran after the actor stopped") }); !errors.Is(err, ErrNotOwned) {
			t.Errorf("Read after the actor stopped = %v, want ErrNotOwned", err)
		}
		close(b.gate)
		<-a.Done()
	})
}
