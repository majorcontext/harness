package session

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// kindBackend answers each model call at once, except a call of a held
// kind, which waits for its ctx or for release. It names each call by the
// kind of run that makes it.
type kindBackend struct {
	mu      sync.Mutex
	held    map[runKind]bool
	started chan runKind
	gate    chan struct{}
	// usage is the input tokens that a call of each kind reports.
	usage map[runKind]int64
	// tokens is the prompt size that each call reports as a context
	// reading with no window, and window is the window that the backend
	// reports for every model.
	tokens int64
	window int
	// windows gives a model its own window, in the capabilities and in the
	// reading of each of its calls.
	windows map[string]int
	calls   []runKind
	reqs    map[runKind]turn.Request
	// fail is the error that a call of each kind returns after it reports
	// its usage.
	fail map[runKind]error
	// overflow makes the first call of a turn answer ErrContextOverflow.
	overflow bool
}

func (b *kindBackend) Capabilities(model string) turn.Capabilities {
	if w, ok := b.windows[model]; ok {
		return turn.Capabilities{ContextWindow: w}
	}
	return turn.Capabilities{ContextWindow: b.window}
}

func (b *kindBackend) called() []runKind {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

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
	b.calls = append(b.calls, kind)
	if b.reqs == nil {
		b.reqs = map[runKind]turn.Request{}
	}
	b.reqs[kind] = req
	b.mu.Unlock()
	if held {
		b.started <- kind
		select {
		case <-b.gate:
		case <-ctx.Done():
			return turn.Result{}, context.Cause(ctx)
		}
	}
	t := turn.Telemetry{Usage: eventlog.Usage{InputTokens: b.usage[kind]}}
	if kind == kindTurn && b.tokens > 0 {
		t.Context = eventlog.ContextMeasured{Tokens: b.tokens, Window: int64(b.windows[req.Model]), Source: "m"}
	}
	out.Telemetry(t)
	if err := b.fail[kind]; err != nil {
		return turn.Result{}, err
	}
	b.mu.Lock()
	over := kind == kindTurn && b.overflow
	b.overflow = false
	b.mu.Unlock()
	if over {
		return turn.Result{}, turn.ErrContextOverflow
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

func TestEveryModelCallExceptAnEvaluationCountsInTheUsageOfTheSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &kindBackend{held: map[runKind]bool{}, started: make(chan runKind, 1), gate: make(chan struct{}),
			usage: map[runKind]int64{kindTurn: 1, kindCompaction: 20, kindJudge: 300}}
		cfg := actorConfig(t, &memLog{}, owned{}, b)
		cfg.Evaluator, cfg.KeepTurns, cfg.Threshold = "m/eval", 1, 0.8
		a, err := Create(context.Background(), cfg, eventlog.SessionCreated{Model: "m/m"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		converse(t, a, "one", "two")
		if _, _, err := a.Compact(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if err := a.SetGoal(context.Background(), "say done", 0); err != nil {
			t.Fatal(err)
		}
		settled(a)
		if got, want := a.View().Session.Usage.InputTokens, int64(1+1+20+1); got != want {
			t.Errorf("input tokens = %d, want %d: two turns, a summary, and a goal turn, but not the evaluation", got, want)
		}
	})
}

func TestTheViewShowsTheGaugeTheLastTurnCompactionsAndSubscriptionUsage(t *testing.T) {
	sub := &eventlog.SubscriptionUsage{Provider: "claude", CapturedAt: 7, Windows: []eventlog.SubscriptionUsageWindow{{Key: "five_hour", Label: "5-hour", UsedPercent: 40, ResetsAt: 9}}}
	log := encode(t, eventlog.SessionCreated{Model: "m/m"}, eventlog.OwnerAcquired{Epoch: 1}, *firstInput(),
		eventlog.TurnStarted{TurnID: "turn_1", InputIDs: []string{"in1"}},
		eventlog.ContextMeasured{Tokens: 700, Window: 1000, Source: "m", SubscriptionUsage: sub},
		eventlog.TurnEnded{TurnID: "turn_1", StopReason: eventlog.StopFailed, Error: "boom"},
		eventlog.CompactionApplied{FromSeq: 1, ToSeq: 3, Summary: "s"})
	s, err := Load(context.Background(), "s1", log)
	if err != nil {
		t.Fatal(err)
	}
	v := Describe("s1", s, 0)
	if v.Context != (protocol.Context{Tokens: 700, Window: 1000}) {
		t.Errorf("Context = %+v", v.Context)
	}
	if v.LastTurn == nil || *v.LastTurn != (protocol.LastTurn{TurnID: "turn_1", StopReason: "failed", Error: "boom"}) {
		t.Errorf("LastTurn = %+v", v.LastTurn)
	}
	if v.CompactionCount != 1 {
		t.Errorf("CompactionCount = %d", v.CompactionCount)
	}
	want := &protocol.SubscriptionUsage{Provider: "claude", CapturedAt: 7, Windows: []protocol.SubscriptionUsageWindow{{Key: "five_hour", Label: "5-hour", UsedPercent: 40, ResetsAt: 9}}}
	if !reflect.DeepEqual(v.SubscriptionUsage, want) {
		t.Errorf("SubscriptionUsage = %+v, want %+v", v.SubscriptionUsage, want)
	}
}

func TestSideCallsKeepTheParametersOfTheEngine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &kindBackend{held: map[runKind]bool{}, started: make(chan runKind, 1), gate: make(chan struct{})}
		cfg := actorConfig(t, &memLog{}, owned{}, b)
		cfg.Evaluator, cfg.KeepTurns, cfg.Threshold = "m/eval", 1, 0.8
		a, err := Create(context.Background(), cfg, eventlog.SessionCreated{Model: "m/m", Settings: eventlog.Settings{Effort: "high"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		converse(t, a, "one", "two")
		if _, _, err := a.Compact(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if err := a.SetGoal(context.Background(), "say done", 0); err != nil {
			t.Fatal(err)
		}
		settled(a)
		if r := b.reqs[kindJudge]; r.MaxTokens != 256 || r.Settings.Effort != "off" {
			t.Errorf("evaluator request: MaxTokens %d, effort %q, want 256 and off", r.MaxTokens, r.Settings.Effort)
		}
		if r := b.reqs[kindCompaction]; r.MaxTokens != 1024 || r.Settings.Effort != "high" {
			t.Errorf("summary request: MaxTokens %d, effort %q, want 1024 and the session effort", r.MaxTokens, r.Settings.Effort)
		}
		if r := b.reqs[kindTurn]; r.MaxTokens != 0 {
			t.Errorf("turn request MaxTokens = %d, want the backend default (0)", r.MaxTokens)
		}
	})
}

func TestTheWindowOfTheModelBacksUpAReadingWithNone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &kindBackend{held: map[runKind]bool{}, started: make(chan runKind, 1), gate: make(chan struct{}), tokens: 900, window: 1000}
		cfg := actorConfig(t, &memLog{}, owned{}, b)
		cfg.KeepTurns, cfg.Threshold = 1, 0.8
		a, err := Create(context.Background(), cfg, eventlog.SessionCreated{Model: "m/m"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		converse(t, a, "one", "two", "three")
		synctest.Wait()
		if got, want := b.called(), []runKind{kindTurn, kindTurn, kindCompaction, kindTurn}; !slices.Equal(got, want) {
			t.Errorf("model calls = %v, want %v: a reading of 900 tokens in a window of 1000 compacts before the third turn", got, want)
		}
	})
}

func TestTheWindowOfTheNewModelReplacesThatOfAnOlderReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &kindBackend{held: map[runKind]bool{}, started: make(chan runKind, 1), gate: make(chan struct{}), tokens: 900,
			windows: map[string]int{"big/m": 2000, "small/m": 1000}}
		cfg := actorConfig(t, &memLog{}, owned{}, b)
		cfg.KeepTurns, cfg.Threshold = 1, 0.8
		a, err := Create(context.Background(), cfg, eventlog.SessionCreated{Model: "big/m"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		converse(t, a, "one", "two")
		small := "small/m"
		if err := a.Update(context.Background(), eventlog.SettingsChanged{Model: &small}); err != nil {
			t.Fatal(err)
		}
		converse(t, a, "three")
		synctest.Wait()
		if got, want := b.called(), []runKind{kindTurn, kindTurn, kindCompaction, kindTurn}; !slices.Equal(got, want) {
			t.Errorf("model calls = %v, want %v: 900 tokens pass 0.8 of the 1000 tokens of the new model, not of the 2000 of the older reading", got, want)
		}
	})
}

func TestTheViewTakesTheWindowOfTheModelBeforeThatOfTheReading(t *testing.T) {
	read := encode(t, eventlog.SessionCreated{Model: "m/m"}, eventlog.OwnerAcquired{Epoch: 1}, *firstInput(),
		eventlog.TurnStarted{TurnID: "turn_1", InputIDs: []string{"in1"}},
		eventlog.ContextMeasured{Tokens: 700, Window: 2000, Source: "m"})
	unread := encode(t, eventlog.SessionCreated{Model: "m/m"}, eventlog.OwnerAcquired{Epoch: 1})
	for _, tc := range []struct {
		name   string
		log    *memLog
		window int
		want   protocol.Context
	}{
		{"a model window replaces that of an older reading", read, 1000, protocol.Context{Tokens: 700, Window: 1000}},
		{"a reading window serves a model with none", read, 0, protocol.Context{Tokens: 700, Window: 2000}},
		{"a model window shows before any reading", unread, 1000, protocol.Context{Window: 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Load(context.Background(), "s1", tc.log)
			if err != nil {
				t.Fatal(err)
			}
			if got := Describe("s1", s, tc.window).Context; got != tc.want {
				t.Errorf("Context = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAFailedSummaryKeepsItsUsageInTheSession(t *testing.T) {
	boom := errors.New("summary failed")
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, a *Actor, b *kindBackend)
	}{
		{"manual compaction", func(t *testing.T, a *Actor, _ *kindBackend) {
			if _, _, err := a.Compact(context.Background(), 0); !errors.Is(err, boom) {
				t.Fatalf("Compact error = %v, want the summary error", err)
			}
		}},
		{"overflow inside a turn", func(t *testing.T, a *Actor, b *kindBackend) {
			b.overflow = true
			converse(t, a, "three")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := &kindBackend{held: map[runKind]bool{}, started: make(chan runKind, 1), gate: make(chan struct{}),
					usage: map[runKind]int64{kindTurn: 1, kindCompaction: 20}}
				cfg := actorConfig(t, &memLog{}, owned{}, b)
				cfg.KeepTurns = 1
				a, err := Create(context.Background(), cfg, eventlog.SessionCreated{Model: "m/m"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				a.Run()
				converse(t, a, "one", "two")
				b.fail = map[runKind]error{kindCompaction: boom}
				tc.run(t, a, b)
				synctest.Wait()
				if got := a.View().Session.Usage.InputTokens; got < 1+1+20 {
					t.Errorf("input tokens = %d, want the 20 of the failed summary counted", got)
				}
			})
		})
	}
}
