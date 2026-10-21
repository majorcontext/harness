package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/plugin"
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
	// The evaluator is a classifier: it never reasons, and it answers in one line.
	evaluatorEffort    = "off"
	evaluatorMaxTokens = 256
	// goalRetry is the first wait of a paused goal. Each later pause before
	// a verdict doubles it, up to goalRetryMax.
	goalRetry    = 30 * time.Second
	goalRetryMax = 30 * time.Minute
	// goalMaxPauses bounds the pauses between verdicts.
	goalMaxPauses        = 6
	goalRetriesExhausted = "retries_exhausted"
	goalInterrupted      = "interrupted"
	goalEnded            = "ended"
	goalNoDriver         = "no_driver"
	// partBytes bounds one part of the transcript of the evaluator, which
	// keeps the newest messages within a share of the window of the evaluator
	// model: transcriptShare of the window less transcriptReserve tokens, at
	// bytesPerToken. A model with no known window reads transcriptFloorWindow.
	partBytes             = 4096
	transcriptShare       = 0.5
	transcriptReserve     = 2048
	transcriptFloorWindow = 16000
	partCut               = "…[truncated]"
	transcriptCut         = "[earlier conversation omitted to fit the evaluator's context budget]\n"
)

var (
	errGoalCleared = errors.New("harness: goal cleared")
	// ErrGoalActive reports a StartGoal while a goal is active, paused, or blocked.
	ErrGoalActive = errors.New("harness: a goal is already active")
	// ErrNoGoal reports an AdjustGoal with no active, paused, or blocked goal.
	ErrNoGoal = errors.New("harness: no active goal")
)

// SetGoal replaces the goal and resets its turn count. With no turn running
// and no input queued, it admits the condition as an input with source
// goal. Otherwise the next turn that ends is the first one judged.
func (a *Actor) SetGoal(ctx context.Context, condition string, maxTurns int) error {
	return a.setGoal(ctx, func(eventlog.Goal, bool) (*protocol.GoalSet, bool, error) {
		return &protocol.GoalSet{Condition: condition, MaxTurns: maxTurns}, false, nil
	})
}

// StartGoal is SetGoal with no turn limit that always admits the
// condition: while a turn runs, the condition waits, and the goal judges
// no turn before it; the condition posts after the turn of
// the goal tool. It fails with ErrGoalActive while a goal is active, paused,
// or blocked.
func (a *Actor) StartGoal(ctx context.Context, condition string) error {
	return a.setGoal(ctx, func(_ eventlog.Goal, live bool) (*protocol.GoalSet, bool, error) {
		if live {
			return nil, false, ErrGoalActive
		}
		return &protocol.GoalSet{Condition: condition}, true, nil
	})
}

// AdjustGoal replaces the condition of the active, paused, or blocked goal. It keeps
// max_turns and the turn count, so an adjust never extends the turn limit.
// The same condition changes nothing. A condition that still waits in the
// queue is replaced by the new one. It fails with ErrNoGoal when no goal is
// active, paused, or blocked.
func (a *Actor) AdjustGoal(ctx context.Context, condition string) error {
	return a.setGoal(ctx, func(g eventlog.Goal, live bool) (*protocol.GoalSet, bool, error) {
		switch {
		case !live:
			return nil, false, ErrNoGoal
		case g.Condition == condition:
			return nil, false, nil
		}
		waiting := slices.ContainsFunc(a.state.Queue(), func(in protocol.InputAdmitted) bool {
			return in.Source == sourceGoal && textOf(protocol.LogMessage{Parts: in.Parts}) == g.Condition
		})
		return &protocol.GoalSet{Condition: condition, MaxTurns: g.MaxTurns, Turns: g.Turns}, waiting, nil
	})
}

// setGoal appends the goal that set returns for the current goal, and
// whether that goal is active, paused, or blocked. A nil goal appends nothing. When
// set reports post, the condition is an input even while the session is
// busy.
func (a *Actor) setGoal(ctx context.Context, set func(g eventlog.Goal, live bool) (*protocol.GoalSet, bool, error)) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		g, _ := a.state.Goal()
		gs, post, err := set(g, liveGoal(g.State))
		if err != nil || gs == nil {
			reply(struct{}{}, err)
			return
		}
		events := append(a.withdrawGoal(), *gs)
		_, busy := a.state.Turn()
		var rest []protocol.InputAdmitted
		for _, in := range a.state.Queue() {
			if in.Source != sourceGoal {
				rest = append(rest, in)
			}
		}
		idle := !busy && (len(rest) == 0 || a.heldByRequest(rest))
		if idle || post {
			events = append(append(events, a.dismissRequests()...), goalInput(gs.Condition))
		}
		err = a.append(events...)
		if err == nil && idle && a.run == nil {
			err = a.next(true)
		}
		if err == nil {
			err = a.blockUndriven()
		}
		reply(struct{}{}, err)
	})
	return err
}

// liveGoal reports whether a goal in state s still pursues its condition,
// or waits for an input to pursue it.
func liveGoal(s protocol.GoalState) bool {
	return s == protocol.GoalActive || s == protocol.GoalPaused || s == protocol.GoalBlocked
}

// ClearGoal clears the goal and withdraws its queued inputs. A running goal
// turn or evaluation stops with cause goal_cleared, and ClearGoal returns
// after it ends.
func (a *Actor) ClearGoal(ctx context.Context) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		g, ok := a.state.Goal()
		if !ok || g.State == protocol.GoalCleared {
			reply(struct{}{}, nil)
			return
		}
		err := a.append(append(a.withdrawGoal(), protocol.GoalChanged{State: protocol.GoalCleared})...)
		if r := a.run; err == nil && g.State == protocol.GoalActive && r != nil && r.kind != kindCompaction {
			r.cancel(errGoalCleared)
			r.waiters = append(r.waiters, replyAppend(reply))
			return
		}
		if err == nil && a.run == nil {
			err = a.next(true)
		}
		reply(struct{}{}, err)
	})
	return err
}

func goalInput(text string) protocol.InputAdmitted {
	return protocol.InputAdmitted{InputID: newID("input"), Delivery: protocol.DeliveryQueue, Source: sourceGoal,
		Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: text}}}
}

func (a *Actor) withdrawGoal() []protocol.Payload {
	var events []protocol.Payload
	for _, in := range a.state.Queue() {
		if in.Source == sourceGoal {
			events = append(events, protocol.InputWithdrawn{InputID: in.InputID})
		}
	}
	return events
}

// resumed returns the goal change of an input: any input resumes a paused or
// blocked goal.
func (a *Actor) resumed() []protocol.Payload {
	if g, _ := a.state.Goal(); g.State == protocol.GoalPaused || g.State == protocol.GoalBlocked {
		return []protocol.Payload{protocol.GoalChanged{State: protocol.GoalActive}}
	}
	return nil
}

// judgeable reports whether the goal has not judged the last ended turn,
// and that turn completed or was interrupted.
func (a *Actor) judgeable() bool {
	g, _ := a.state.Goal()
	last := a.state.LastEnded()
	return last.TurnID != g.Evaluated && (last.StopReason == protocol.StopCompleted || last.StopReason == protocol.StopInterrupted)
}

// settle judges the last ended turn for an active goal, or starts the next
// queued input. A new goal judges no turn while its condition waits. The
// actor runs nothing.
func (a *Actor) settle(check bool) error {
	g, _ := a.state.Goal()
	posted := g.Turns == 0 && slices.ContainsFunc(a.state.Queue(), func(in protocol.InputAdmitted) bool { return in.Source == sourceGoal })
	if g.State == protocol.GoalActive && a.judgeable() && !posted {
		a.judge(g)
		return nil
	}
	if err := a.next(check); err != nil {
		return err
	}
	return a.blockUndriven()
}

// blockUndriven blocks an active goal that nothing drives: no run, no open
// turn or request, no queued input that starts, and no
// turn left to judge. It starts no turn and calls no model.
func (a *Actor) blockUndriven() error {
	g, _ := a.state.Goal()
	if g.State != protocol.GoalActive || a.run != nil || len(a.releasing) > 0 || len(a.state.Requests()) > 0 || a.judgeable() {
		return nil
	}
	if _, busy := a.state.Turn(); busy {
		return nil
	}
	q := a.state.Queue()
	if len(q) > 0 && !a.waitsForInput() {
		return nil
	}
	return a.append(protocol.GoalChanged{State: protocol.GoalBlocked, Reason: goalNoDriver})
}

// judge runs the evaluator on the session history as the run of the actor.
func (a *Actor) judge(g eventlog.Goal) {
	r := a.newRun(kindJudge, newID("goal"))
	a.run = r
	turnID := a.state.LastEnded().TurnID
	text := "GOAL CONDITION:\n" + g.Condition + "\n\nCONVERSATION TRANSCRIPT:\n" + transcript(a.state.History(), a.transcriptBudget())
	req := turn.Request{SessionID: a.cfg.ID, TurnID: r.id, Model: a.cfg.Evaluator, Instructions: evaluatorPrompt,
		Settings: protocol.Settings{Effort: evaluatorEffort, Accounts: a.state.Settings().Accounts}, MaxTokens: evaluatorMaxTokens,
		History: []protocol.LogMessage{{Role: protocol.RoleUser, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: text}}}}}
	a.spawn(func() {
		answer, _, err := turn.Ask(r.ctx, a.cfg.Backend, req, a.cfg.Limits)
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
	var appendErr error
	if stopped == nil && len(a.releasing) == 0 && g.State == protocol.GoalActive && g.Evaluated != turnID {
		events := append(a.withdrawGoal(), verdict(turnID, g, answer)...)
		if err != nil {
			events = a.goalStop(err)
		}
		appendErr = a.append(events...)
	}
	var next func() error
	switch {
	case stopped != nil:
		next = func() error { return a.next(true) }
	case appendErr == nil:
		next = func() error { return a.settle(true) }
	}
	a.finishRun(r, err, appendErr, next)
}

func verdict(turnID string, g eventlog.Goal, answer string) []protocol.Payload {
	v, why := parseVerdict(answer)
	ev := protocol.GoalEvaluated{TurnID: turnID, Verdict: v}
	switch v {
	case protocol.VerdictMet:
		return []protocol.Payload{ev, protocol.GoalChanged{State: protocol.GoalAchieved, Reason: why}}
	case protocol.VerdictImpossible:
		return []protocol.Payload{ev, protocol.GoalChanged{State: protocol.GoalFailed, Reason: why}}
	}
	ev.Guidance = why
	if g.MaxTurns > 0 && g.Turns+1 >= g.MaxTurns {
		return []protocol.Payload{ev, protocol.GoalChanged{State: protocol.GoalExhausted, Reason: fmt.Sprintf("max_turns %d reached", g.MaxTurns)}}
	}
	return []protocol.Payload{ev, goalInput("The goal has not been met yet.\n\nGOAL: " + g.Condition +
		"\n\nEVALUATOR FEEDBACK: " + why + "\n\nKeep working until the goal is fully satisfied, then stop.")}
}

// parseVerdict reads a verdict prefix in any case, after markdown marks. A
// reply with no verdict is not_met, and the reply is the guidance.
func parseVerdict(answer string) (protocol.Verdict, string) {
	const marks = "*#` \t\n"
	t := strings.Trim(answer, marks)
	for _, f := range []struct {
		prefix  string
		verdict protocol.Verdict
	}{{"NOT MET", protocol.VerdictNotMet}, {"IMPOSSIBLE", protocol.VerdictImpossible}, {"MET", protocol.VerdictMet}} {
		if len(t) >= len(f.prefix) && strings.EqualFold(t[:len(f.prefix)], f.prefix) {
			return f.verdict, strings.Trim(strings.TrimPrefix(strings.TrimLeft(t[len(f.prefix):], marks), ":"), marks)
		}
	}
	return protocol.VerdictNotMet, t
}

// transcriptBudget returns the bytes of transcript that the evaluator model
// reads: half its window less a reserve for the rest of the request, never
// less than the reserve. A window that the backend only estimates counts as
// transcriptFloorWindow.
func (a *Actor) transcriptBudget() int {
	caps := a.cfg.Backend.Capabilities(a.cfg.Evaluator)
	window := caps.ContextWindow
	if caps.WindowEstimated || window <= 0 {
		window = transcriptFloorWindow
	}
	return max(int(float64(window)*transcriptShare)-transcriptReserve, transcriptReserve) * bytesPerToken
}

// transcript renders h for the evaluator: each message under its role, each
// part cut at partBytes, and the newest messages within budget bytes, each
// message counted with the blank line that follows it. It keeps the newest
// message even over the budget.
func transcript(h []protocol.LogMessage, budget int) string {
	var blocks []string
	size := 0
	for _, m := range slices.Backward(h) {
		var b strings.Builder
		b.WriteString(strings.ToUpper(m.Role) + ":\n")
		for _, p := range m.Parts {
			s := p.Text
			switch {
			case p.Type == protocol.LogPartWebSearch:
				s = "[web search] " + p.Query
			case p.Type == protocol.LogPartToolCall:
				s = "[tool call " + p.Name + "] " + string(p.Arguments)
			case p.Type == protocol.LogPartToolResult && p.IsError:
				s = "[tool result (error)] " + p.Text
			case p.Type == protocol.LogPartToolResult:
				s = "[tool result] " + p.Text
			case p.Type == protocol.LogPartReasoning:
				s = "[reasoning] " + p.Text
			case p.Type == protocol.LogPartBlob:
				s = "[blob " + p.MediaType + "]"
			}
			if len(s) > partBytes {
				s = strings.ToValidUTF8(s[:partBytes], "") + partCut
			}
			b.WriteString(s + "\n")
		}
		if size += b.Len() + 1; size > budget && len(blocks) > 0 {
			blocks = append(blocks, transcriptCut)
			break
		}
		blocks = append(blocks, b.String())
	}
	slices.Reverse(blocks)
	return strings.TrimSuffix(strings.Join(blocks, "\n"), "\n")
}

// goalStop returns the goal change after err ended a goal turn or its
// evaluation: paused for a retryable error or a usage limit, else failed. The
// goalMaxPauses+1th pause fails with retries_exhausted.
func (a *Actor) goalStop(err error) []protocol.Payload {
	g, _ := a.state.Goal()
	if g.State != protocol.GoalActive {
		return nil
	}
	change := protocol.GoalChanged{State: protocol.GoalFailed, Reason: plugin.SanitizeSessionError(err.Error())}
	if errors.Is(err, turn.ErrRetryable) || errors.Is(err, turn.ErrExhausted) {
		if g.Pauses >= goalMaxPauses {
			change.Reason = goalRetriesExhausted
			return append(a.withdrawGoal(), change)
		}
		change.State, change.RetryAt = protocol.GoalPaused, time.Now().Add(min(goalRetry<<min(g.Pauses, 10), goalRetryMax))
	}
	return append(a.withdrawGoal(), change)
}

// retryLater resumes a paused goal at its retry time. It replaces the
// timer of an earlier pause, and appending an event that ends the pause
// stops it.
func (a *Actor) retryLater() {
	a.stopRetry()
	g, _ := a.state.Goal()
	if g.State != protocol.GoalPaused {
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
	if g.State != protocol.GoalPaused || !g.RetryAt.Equal(at) {
		return nil
	}
	events := a.resumed()
	if !a.judgeable() {
		events = append(events, goalInput("Continue working toward the goal.\n\nGOAL: "+g.Condition))
	}
	if err := a.append(events...); err != nil || a.run != nil {
		return err
	}
	return a.settle(true)
}
