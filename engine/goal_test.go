package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// goalProvider serves both the worker model and the evaluator model from one
// registry entry. It keys the two apart by the presence of tools: the worker
// loop always offers built-in tools, while the goal evaluator's one-shot
// request is deliberately tool-less. Each side is scripted independently.
type goalProvider struct {
	worker   [][]provider.Event
	eval     [][]provider.Event
	wi, ei   int
	requests []*provider.Request
	failCtx  bool // when true, honor ctx cancellation in Stream
}

func (p *goalProvider) Name() string { return "test" }

func (p *goalProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if p.failCtx {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	p.requests = append(p.requests, req)
	if len(req.Tools) == 0 {
		// Evaluator request (tool-less).
		if p.ei >= len(p.eval) {
			return &scriptedStream{}, nil
		}
		ev := p.eval[p.ei]
		p.ei++
		return &scriptedStream{events: ev}, nil
	}
	if p.wi >= len(p.worker) {
		return &scriptedStream{}, nil
	}
	ev := p.worker[p.wi]
	p.wi++
	return &scriptedStream{events: ev}, nil
}

// evalTurn is a complete evaluator reply carrying a single text block.
func evalTurn(text string) []provider.Event {
	msg := &message.Message{ID: "msg_eval", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}}
	return []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}}
}

func goalSession(t *testing.T, prov provider.Provider, dir string) *Session {
	t.Helper()
	return NewSession(Config{
		Providers:    provider.Registry{prov.Name(): prov},
		Model:        message.ModelRef{Provider: prov.Name(), Model: "m1"},
		System:       []string{"base"},
		SessionDir:   dir,
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
}

var evalModel = message.ModelRef{Provider: "test", Model: "eval"}

func TestPursueGoalAchievedSecondTurn(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "working on it"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "all done"}),
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: the summary is missing"),
			evalTurn("MET: the summary is present"),
		},
	}
	var evs []Event
	s := goalSession(t, prov, t.TempDir())
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	res, err := s.PursueGoal(context.Background(), "write a summary", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Achieved || res.Turns != 2 {
		t.Fatalf("result = %+v, want achieved in 2 turns", res)
	}
	if !strings.Contains(res.Reason, "present") {
		t.Errorf("reason = %q", res.Reason)
	}

	// History: user(condition), asst, user(guidance), asst.
	h := s.History()
	if len(h) != 4 {
		t.Fatalf("history len = %d: %+v", len(h), h)
	}
	// The second directive (the guidance message) must carry the evaluator reason.
	guidance := h[2].Parts.Text()
	if h[2].Role != message.RoleUser || !strings.Contains(guidance, "the summary is missing") {
		t.Errorf("guidance message = %q, want it to include the NOT MET reason", guidance)
	}

	// Goal events, in order.
	var got []string
	for _, ev := range evs {
		if strings.HasPrefix(ev.Type, "goal.") {
			got = append(got, ev.Type)
		}
	}
	want := []string{EventGoalSet, EventGoalEval, EventGoalEval, EventGoalAchieved}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("goal events = %v, want %v", got, want)
	}

	// Evaluator requests are tool-less and use the evaluator model.
	var evalReqs int
	for _, rq := range prov.requests {
		if len(rq.Tools) == 0 {
			evalReqs++
			if rq.Model != evalModel {
				t.Errorf("evaluator request model = %v, want %v", rq.Model, evalModel)
			}
			if rq.MaxTokens != 256 {
				t.Errorf("evaluator MaxTokens = %d, want 256", rq.MaxTokens)
			}
		}
	}
	if evalReqs != 2 {
		t.Errorf("evaluator requests = %d, want 2", evalReqs)
	}
}

func TestPursueGoalRequiresEvaluator(t *testing.T) {
	prov := &goalProvider{}
	s := goalSession(t, prov, t.TempDir())
	if _, err := s.PursueGoal(context.Background(), "do it", GoalOptions{}); err == nil {
		t.Fatal("PursueGoal with zero evaluator succeeded, want error")
	}
}

func TestPursueGoalMaxTurns(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "try 1"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "try 2"}),
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: nope"),
			evalTurn("NOT MET: still nope"),
		},
	}
	s := goalSession(t, prov, t.TempDir())
	res, err := s.PursueGoal(context.Background(), "impossible", GoalOptions{MaxTurns: 2, Evaluator: evalModel})
	if err != nil {
		t.Fatal(err)
	}
	if res.Achieved {
		t.Fatalf("result = %+v, want not achieved", res)
	}
	if res.Turns != 2 || res.Reason != "max turns" {
		t.Errorf("result = %+v, want turns=2 reason=%q", res, "max turns")
	}
	// A goal that exhausted its turns without achieving stays active for resume.
	if cond, ok := s.ActiveGoal(); !ok || cond != "impossible" {
		t.Errorf("ActiveGoal = %q, %v; want the condition, active", cond, ok)
	}
}

func TestPursueGoalUnparseableTwice(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
		},
		eval: [][]provider.Event{
			evalTurn("I am not sure about this"),
			evalTurn("still rambling with no verdict"),
		},
	}
	s := goalSession(t, prov, t.TempDir())
	if _, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel}); err == nil {
		t.Fatal("PursueGoal with two unparseable evaluations succeeded, want error")
	}
}

func TestPursueGoalUnparseableThenRecovers(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
		},
		eval: [][]provider.Event{
			evalTurn("hmm, unclear"),        // unparseable
			evalTurn("MET: looks good now"), // retry parses
		},
	}
	s := goalSession(t, prov, t.TempDir())
	res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Achieved || res.Turns != 1 {
		t.Fatalf("result = %+v, want achieved in 1 turn after a retry", res)
	}
}

func TestPursueGoalContextCancel(t *testing.T) {
	prov := &goalProvider{
		failCtx: true,
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
		},
		eval: [][]provider.Event{evalTurn("MET: fine")},
	}
	s := goalSession(t, prov, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.PursueGoal(ctx, "cond", GoalOptions{Evaluator: evalModel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestGoalRecordsResumeActive(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "try 1"}),
		},
		eval: [][]provider.Event{evalTurn("NOT MET: keep going")},
	}
	s := goalSession(t, prov, dir)
	cfg := s.cfg
	// One turn, max 1 → not achieved, goal remains active in the log.
	if _, err := s.PursueGoal(context.Background(), "ongoing goal", GoalOptions{MaxTurns: 1, Evaluator: evalModel}); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	cond, ok := loaded.ActiveGoal()
	if !ok || cond != "ongoing goal" {
		t.Errorf("resumed ActiveGoal = %q, %v; want active with the condition", cond, ok)
	}
}

func TestGoalAchievedNotActiveOnResume(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProvider{
		worker: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "done"})},
		eval:   [][]provider.Event{evalTurn("MET: complete")},
	}
	s := goalSession(t, prov, dir)
	cfg := s.cfg
	if _, err := s.PursueGoal(context.Background(), "finished goal", GoalOptions{Evaluator: evalModel}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ActiveGoal(); ok {
		t.Error("ActiveGoal still active after achievement")
	}
	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.ActiveGoal(); ok {
		t.Error("resumed ActiveGoal active after achievement, want inactive")
	}
}

func TestClearGoalRecordsAndResets(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProvider{
		worker: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "try"})},
		eval:   [][]provider.Event{evalTurn("NOT MET: nope")},
	}
	s := goalSession(t, prov, dir)
	cfg := s.cfg
	var cleared int
	s.cfg.OnEvent = func(ev Event) {
		if ev.Type == EventGoalCleared {
			cleared++
		}
	}
	if _, err := s.PursueGoal(context.Background(), "goalx", GoalOptions{MaxTurns: 1, Evaluator: evalModel}); err != nil {
		t.Fatal(err)
	}
	if !s.ClearGoal() {
		t.Fatal("ClearGoal returned false for an active goal")
	}
	if cleared != 1 {
		t.Errorf("goal.cleared events = %d, want 1", cleared)
	}
	if _, ok := s.ActiveGoal(); ok {
		t.Error("ActiveGoal still active after ClearGoal")
	}
	// Idempotent: a second clear does nothing.
	if s.ClearGoal() {
		t.Error("second ClearGoal returned true, want false (no active goal)")
	}
	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.ActiveGoal(); ok {
		t.Error("resumed ActiveGoal active after clear, want inactive")
	}
}

// blockingEvalProvider serves the worker turn immediately but parks the
// evaluator's Stream call on a channel released by the test, so a test can
// race ClearGoal against an in-flight evaluation. entered is closed the
// moment the evaluator request arrives, letting the test know it is safe to
// call ClearGoal.
type blockingEvalProvider struct {
	worker  [][]provider.Event
	wi      int
	entered chan struct{}
	release chan struct{}
	evalOut string

	once sync.Once
}

func (p *blockingEvalProvider) Name() string { return "test" }

func (p *blockingEvalProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) == 0 {
		p.once.Do(func() { close(p.entered) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &scriptedStream{events: evalTurn(p.evalOut)}, nil
	}
	ev := p.worker[p.wi]
	p.wi++
	return &scriptedStream{events: ev}, nil
}

// TestClearGoalDuringPendingEvaluationIsCleanStop reproduces the concurrency
// finding: a ClearGoal (DELETE /goal) racing an in-flight evaluation must not
// let that evaluation's verdict land in the journal after goal.cleared, and
// PursueGoal must treat the race as a clean stop rather than an achievement.
func TestClearGoalDuringPendingEvaluationIsCleanStop(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	prov := &blockingEvalProvider{
		worker:  [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "working"})},
		entered: entered,
		release: release,
		evalOut: "MET: looks done",
	}
	s := goalSession(t, prov, dir)

	type outcome struct {
		res *GoalResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.PursueGoal(context.Background(), "goalx", GoalOptions{Evaluator: evalModel})
		done <- outcome{res, err}
	}()

	<-entered // the evaluation is in flight, blocked on release

	if !s.ClearGoal() {
		t.Fatal("ClearGoal returned false for an active goal")
	}

	releaseOnce.Do(func() { close(release) }) // let the pending MET verdict land

	out := <-done
	if out.err != nil {
		t.Fatalf("PursueGoal error = %v", out.err)
	}
	if out.res.Achieved {
		t.Fatalf("result = %+v, want a clean stop (Achieved=false) since the goal was cleared mid-evaluation", out.res)
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"goal.cleared"`) {
		t.Fatalf("session log missing goal.cleared record:\n%s", log)
	}
	if strings.Contains(log, `"type":"goal.achieved"`) {
		t.Errorf("session log contains goal.achieved after a ClearGoal raced an in-flight evaluation:\n%s", log)
	}
	if strings.Contains(log, `"type":"goal.eval"`) {
		t.Errorf("session log contains a goal.eval record for an evaluation that resolved after ClearGoal:\n%s", log)
	}
}

// TestGoalEventsEmitWhileLockHeld pins the follow-up ordering fix:
// recordGoalEval, achieveGoal, and ClearGoal must emit their goal.* event
// while still holding s.mu, not after releasing it — otherwise the emitted
// event order (-> server journal/SSE seqs) can invert relative to the
// journaled log order under the same ClearGoal-vs-evaluation race exercised
// by TestClearGoalDuringPendingEvaluationIsCleanStop.
//
// A deterministic "red" reproduction of the actual inversion (two goroutines
// racing so that goroutine B's write+emit both complete while goroutine A is
// paused between its own write and its own emit) is not constructible from
// outside the package without adding a production-only test seam: which
// goroutine's emit callback runs first, once both are runnable, is a plain
// scheduler race with no available happens-before edge to pin from a test.
// Forcing it via a real second sync.Mutex.Lock from inside the OnEvent
// callback would risk a genuine runtime self-deadlock (fatal, not a clean
// test failure) if the fix were absent. So instead this pins the invariant
// that makes the inversion impossible in the first place — the event fires
// synchronously inside the same critical section as the log write — using a
// non-blocking sync.Mutex.TryLock from the test goroutine, which can never
// block or deadlock either way.
func TestGoalEventsEmitWhileLockHeld(t *testing.T) {
	tests := []struct {
		name string
		want string
		run  func(s *Session)
	}{
		{
			name: "recordGoalEval",
			want: EventGoalEval,
			run: func(s *Session) {
				s.setGoal("cond")
				s.recordGoalEval(true, "reason", 1)
			},
		},
		{
			name: "achieveGoal",
			want: EventGoalAchieved,
			run: func(s *Session) {
				s.setGoal("cond")
				s.achieveGoal("reason", 1)
			},
		},
		{
			name: "ClearGoal",
			want: EventGoalCleared,
			run: func(s *Session) {
				s.setGoal("cond")
				s.ClearGoal()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			s := goalSession(t, &goalProvider{}, dir)

			entered := make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			s.cfg.OnEvent = func(ev Event) {
				if ev.Type != tt.want {
					return
				}
				close(entered)
				<-release
			}

			done := make(chan struct{})
			go func() {
				tt.run(s)
				close(done)
			}()

			<-entered // the target event's handler is now parked inside emit

			// Non-blocking: if the write's lock had already been released
			// before emit (the pre-fix bug), TryLock succeeds here and must
			// be undone immediately so the emitter's later Unlock doesn't
			// panic on an unlocked mutex. If the fix holds, s.mu is still
			// held by the emitting goroutine and TryLock reports that
			// honestly — no blocking, no risk of deadlock, either way.
			if s.mu.TryLock() {
				s.mu.Unlock()
				t.Fatalf("s.mu was free while the %s event handler was still running: emit must happen inside the same critical section as the log write, not after its Unlock", tt.want)
			}

			close(release) // let the emitter finish
			<-done
		})
	}
}
