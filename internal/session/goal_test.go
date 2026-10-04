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
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// goalBackend answers an evaluator call with the next verdict, and any other
// call with "re" and the first line of its input, or the next error. With
// gated, a turn, or with gateJudge an evaluator call, waits for gate, which
// the test makes in its bubble. Each list repeats its last entry.
type goalBackend struct {
	mu               sync.Mutex
	verdicts         []string
	errs             []error
	gated, gateJudge bool
	gate             chan struct{}
}

func next[T any](mu *sync.Mutex, list *[]T) T {
	mu.Lock()
	defer mu.Unlock()
	var v T
	if len(*list) > 0 {
		v = (*list)[0]
	}
	if len(*list) > 1 {
		*list = (*list)[1:]
	}
	return v
}

func (*goalBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (b *goalBackend) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	text, judge := "", req.Instructions == evaluatorPrompt
	var fail error
	if judge {
		text = next(&b.mu, &b.verdicts)
	} else {
		fail = next(&b.mu, &b.errs)
		in, _, _ := strings.Cut(req.Input[0].Parts[0].Text, "\n")
		text = "re " + in
	}
	if b.gated && judge == b.gateJudge {
		select {
		case <-b.gate:
		case <-ctx.Done():
			return turn.Result{}, context.Cause(ctx)
		}
	}
	if fail != nil {
		return turn.Result{}, fail
	}
	return turn.Result{}, out.Item(eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}})
}

func goalActor(t *testing.T, log *memLog, b turn.Backend, create bool, live *atomic.Int32) *Actor {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	cfg := Config{ID: "s1", Log: log, Evaluator: "m/eval", Ownership: owned{}, Backend: b, Base: ctx, Go: func(f func()) { wg.Go(func() { live.Add(1); defer live.Add(-1); f() }) }, Done: func() {}, Prompt: func(string) string { return "" }}
	open := func() (*Actor, error) { return Open(ctx, cfg) }
	if create {
		open = func() (*Actor, error) { return Create(ctx, cfg, eventlog.SessionCreated{Model: "m/m"}, nil) }
	}
	a, err := open()
	if err != nil {
		t.Fatal(err)
	}
	a.Run()
	return a
}

// settled waits until a stops, or runs no turn and its goal is final.
func settled(a *Actor) {
	for v := a.View(); ; v = a.View() {
		g := v.Session.Goal
		if v.Stopped || g != nil && v.Session.TurnID == "" && g.State != "active" && g.State != "paused" {
			synctest.Wait()
			return
		}
		<-v.changed
	}
}

func submitHi(t *testing.T, a *Actor) {
	t.Helper()
	in := eventlog.InputAdmitted{InputID: "hi", Delivery: eventlog.DeliveryQueue, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "hi"}}}
	if _, _, err := a.Submit(context.Background(), in, ""); err != nil {
		t.Fatal(err)
	}
}

func clearGoal(t *testing.T, a *Actor) {
	t.Helper()
	if err := a.ClearGoal(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGoal(t *testing.T) {
	setGoal := func(max int) func(*testing.T, *Actor, *goalBackend) {
		return func(t *testing.T, a *Actor, _ *goalBackend) {
			if err := a.SetGoal(context.Background(), "say done", max); err != nil {
				t.Fatal(err)
			}
		}
	}
	ran := []string{"goal.set", "input.admitted", "turn.started", "item.completed assistant re say done", "turn.ended completed"}
	again := []string{"input.admitted", "turn.started", "item.completed assistant re The goal has not been met yet.", "turn.ended completed"}
	achieved := []string{"goal.evaluated met", "goal.changed achieved"}
	for _, tc := range []struct {
		name string
		b    *goalBackend
		act  func(*testing.T, *Actor, *goalBackend)
		want []string
	}{
		{"a goal on an idle session runs its condition until met", &goalBackend{verdicts: []string{"MET: said done"}}, setGoal(0),
			slices.Concat(ran, achieved)},
		{"a not_met verdict admits its guidance as the next input", &goalBackend{verdicts: []string{"NOT MET: say it", "met: ok"}}, setGoal(0),
			slices.Concat(ran, []string{"goal.evaluated not_met"}, again, achieved)},
		{"max_turns ends the goal exhausted", &goalBackend{verdicts: []string{"NOT MET: say it"}}, setGoal(2),
			slices.Concat(ran, []string{"goal.evaluated not_met"}, again, []string{"goal.evaluated not_met", "goal.changed exhausted"})},
		{"an impossible verdict fails the goal", &goalBackend{verdicts: []string{"IMPOSSIBLE: no way"}}, setGoal(0),
			slices.Concat(ran, []string{"goal.evaluated impossible", "goal.changed failed"})},
		{"a usage limit pauses the goal until its retry", &goalBackend{verdicts: []string{"MET: ok"}, errs: []error{turn.ErrExhausted, nil}}, setGoal(0),
			slices.Concat(ran[:3], []string{"turn.ended failed provider_exhausted", "goal.changed paused", "goal.changed active", "input.admitted", "turn.started",
				"item.completed assistant re Continue working toward the goal.", "turn.ended completed"}, achieved)},
		{"an error the user must fix fails the goal", &goalBackend{errs: []error{errors.New("bad request")}}, setGoal(0),
			slices.Concat(ran[:3], []string{"turn.ended failed bad request", "goal.changed failed"})},
		{"a goal set on a busy session judges the running turn", &goalBackend{verdicts: []string{"MET: ok"}, gated: true}, func(t *testing.T, a *Actor, b *goalBackend) {
			submitHi(t, a)
			setGoal(0)(t, a, b)
			close(b.gate)
		}, slices.Concat([]string{"input.admitted", "turn.started", "goal.set", "item.completed assistant re hi", "turn.ended completed"}, achieved)},
		{"clearing a goal stops its turn", &goalBackend{gated: true}, func(t *testing.T, a *Actor, b *goalBackend) {
			setGoal(0)(t, a, b)
			clearGoal(t, a)
		}, slices.Concat(ran[:3], []string{"goal.changed cleared", "turn.ended interrupted goal_cleared"})},
		{"an input during a goal turn leaves no goal input after the verdict", &goalBackend{verdicts: []string{"NOT MET: a", "NOT MET: b", "MET: ok"}, gated: true},
			func(t *testing.T, a *Actor, b *goalBackend) {
				setGoal(0)(t, a, b)
				submitHi(t, a)
				close(b.gate)
			}, slices.Concat(ran[:3], []string{"input.admitted", "item.completed assistant re say done", "turn.ended completed", "goal.evaluated not_met", "input.admitted",
				"turn.started", "item.completed assistant re hi", "turn.ended completed", "input.withdrawn", "goal.evaluated not_met"}, again, achieved)},
		{"clearing a paused goal starts the input it left queued", &goalBackend{errs: []error{turn.ErrExhausted, nil}, gated: true}, func(t *testing.T, a *Actor, b *goalBackend) {
			setGoal(0)(t, a, b)
			submitHi(t, a)
			close(b.gate)
			synctest.Wait()
			clearGoal(t, a)
		}, slices.Concat(ran[:3], []string{"input.admitted", "turn.ended failed provider_exhausted", "goal.changed paused", "goal.changed cleared",
			"turn.started", "item.completed assistant re hi", "turn.ended completed"})},
		{"an interrupt during the evaluation leaves the goal running", &goalBackend{verdicts: []string{"MET: ok"}, gated: true, gateJudge: true},
			func(t *testing.T, a *Actor, b *goalBackend) {
				setGoal(0)(t, a, b)
				synctest.Wait()
				if err := a.Interrupt(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
				close(b.gate)
			}, slices.Concat(ran, achieved)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start, log := time.Now(), &memLog{}
				tc.b.gate = make(chan struct{})
				a := goalActor(t, log, tc.b, true, new(atomic.Int32))
				tc.act(t, a, tc.b)
				settled(a)
				if got := lines(t, log, 2); !slices.Equal(got, tc.want) {
					t.Errorf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
				}
				if resumed := slices.Contains(tc.want, "goal.changed active"); resumed != (time.Since(start) >= goalRetry) {
					t.Errorf("goal ran again after %v, resumed %v", time.Since(start), resumed)
				}
			})
		})
	}
}

func TestResumedGoalLeavesNoRetryTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		live := new(atomic.Int32)
		b := &goalBackend{verdicts: []string{"MET: ok"}, errs: []error{turn.ErrExhausted, nil}, gate: make(chan struct{})}
		a := goalActor(t, &memLog{}, b, true, live)
		synctest.Wait()
		idle := live.Load()
		if err := a.SetGoal(context.Background(), "say done", 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		submitHi(t, a)
		settled(a)
		if n := live.Load(); n != idle {
			t.Errorf("%d goroutines run after the goal ended, %d before it", n, idle)
		}
	})
}

func TestOpenContinuesTheGoal(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    *goalBackend
		want []string
	}{
		{"an active goal judges the last turn", &goalBackend{verdicts: []string{"MET: ok"}, gated: true, gateJudge: true},
			[]string{"turn.ended completed", "owner.acquired", "goal.evaluated met", "goal.changed achieved"}},
		{"a paused goal runs again at its retry time", &goalBackend{verdicts: []string{"MET: ok"}, errs: []error{turn.ErrExhausted, nil}},
			[]string{"turn.ended failed provider_exhausted", "goal.changed paused", "owner.acquired", "goal.changed active", "input.admitted", "turn.started",
				"item.completed assistant re Continue working toward the goal.", "turn.ended completed", "goal.evaluated met", "goal.changed achieved"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				log := &memLog{}
				tc.b.gate = make(chan struct{})
				a := goalActor(t, log, tc.b, true, new(atomic.Int32))
				if err := a.SetGoal(context.Background(), "say done", 0); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if err := a.Release(context.Background()); err != nil {
					t.Fatal(err)
				}
				close(tc.b.gate)
				settled(goalActor(t, log, tc.b, false, new(atomic.Int32)))
				got := lines(t, log, 0)
				if got = got[max(0, len(got)-len(tc.want)):]; !slices.Equal(got, tc.want) {
					t.Errorf("log ends\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
				}
			})
		})
	}
}

func TestParseVerdict(t *testing.T) {
	for _, tc := range []struct {
		answer, why string
		want        eventlog.Verdict
	}{
		{"MET: done", "done", eventlog.VerdictMet},
		{"not met: more", "more", eventlog.VerdictNotMet},
		{"**MET**: done", "done", eventlog.VerdictMet},
		{"`IMPOSSIBLE: no way`", "no way", eventlog.VerdictImpossible},
		{"## NOT MET: more", "more", eventlog.VerdictNotMet},
		{"I think so", "I think so", eventlog.VerdictNotMet},
	} {
		if v, why := parseVerdict(tc.answer); v != tc.want || why != tc.why {
			t.Errorf("parseVerdict(%q) = %s, %q, want %s, %q", tc.answer, v, why, tc.want, tc.why)
		}
	}
}

func TestTranscriptKeepsTheNewestMessageOverBudget(t *testing.T) {
	big := eventlog.Message{Role: eventlog.RoleAssistant}
	for range transcriptBytes/partBytes + 1 {
		big.Parts = append(big.Parts, eventlog.Part{Type: eventlog.PartText, Text: strings.Repeat("x", partBytes)})
	}
	got := transcript([]eventlog.Message{{Role: eventlog.RoleUser, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "old"}}}, big})
	if !strings.HasPrefix(got, "[earlier conversation omitted]\n\nASSISTANT:\n") || strings.Contains(got, "old") {
		t.Errorf("transcript starts %q, want the omitted marker, then the newest message", got[:min(len(got), 80)])
	}
}
