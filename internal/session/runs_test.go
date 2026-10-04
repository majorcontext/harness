package session

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// kindBackend answers each model call at once, except a call of a held
// kind, which waits for its ctx or for release. It names each call by the
// kind of run that makes it.
type kindBackend struct {
	mu      sync.Mutex
	held    map[runKind]bool
	started chan runKind
	gate    chan struct{}
}

func (*kindBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (b *kindBackend) hold(k runKind) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held[k] = true
}

func (b *kindBackend) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	kind, text := kindTurn, "ok"
	switch req.Instructions {
	case evaluatorPrompt:
		kind, text = kindJudge, "MET: ok"
	case "":
	default:
		kind, text = kindCompaction, "summary"
	}
	b.mu.Lock()
	held := b.held[kind]
	b.mu.Unlock()
	if held {
		b.started <- kind
		select {
		case <-b.gate:
		case <-ctx.Done():
			return turn.Result{}, context.Cause(ctx)
		}
	}
	return turn.Result{}, out.Item(eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}})
}

func runOf(t *testing.T, a *Actor) (kind runKind, none bool) {
	t.Helper()
	type run struct {
		kind runKind
		none bool
	}
	r, err := call(context.Background(), a, func(reply func(run, error)) {
		if a.run == nil {
			reply(run{none: true}, nil)
			return
		}
		reply(run{kind: a.run.kind}, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	return r.kind, r.none
}

func TestEachKindOfRunAnswersTheSameCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind runKind
		// stops reports that an interrupt ends the run.
		stops bool
	}{
		{"a turn", kindTurn, true},
		{"a compaction", kindCompaction, true},
		{"a goal evaluation", kindJudge, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := &kindBackend{held: map[runKind]bool{}, started: make(chan runKind, 1), gate: make(chan struct{})}
				log := &memLog{}
				cfg := actorConfig(t, log, owned{}, b)
				cfg.Evaluator, cfg.KeepTurns, cfg.Threshold = "m/eval", 1, 0.8
				a, err := Create(context.Background(), cfg, eventlog.SessionCreated{Model: "m/m"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				a.Run()
				converse(t, a, "one", "two")
				b.hold(tc.kind)
				compacted := make(chan error, 1)
				switch tc.kind {
				case kindTurn:
					submitHi(t, a)
				case kindCompaction:
					go func() { _, _, err := a.Compact(context.Background(), 0); compacted <- err }()
				case kindJudge:
					if err := a.SetGoal(context.Background(), "say done", 0); err != nil {
						t.Fatal(err)
					}
				}
				if got := <-b.started; got != tc.kind {
					t.Fatalf("model call of kind %q, want %q", got, tc.kind)
				}
				if got, none := runOf(t, a); none || got != tc.kind {
					t.Fatalf("the run of the actor is kind %q (none %t), want %q", got, none, tc.kind)
				}
				if _, _, err := a.Compact(context.Background(), 0); !errors.Is(err, ErrBusy) {
					t.Errorf("Compact during the run = %v, want ErrBusy", err)
				}
				if err := a.Interrupt(context.Background(), ""); err != nil {
					t.Fatalf("Interrupt: %v", err)
				}
				got, none := runOf(t, a)
				switch {
				case tc.stops && !none:
					t.Errorf("the run of kind %q still runs after an interrupt", got)
				case !tc.stops && (none || got != tc.kind):
					t.Errorf("an interrupt ended the run of kind %q", tc.kind)
				}
				close(b.gate)
				synctest.Wait()
				if tc.kind == kindCompaction {
					<-compacted
					if appliedCompaction(a) {
						t.Error("an interrupted compaction appended compaction.applied")
					}
				}
				if got, want := slices.Contains(lines(t, log, 2), "turn.ended interrupted stopped"), tc.kind == kindTurn; got != want {
					t.Errorf("log holds an interrupted turn = %t, want %t", got, want)
				}
			})
		})
	}
}

func appliedCompaction(a *Actor) bool {
	ok, _ := call(context.Background(), a, func(reply func(bool, error)) {
		_, ok := a.state.Compaction()
		reply(ok, nil)
	})
	return ok
}

func submitText(t *testing.T, a *Actor, id, text string) {
	t.Helper()
	in := eventlog.InputAdmitted{InputID: id, Delivery: eventlog.DeliveryQueue, Source: "user", Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
	if _, _, err := a.Submit(context.Background(), in, ""); err != nil {
		t.Fatal(err)
	}
}

func TestAFailedTurnLeavesItsQueuedInputToRunExceptAtAUsageLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		ended string
		runs  bool
	}{
		{"a failed turn", errors.New("bad request"), "turn.ended failed bad request", true},
		{"a usage limit", turn.ErrExhausted, "turn.ended failed provider_exhausted", false},
	} {
		t.Run(tc.name+" live", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				log, b := &memLog{}, &goalBackend{errs: []error{tc.err, nil}, gated: true, gate: make(chan struct{})}
				a := goalActor(t, log, b, true, new(atomic.Int32))
				submitText(t, a, "one", "one")
				submitText(t, a, "two", "two")
				close(b.gate)
				synctest.Wait()
				want := []string{"input.admitted", "turn.started", "input.admitted", tc.ended}
				if tc.runs {
					want = append(want, "turn.started", "item.completed assistant re two", "turn.ended completed")
				}
				if got := lines(t, log, 2); !slices.Equal(got, want) {
					t.Errorf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
				}
			})
		})
		t.Run(tc.name+" after a restart", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ended := eventlog.TurnEnded{TurnID: "turn_1", StopReason: eventlog.StopFailed, Error: "bad request"}
				if !tc.runs {
					ended.Error = string(eventlog.CauseProviderExhausted)
				}
				log := encode(t, eventlog.SessionCreated{Model: "m/m"}, eventlog.OwnerAcquired{Epoch: 1}, *firstInput(),
					eventlog.TurnStarted{TurnID: "turn_1", InputIDs: []string{"in1"}},
					eventlog.InputAdmitted{InputID: "in2", Delivery: eventlog.DeliveryQueue, Source: "user", Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "two"}}},
					ended)
				b := newHeldBackend(false)
				a, err := Open(context.Background(), actorConfig(t, log, owned{}, b))
				if err != nil {
					t.Fatal(err)
				}
				a.Run()
				synctest.Wait()
				select {
				case <-b.ran:
					if !tc.runs {
						t.Error("Open ran the input that waited after a usage limit")
					}
					close(b.gate)
				default:
					if tc.runs {
						t.Error("Open left a queued input waiting after a failed turn")
					}
				}
			})
		})
	}
}
