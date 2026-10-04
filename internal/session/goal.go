package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

const evaluatorPrompt = `You are a strict goal-completion evaluator for an autonomous agent.
You are given a GOAL CONDITION and a transcript of the agent's work so far.
Decide whether the condition has been FULLY satisfied by the work shown.

Reply with EXACTLY ONE line, in one of these three forms and nothing else:
MET: <one short sentence saying why>
NOT MET: <one short sentence saying what is still missing>
IMPOSSIBLE: <one short sentence saying why no further work can satisfy it>

Do not add any other text, headings, markdown, or code fences.`

const (
	sourceGoal = "goal"
	// goalRetry is the first wait of a paused goal. Each later pause before
	// a verdict doubles it, up to goalRetryMax.
	goalRetry    = 30 * time.Second
	goalRetryMax = 30 * time.Minute
	// partBytes and transcriptBytes bound the transcript of the evaluator,
	// which keeps the newest messages.
	partBytes       = 4096
	transcriptBytes = 128 << 10
)

var errGoalCleared = errors.New("harness: goal cleared")

// SetGoal replaces the goal and resets its turn count. With no turn running
// and no input queued, it admits the condition as an input with source
// goal. Otherwise the next turn that ends is the first one judged.
func (a *Actor) SetGoal(ctx context.Context, condition string, maxTurns int) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		events := append(a.withdrawGoal(), eventlog.GoalSet{Condition: condition, MaxTurns: maxTurns})
		_, busy := a.state.Turn()
		idle := !busy && len(a.state.Queue()) == len(events)-1
		if idle {
			events = append(append(events, a.dismissRequests()...), goalInput(condition))
		}
		err := a.append(events...)
		if err == nil && idle && a.run == nil {
			err = a.next(true)
		}
		reply(struct{}{}, err)
	})
	return err
}

// ClearGoal clears the goal and withdraws its queued inputs. A running goal
// turn or evaluation stops with cause goal_cleared, and ClearGoal returns
// after it ends.
func (a *Actor) ClearGoal(ctx context.Context) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		g, ok := a.state.Goal()
		if !ok || g.State == eventlog.GoalCleared {
			reply(struct{}{}, nil)
			return
		}
		err := a.append(append(a.withdrawGoal(), eventlog.GoalChanged{State: eventlog.GoalCleared})...)
		_, turning := a.state.Turn()
		if r := a.run; err == nil && g.State == eventlog.GoalActive && r != nil && (r.judge || turning) {
			r.cancel(errGoalCleared)
			r.waiters = append(r.waiters, reply)
			return
		}
		if err == nil && a.run == nil {
			err = a.next(true)
		}
		reply(struct{}{}, err)
	})
	return err
}

func goalInput(text string) eventlog.InputAdmitted {
	return eventlog.InputAdmitted{InputID: newID("input"), Delivery: eventlog.DeliveryQueue, Source: sourceGoal,
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
}

func (a *Actor) withdrawGoal() []eventlog.Event {
	var events []eventlog.Event
	for _, in := range a.state.Queue() {
		if in.Source == sourceGoal {
			events = append(events, eventlog.InputWithdrawn{InputID: in.InputID})
		}
	}
	return events
}

// resumed returns the goal change of an input: any input resumes a paused goal.
func (a *Actor) resumed() []eventlog.Event {
	if g, _ := a.state.Goal(); g.State == eventlog.GoalPaused {
		return []eventlog.Event{eventlog.GoalChanged{State: eventlog.GoalActive}}
	}
	return nil
}

// judgeable reports whether the goal has not judged the last ended turn,
// and that turn completed or was interrupted.
func (a *Actor) judgeable() bool {
	g, _ := a.state.Goal()
	last := a.state.LastEnded()
	return last.TurnID != g.Evaluated && (last.StopReason == eventlog.StopCompleted || last.StopReason == eventlog.StopInterrupted)
}

// settle judges the last ended turn for an active goal, or starts the next
// queued input. The actor runs nothing.
func (a *Actor) settle(check bool) error {
	if g, _ := a.state.Goal(); g.State == eventlog.GoalActive && a.judgeable() {
		a.judge(g)
		return nil
	}
	return a.next(check)
}

// judge runs the evaluator on the session history as the run of the actor.
func (a *Actor) judge(g eventlog.Goal) {
	ctx, cancel := context.WithCancelCause(a.cfg.Base)
	r := &running{id: newID("goal"), ctx: ctx, cancel: cancel, step: ctx, handoff: cancel, judge: true}
	a.run = r
	turnID := a.state.LastEnded().TurnID
	text := "GOAL CONDITION:\n" + g.Condition + "\n\nCONVERSATION TRANSCRIPT:\n" + transcript(a.state.History())
	req := turn.Request{SessionID: a.cfg.ID, TurnID: r.id, Model: a.cfg.Evaluator, Instructions: evaluatorPrompt,
		History: []eventlog.Message{{Role: eventlog.RoleUser, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}}}
	a.cfg.Go(func() {
		answer, err := turn.Ask(ctx, a.cfg.Backend, req, a.cfg.Limits.Idle)
		_, _ = call(context.Background(), a, func(reply func(struct{}, error)) {
			a.judged(r, turnID, answer, err)
			reply(struct{}{}, nil)
		})
	})
}

// judged records the verdict of run r on turnID, unless r was stopped or
// the goal changed. An evaluator error pauses or fails the goal as a turn
// error does.
func (a *Actor) judged(r *running, turnID, answer string, err error) {
	if a.run != r {
		return
	}
	a.run = nil
	stopped := context.Cause(r.ctx)
	r.cancel(nil)
	g, _ := a.state.Goal()
	var aerr error
	if stopped == nil && len(a.releasing) == 0 && g.State == eventlog.GoalActive && g.Evaluated != turnID {
		events := append(a.withdrawGoal(), verdict(turnID, g, answer)...)
		if err != nil {
			events = a.goalStop(err)
		}
		aerr = a.append(events...)
		a.retryLater()
	}
	for _, w := range r.waiters {
		w(struct{}{}, aerr)
	}
	switch {
	case len(a.releasing) > 0:
		a.stop(nil)
	case stopped != nil:
		_ = a.next(true)
	case aerr == nil:
		_ = a.settle(true)
	}
}

func verdict(turnID string, g eventlog.Goal, answer string) []eventlog.Event {
	v, why := parseVerdict(answer)
	ev := eventlog.GoalEvaluated{TurnID: turnID, Verdict: v}
	switch v {
	case eventlog.VerdictMet:
		return []eventlog.Event{ev, eventlog.GoalChanged{State: eventlog.GoalAchieved, Reason: why}}
	case eventlog.VerdictImpossible:
		return []eventlog.Event{ev, eventlog.GoalChanged{State: eventlog.GoalFailed, Reason: why}}
	}
	ev.Guidance = why
	if g.MaxTurns > 0 && g.Turns+1 >= g.MaxTurns {
		return []eventlog.Event{ev, eventlog.GoalChanged{State: eventlog.GoalExhausted, Reason: fmt.Sprintf("max_turns %d reached", g.MaxTurns)}}
	}
	return []eventlog.Event{ev, goalInput("The goal has not been met yet.\n\nGOAL: " + g.Condition +
		"\n\nEVALUATOR FEEDBACK: " + why + "\n\nKeep working until the goal is fully satisfied, then stop.")}
}

// parseVerdict reads a verdict prefix in any case, after markdown marks. A
// reply with no verdict is not_met, and the reply is the guidance.
func parseVerdict(answer string) (eventlog.Verdict, string) {
	const marks = "*#` \t\n"
	t := strings.Trim(answer, marks)
	for _, f := range []struct {
		prefix  string
		verdict eventlog.Verdict
	}{{"NOT MET", eventlog.VerdictNotMet}, {"IMPOSSIBLE", eventlog.VerdictImpossible}, {"MET", eventlog.VerdictMet}} {
		if len(t) >= len(f.prefix) && strings.EqualFold(t[:len(f.prefix)], f.prefix) {
			return f.verdict, strings.Trim(strings.TrimPrefix(strings.TrimLeft(t[len(f.prefix):], marks), ":"), marks)
		}
	}
	return eventlog.VerdictNotMet, t
}

// transcript renders h for the evaluator: each message under its role, each
// part cut at partBytes, and the newest messages within transcriptBytes. It
// keeps the newest message even over the budget.
func transcript(h []eventlog.Message) string {
	var blocks []string
	size := 0
	for _, m := range slices.Backward(h) {
		var b strings.Builder
		b.WriteString(strings.ToUpper(m.Role) + ":\n")
		for _, p := range m.Parts {
			s := p.Text
			if p.Type == eventlog.PartToolCall {
				s = p.Name + " " + string(p.Arguments)
			}
			if len(s) > partBytes {
				s = strings.ToValidUTF8(s[:partBytes], "") + " [cut]"
			}
			if p.Type != eventlog.PartReasoning {
				b.WriteString(s + "\n")
			}
		}
		if size += b.Len(); size > transcriptBytes && len(blocks) > 0 {
			blocks = append(blocks, "[earlier conversation omitted]\n")
			break
		}
		blocks = append(blocks, b.String())
	}
	slices.Reverse(blocks)
	return strings.Join(blocks, "\n")
}

// goalStop returns the goal change after err ended a goal turn or its
// evaluation: paused for a retryable error or a usage limit, else failed.
func (a *Actor) goalStop(err error) []eventlog.Event {
	g, _ := a.state.Goal()
	if g.State != eventlog.GoalActive {
		return nil
	}
	change := eventlog.GoalChanged{State: eventlog.GoalFailed, Reason: err.Error()}
	if errors.Is(err, turn.ErrRetryable) || errors.Is(err, turn.ErrExhausted) {
		change.State, change.RetryAt = eventlog.GoalPaused, time.Now().Add(min(goalRetry<<min(g.Pauses, 10), goalRetryMax))
	}
	return append(a.withdrawGoal(), change)
}

// retryLater resumes a paused goal at its retry time. It replaces the
// timer of an earlier pause, and appending an event that ends the pause
// stops it.
func (a *Actor) retryLater() {
	a.stopRetry()
	g, _ := a.state.Goal()
	if g.State != eventlog.GoalPaused {
		return
	}
	ctx, cancel := context.WithCancel(a.cfg.Base)
	a.retryStop, a.retryAt = cancel, g.RetryAt
	a.cfg.Go(func() {
		t := time.NewTimer(time.Until(g.RetryAt))
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		case <-a.quit:
			return
		}
		_, _ = call(context.Background(), a, func(reply func(struct{}, error)) { reply(struct{}{}, a.retry(g.RetryAt)) })
	})
}

func (a *Actor) stopRetry() {
	if a.retryStop != nil {
		a.retryStop()
		a.retryStop = nil
	}
}

// retry resumes the goal paused until at. It judges the last turn again
// after an evaluator error, and otherwise admits an input that continues
// the goal.
func (a *Actor) retry(at time.Time) error {
	g, _ := a.state.Goal()
	if g.State != eventlog.GoalPaused || !g.RetryAt.Equal(at) {
		return nil
	}
	events := a.resumed()
	if !a.judgeable() {
		events = append(append(events, a.dismissRequests()...), goalInput("Continue working toward the goal.\n\nGOAL: "+g.Condition))
	}
	if err := a.append(events...); err != nil || a.run != nil {
		return err
	}
	return a.settle(true)
}
