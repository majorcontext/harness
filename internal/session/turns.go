package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/plugin"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/turn"
)

const (
	cutOff      = "cut off before a result was recorded; check whether it took effect before running it again"
	interrupted = "interrupted before a result was recorded; check whether it took effect before running it again"
	// lostToRestart closes the history of a turn that a crash ended, so the
	// next user message does not join the crashed one on the wire.
	lostToRestart = "[harness: this turn was interrupted by a process restart and could not complete]"
)

var (
	errStopTurn = errors.New("harness: turn stopped")
	errEndTurn  = errors.New("harness: turn stopped by the end of a session")
	errTaskTurn = fmt.Errorf("harness: turn stopped by a task cancel: %w", errStopTurn)
)

// Submit admits in and returns the seq of its input.admitted record. A
// repeated input ID returns the original seq and repeat set.
func (a *Actor) Submit(ctx context.Context, in protocol.InputAdmitted, expectedTurn string) (seq uint64, repeat bool, err error) {
	type receipt struct {
		seq    uint64
		repeat bool
	}
	r, err := call(ctx, a, func(reply func(receipt, error)) {
		if _, _, ok := a.state.Command(in.InputID); ok {
			reply(receipt{}, ErrInputConflict)
			return
		}
		if old, seq, ok := a.state.Input(in.InputID); ok {
			if !sameJSON(old, in) {
				reply(receipt{}, ErrInputConflict)
				return
			}
			reply(receipt{seq, true}, nil)
			return
		}
		seq, err := a.admit(in, expectedTurn, a.resumed()...)
		reply(receipt{seq, false}, err)
	})
	return r.seq, r.repeat, err
}

// admit appends before, then admits in.
func (a *Actor) admit(in protocol.InputAdmitted, expectedTurn string, before ...protocol.Payload) (uint64, error) {
	if in.Delivery == protocol.DeliverySteer && expectedTurn != "" && (a.run == nil || a.run.id != expectedTurn) {
		return 0, ErrTurnMismatch
	}
	events := append(append(before, a.dismissRequests()...), in)
	seq := a.state.Head() + uint64(len(events))
	r := a.run
	if r == nil && !a.compactDue() {
		ids := a.startInputs(append(a.state.Queue(), in))
		id := newID("turn")
		if err := a.append(append(events, protocol.TurnStarted{TurnID: id, InputIDs: ids})...); err != nil {
			return 0, err
		}
		a.start(id, ids)
		return seq, nil
	}
	if err := a.append(events...); err != nil {
		return 0, err
	}
	if r == nil {
		return seq, a.next(true)
	}
	if in.Delivery == protocol.DeliverySteer && r.steering {
		select {
		case r.steered <- struct{}{}:
		default:
		}
	}
	return seq, nil
}

func sameJSON(x, y any) bool {
	bx, errx := json.Marshal(x)
	by, erry := json.Marshal(y)
	return errx == nil && erry == nil && string(bx) == string(by)
}

// modelRequest returns the request of the next model call before its inputs:
// the conversation as the model reads it, with each pinned segment.
func (a *Actor) modelRequest() turn.Request {
	return turn.Request{SessionID: a.cfg.ID, Model: a.state.Model(), Settings: a.state.Settings(), Instructions: a.cfg.Prompt(), Skills: a.cfg.Skills, Sent: a.cfg.Sent,
		History: a.state.ModelHistory(), AllowedTools: a.state.AllowedTools(), MaxTokens: a.cfg.MaxTokens, Blob: a.blob, Params: a.cfg.Params, MCPServers: a.mcpServers()}
}

func (a *Actor) start(id string, inputIDs []string) {
	r := a.newRun(kindTurn, id)
	req := a.modelRequest()
	req.TurnID, req.Foreign = id, a.state.Foreign(req.Model)
	caps := a.cfg.Backend.Capabilities(req.Model)
	r.steering, r.ownsLoop = caps.Steering || !caps.OwnsLoop, caps.OwnsLoop
	if r.steering {
		r.steered = make(chan struct{}, 1)
		req.Steered = r.steered
	}
	req.Questions = a.questions()
	req.Banner, req.Pins = a.cfg.Banner, a.pins.Pinned()
	for _, in := range inputIDs {
		ev, _, _ := a.state.Input(in)
		req.Input = append(req.Input, protocol.LogMessage{Role: protocol.RoleUser, Parts: ev.Parts})
	}
	a.run = r
	src := a.source(r)
	t := &turnRun{a: a, r: r}
	a.spawn(func() {
		a.awaitWarm(r.ctx)
		turn.Run(r.ctx, r.step, a.cfg.Backend, req, src, t, a.cfg.Limits)
	})
}

// item records m under itemID, or under a new ID when itemID is empty. After
// a stop or a handoff starts, it admits no new tool call, except from a
// backend that owns the loop: that backend has already run the call.
func (a *Actor) item(r *running, itemID string, m protocol.LogMessage) error {
	if a.run != r {
		return ErrTurnMismatch
	}
	if err := context.Cause(r.step); err != nil && !r.ownsLoop && slices.ContainsFunc(m.Parts, isCall) {
		return err
	}
	if itemID == "" {
		itemID = newID("item")
	}
	return a.append(protocol.ItemCompleted{ItemID: itemID, TurnID: r.id, Message: m})
}

// telemetry records what a model call of r measured. A call that measured
// nothing records nothing.
func (a *Actor) telemetry(r *running, t turn.Telemetry) error {
	if t.Call != nil {
		a.resolveWarm(t.Call)
	}
	if a.run != r {
		return nil
	}
	m := t.Context
	m.Usage, m.SubscriptionUsage, m.CostUSD = t.Usage, t.SubscriptionUsage, t.CostUSD
	if sub := m.SubscriptionUsage; sub != nil && sub.CapturedAt == 0 {
		stamped := *sub
		stamped.CapturedAt = time.Now().Unix()
		m.SubscriptionUsage = &stamped
	}
	if m == (protocol.ContextMeasured{}) {
		return nil
	}
	return a.append(m)
}

// steer promotes the queued steer inputs into r and returns them. A turn
// that is stopping, or that takes no steer input, gets none.
func (a *Actor) steer(r *running) ([]protocol.LogMessage, error) {
	if a.run != r {
		return nil, ErrTurnMismatch
	}
	if !r.steering || r.step.Err() != nil {
		return nil, nil
	}
	var ops, reports []protocol.Payload
	var steered [][]protocol.LogPart
	for _, in := range a.state.Queue() {
		if in.Delivery != protocol.DeliverySteer {
			continue
		}
		e := protocol.InputPromoted{InputID: in.InputID, TurnID: r.id}
		if eventlog.IsReport(in.Parts) {
			reports = append(reports, e)
		} else {
			ops = append(ops, e)
		}
		steered = append(steered, in.Parts)
	}
	events := append(ops, reports...)
	if len(events) == 0 {
		return nil, nil
	}
	if err := a.append(events...); err != nil {
		return nil, err
	}
	return eventlog.SteerMessages(steered), nil
}

func isCall(p protocol.LogPart) bool { return p.Type == protocol.LogPartToolCall }

// ended ends r by the cause of its stop, then starts the next queued input
// after a normal end or a user interrupt.
func (a *Actor) ended(r *running, runErr error) {
	if a.run != r {
		return
	}
	turnID := r.id
	a.run = nil
	cause := context.Cause(r.step)
	r.cancel(nil)
	var err error
	next := false
	switch {
	case runErr == nil:
		err = a.endTurn(a.cfg.Base, protocol.TurnEnded{TurnID: turnID, StopReason: protocol.StopCompleted}, cutOff)
		next = true
	case errors.Is(cause, turn.ErrHandoff):
		err = a.append(append(a.closeOpen(turnID, cutOff, false), protocol.TurnSuspended{TurnID: turnID, Cause: protocol.CauseHandoff})...)
	case errors.Is(cause, errTaskTurn):
		err = a.endTurn(a.cfg.Base, protocol.TurnEnded{TurnID: turnID, StopReason: protocol.StopInterrupted, Cause: protocol.CauseStopped, TaskCancel: true}, interrupted)
		next = true
	case errors.Is(cause, errStopTurn):
		err = a.endTurn(a.cfg.Base, protocol.TurnEnded{TurnID: turnID, StopReason: protocol.StopInterrupted, Cause: protocol.CauseStopped}, interrupted)
		next = true
	case errors.Is(cause, errEndTurn):
		err = a.endTurn(a.cfg.Base, protocol.TurnEnded{TurnID: turnID, StopReason: protocol.StopInterrupted, Cause: protocol.CauseEnded}, interrupted)
		next = true
	case errors.Is(cause, errGoalCleared):
		err = a.endTurn(a.cfg.Base, protocol.TurnEnded{TurnID: turnID, StopReason: protocol.StopInterrupted, Cause: protocol.CauseGoalCleared}, interrupted)
		next = true
	default:
		err = a.endTurn(a.cfg.Base, failedEnd(turnID, runErr), cutOff, a.goalStop(runErr)...)
		next = !errors.Is(runErr, turn.ErrExhausted)
	}
	var after func() error
	if next && err == nil {
		after = func() error { return a.settle(true) }
	}
	a.finishRun(r, runErr, err, after)
}

// next starts the next queued input. With check, a compaction runs first
// when the context reading passes the threshold.
func (a *Actor) next(check bool) error {
	q := a.state.Queue()
	if len(q) == 0 || check && a.autoCompact() {
		return nil
	}
	id, ids := newID("turn"), a.startInputs(q)
	if err := a.append(append(a.dismissRequests(), protocol.TurnStarted{TurnID: id, InputIDs: ids})...); err != nil {
		return err
	}
	a.start(id, ids)
	return nil
}

// startInputs returns the inputs of the turn that starts from q, which is not
// empty: the first input, and, on a backend that owns the loop, every report
// of a child, because that backend reads no report in a turn that it runs.
func (a *Actor) startInputs(q []protocol.InputAdmitted) []string {
	ids := []string{q[0].InputID}
	if !a.ownsLoop() {
		return ids
	}
	for _, in := range q[1:] {
		if eventlog.IsReport(in.Parts) {
			ids = append(ids, in.InputID)
		}
	}
	return ids
}

// ownsLoop reports whether the backend that runs the current turn runs its
// loop itself. With no turn, or a run that is not a turn, it reads the backend
// of the current model, which the next turn uses. A model change does not move
// a turn that runs.
func (a *Actor) ownsLoop() bool {
	if a.run != nil && a.run.kind == kindTurn {
		return a.run.ownsLoop
	}
	return a.cfg.Backend.Capabilities(a.state.Model()).OwnsLoop
}

func (a *Actor) endTurn(ctx context.Context, ended protocol.TurnEnded, text string, after ...protocol.Payload) error {
	awaiting := ended.StopReason == protocol.StopCompleted && len(a.state.Requests()) > 0
	events := a.closeOpen(ended.TurnID, text, awaiting)
	if awaiting {
		ended.StopReason = protocol.StopAwaitingInput
	}
	if ended.Cause == protocol.CauseCrashed {
		marker := protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: lostToRestart}}}
		events = append(events, protocol.ItemCompleted{ItemID: newID("item"), TurnID: ended.TurnID, Message: marker})
	}
	events = append(events, ended)
	return a.appendCtx(ctx, append(events, after...)...)
}

// failedEnd is the end of a turn that err failed. A usage limit of the
// provider is a cause of its own, and its message keeps no sentinel. Any other
// failure holds its class and message, so a reader never parses the text.
func failedEnd(turnID string, err error) protocol.TurnEnded {
	ended := protocol.TurnEnded{TurnID: turnID, StopReason: protocol.StopFailed}
	if errors.Is(err, turn.ErrExhausted) {
		ended.Cause = protocol.CauseProviderExhausted
		ended = withError(ended, strings.TrimPrefix(err.Error(), turn.ErrExhausted.Error()+": "))
		if pe, ok := provider.AsProviderExhausted(err); ok {
			ended.RecoverHint = boundedText(pe.RecoverHint, hintCap)
		}
		return ended
	}
	ended = withError(ended, strings.TrimPrefix(err.Error(), turn.ErrRetryable.Error()+": "))
	ended.ErrorClass = errorClass(err)
	return ended
}

// withError gives e the message of a failure at the bound of the log, and at
// the longer bound of a report to a parent when that differs.
func withError(e protocol.TurnEnded, msg string) protocol.TurnEnded {
	e.Error = plugin.SanitizeSessionError(msg)
	if detail := boundedText(msg, reasonCap); detail != e.Error {
		e.ErrorDetail = detail
	}
	return e
}

// errorClass types a failure that is not a usage limit, in the order that the
// runtime classifies it: a rate limit that outlasted the retries counts as a
// wall of the account, any other retryable class keeps its name, and an error
// that a retry cannot resolve is permanent.
func errorClass(err error) protocol.ErrorClass {
	class, retryable := provider.AsRetryable(err)
	switch {
	case provider.AsLoginRequired(err):
		return protocol.ErrorLoginRequired
	case errors.Is(err, context.DeadlineExceeded):
		return protocol.ErrorTimedOut
	case retryable && class == provider.RetryableRateLimited:
		return protocol.ErrorRateLimited
	case retryable:
		return protocol.ErrorClass(class)
	case provider.AsPermanent(err):
		return protocol.ErrorPermanent
	}
	return protocol.ErrorUnrecovered
}

// closeOpen dismisses every open request unless keep is set, which closes its
// tool call, and gives every other open tool call a result with text. A kept
// request holds only its own call open.
func (a *Actor) closeOpen(turnID, text string, keep bool) []protocol.Payload {
	var events []protocol.Payload
	if !keep {
		events = a.dismissRequests()
	}
	asked := map[string]bool{}
	for _, r := range a.state.Requests() {
		asked[r.RequestID] = true
	}
	for _, c := range a.state.OpenToolCalls() {
		if asked[c.CallID] {
			continue
		}
		part := protocol.LogPart{Type: protocol.LogPartToolResult, CallID: c.CallID, Name: c.Name, Text: text, IsError: true}
		msg := protocol.LogMessage{Role: protocol.RoleTool, Parts: []protocol.LogPart{part}}
		events = append(events, protocol.ItemCompleted{ItemID: newID("item"), TurnID: turnID, Message: msg})
	}
	return events
}

func (a *Actor) dismissRequests() []protocol.Payload {
	var events []protocol.Payload
	for _, r := range a.state.Requests() {
		events = append(events, protocol.RequestResolved{RequestID: r.RequestID, Resolution: protocol.ResolutionDismissed})
	}
	return events
}

// Interrupt stops the running turn, or only turnID when it is set, and
// returns after the turn has ended.
func (a *Actor) Interrupt(ctx context.Context, turnID string) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) { a.interrupt(turnID, errStopTurn, reply) })
	return err
}

// Cancel withdraws each queued input and stops the running turn in one
// step, so no queued input starts, and returns after the turn has ended.
func (a *Actor) Cancel(ctx context.Context) error { return a.cancel(ctx, errStopTurn) }

// CancelForEnd is Cancel for a session that the end of an ancestor stops. It
// ends the turn with cause ended, which tells Recover that the stop sends no
// report to the parent.
func (a *Actor) CancelForEnd(ctx context.Context) error { return a.cancel(ctx, errEndTurn) }

// CancelForTask is Cancel for a session that a task cancel stops. It ends
// the turn with cause stopped and the task_cancel mark, which tells Recover
// to route the report of the child again.
func (a *Actor) CancelForTask(ctx context.Context) error { return a.cancel(ctx, errTaskTurn) }

func (a *Actor) cancel(ctx context.Context, why error) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		var events []protocol.Payload
		for _, in := range a.state.Queue() {
			events = append(events, protocol.InputWithdrawn{InputID: in.InputID})
		}
		if len(events) > 0 {
			if err := a.append(events...); err != nil {
				reply(struct{}{}, err)
				return
			}
		}
		if err := a.stopIdleGoal(why); err != nil {
			reply(struct{}{}, err)
			return
		}
		a.interrupt("", why, reply)
	})
	return err
}

// stopIdleGoal settles the goal of a session that runs nothing after a cancel
// withdrew its queue: the end of the session ends the goal, and any other
// cancel blocks an active goal that nothing drives.
func (a *Actor) stopIdleGoal(why error) error {
	if a.run != nil {
		return nil
	}
	if a.goalStopped(why) {
		return a.endGoal(why)
	}
	return a.blockUndriven()
}

func (a *Actor) interrupt(turnID string, why error, reply func(struct{}, error)) {
	r := a.run
	if r != nil && r.kind == kindJudge && !a.goalStopped(why) {
		r = nil
	}
	switch {
	case r == nil && turnID == "":
		reply(struct{}{}, nil)
	case r == nil || turnID != "" && turnID != r.id:
		reply(struct{}{}, ErrTurnMismatch)
	default:
		if err := a.endGoal(why); err != nil {
			reply(struct{}{}, err)
			return
		}
		r.cancel(why)
		r.waiters = append(r.waiters, replyAppend(reply))
	}
}

// goalStopped reports whether a stop for why ends the goal: a user stop ends
// an active goal, and the end of the session ends every live goal.
func (a *Actor) goalStopped(why error) bool {
	g, _ := a.state.Goal()
	switch {
	case errors.Is(why, errEndTurn):
		return liveGoal(g.State)
	case errors.Is(why, errStopTurn):
		return g.State == protocol.GoalActive
	}
	return false
}

// endGoal clears the goal that goalStopped names, stops its retry timer, and
// withdraws the queued goal inputs, so no goal turn starts after the stop.
func (a *Actor) endGoal(why error) error {
	if !a.goalStopped(why) {
		return nil
	}
	reason := goalInterrupted
	if errors.Is(why, errEndTurn) {
		reason = goalEnded
	}
	a.stopRetry()
	return a.append(append(a.withdrawGoal(), protocol.GoalChanged{State: protocol.GoalCleared, Reason: reason})...)
}

// Release hands the session off: it suspends the running turn at an item
// boundary, stops the actor, waits for Sync to acknowledge the last record,
// and releases the ownership. An actor that Sync stopped returns the
// rejection.
func (a *Actor) Release(ctx context.Context) error {
	return a.halt(ctx, func(reply func(struct{}, error)) {
		a.requestStop(ErrReleased)
		a.beginRelease(reply)
	})
}

// requestStop gives the stop that a request begins its cause, unless an
// earlier request gave one.
func (a *Actor) requestStop(cause error) {
	if a.endCause == nil {
		a.endCause = cause
	}
}

func (a *Actor) beginRelease(reply func(struct{}, error)) {
	a.releasing = append(a.releasing, reply)
	if a.run == nil {
		a.stop(nil)
		return
	}
	a.run.handoff(turn.ErrHandoff)
}

// End stops the actor and releases its ownership, as Release does, and
// appends nothing. It fails with ErrBusy while a turn or a control command
// runs; a compaction or an evaluation that no command started stops as under
// Release. An actor that has stopped is ended already, so it returns nil.
func (a *Actor) End(ctx context.Context) error {
	err := a.halt(ctx, func(reply func(struct{}, error)) {
		if a.turnRuns() || len(a.state.Unfinished()) > 0 {
			reply(struct{}{}, ErrBusy)
			return
		}
		a.requestStop(ErrEnded)
		a.beginRelease(reply)
	})
	if errors.Is(err, ErrNotOwned) {
		return nil
	}
	return err
}

// ReleaseErr returns the error of Ownership.Release once the actor is done,
// and nil before.
func (a *Actor) ReleaseErr() error {
	select {
	case <-a.done:
		return a.releaseErr
	default:
		return nil
	}
}

func (a *Actor) turnRuns() bool { return a.run != nil && a.run.kind == kindTurn }

// halt runs begin on the actor goroutine, and returns once the actor that
// begin stopped has released its ownership.
func (a *Actor) halt(ctx context.Context, begin func(reply func(struct{}, error))) error {
	_, err := call(ctx, a, begin)
	if err != nil && !errors.Is(err, ErrNotOwned) {
		return err
	}
	select {
	case <-a.done:
		if joined := errors.Join(a.syncErr, a.releaseErr); joined != nil {
			return joined
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Actor) stop(err error) {
	a.stopWith(a.endCause)
	for _, w := range a.releasing {
		w(struct{}{}, err)
	}
	a.releasing = nil
}

func (a *Actor) mcpServers() map[string]turn.MCPServer {
	out := map[string]turn.MCPServer{}
	for n, s := range a.cfg.MCPServers {
		out[n] = turn.MCPServer{Command: s.Command, Env: s.Env, Dir: s.Dir, URL: s.URL, Headers: s.Headers}
	}
	return out
}
