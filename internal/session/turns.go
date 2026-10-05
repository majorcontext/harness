package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
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
)

// Submit admits in and returns the seq of its input.admitted record. A
// repeated input ID returns the original seq and repeat set.
func (a *Actor) Submit(ctx context.Context, in eventlog.InputAdmitted, expectedTurn string) (seq uint64, repeat bool, err error) {
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
func (a *Actor) admit(in eventlog.InputAdmitted, expectedTurn string, before ...eventlog.Event) (uint64, error) {
	if in.Delivery == eventlog.DeliverySteer && expectedTurn != "" && (a.run == nil || a.run.id != expectedTurn) {
		return 0, ErrTurnMismatch
	}
	events := append(append(before, a.dismissRequests()...), in)
	seq := a.state.Head() + uint64(len(events))
	r := a.run
	if r == nil && !a.overThreshold() {
		ids := a.startInputs(append(a.state.Queue(), in))
		id := newID("turn")
		if err := a.append(append(events, eventlog.TurnStarted{TurnID: id, InputIDs: ids})...); err != nil {
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
	if in.Delivery == eventlog.DeliverySteer && r.steering {
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

func (a *Actor) start(id string, inputIDs []string) {
	r := a.newRun(kindTurn, id)
	req := turn.Request{SessionID: a.cfg.ID, TurnID: id, Model: a.state.Model(), Settings: a.state.Settings(), Instructions: a.cfg.Prompt(),
		History: a.state.History(), AllowedTools: a.state.AllowedTools(), Foreign: a.state.Foreign(a.state.Model()), Blob: a.blob}
	caps := a.cfg.Backend.Capabilities(req.Model)
	r.steering, r.ownsLoop = caps.Steering || !caps.OwnsLoop, caps.OwnsLoop
	if r.steering {
		r.steered = make(chan struct{}, 1)
		req.Steered = r.steered
	}
	req.Questions = a.questions()
	if a.cfg.Banner != "" && !r.ownsLoop {
		req.Banner, req.BannerAt = a.cfg.Banner, a.bannerAt(len(req.History))
	}
	for _, in := range inputIDs {
		ev, _, _ := a.state.Input(in)
		req.Input = append(req.Input, eventlog.Message{Role: eventlog.RoleUser, Parts: ev.Parts})
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
func (a *Actor) item(r *running, itemID string, m eventlog.Message) error {
	if a.run != r {
		return ErrTurnMismatch
	}
	if err := context.Cause(r.step); err != nil && !r.ownsLoop && slices.ContainsFunc(m.Parts, isCall) {
		return err
	}
	if itemID == "" {
		itemID = newID("item")
	}
	return a.append(eventlog.ItemCompleted{ItemID: itemID, TurnID: r.id, Message: m})
}

// telemetry records what a model call of r measured. A call that measured
// nothing records nothing.
func (a *Actor) telemetry(r *running, t turn.Telemetry) error {
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
	if m == (eventlog.ContextMeasured{}) {
		return nil
	}
	return a.append(m)
}

// steer promotes the queued steer inputs into r and returns them. A turn
// that is stopping, or that takes no steer input, gets none.
func (a *Actor) steer(r *running) ([]eventlog.Message, error) {
	if a.run != r {
		return nil, ErrTurnMismatch
	}
	if !r.steering || r.step.Err() != nil {
		return nil, nil
	}
	var events []eventlog.Event
	var steered [][]eventlog.Part
	for _, in := range a.state.Queue() {
		if in.Delivery == eventlog.DeliverySteer {
			events = append(events, eventlog.InputPromoted{InputID: in.InputID, TurnID: r.id})
			steered = append(steered, in.Parts)
		}
	}
	if len(events) == 0 {
		return nil, nil
	}
	if err := a.append(events...); err != nil {
		return nil, err
	}
	return []eventlog.Message{eventlog.SteerMessage(steered)}, nil
}

func isCall(p eventlog.Part) bool { return p.Type == eventlog.PartToolCall }

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
		err = a.endTurn(a.cfg.Base, eventlog.TurnEnded{TurnID: turnID, StopReason: eventlog.StopCompleted}, cutOff)
		next = true
	case errors.Is(cause, turn.ErrHandoff):
		err = a.append(append(a.closeOpen(turnID, cutOff, false), eventlog.TurnSuspended{TurnID: turnID, Cause: eventlog.CauseHandoff})...)
	case errors.Is(cause, errStopTurn):
		err = a.endTurn(a.cfg.Base, eventlog.TurnEnded{TurnID: turnID, StopReason: eventlog.StopInterrupted, Cause: eventlog.CauseStopped}, interrupted)
		next = true
	case errors.Is(cause, errEndTurn):
		err = a.endTurn(a.cfg.Base, eventlog.TurnEnded{TurnID: turnID, StopReason: eventlog.StopInterrupted, Cause: eventlog.CauseEnded}, interrupted)
		next = true
	case errors.Is(cause, errGoalCleared):
		err = a.endTurn(a.cfg.Base, eventlog.TurnEnded{TurnID: turnID, StopReason: eventlog.StopInterrupted, Cause: eventlog.CauseGoalCleared}, interrupted)
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
	if err := a.append(append(a.dismissRequests(), eventlog.TurnStarted{TurnID: id, InputIDs: ids})...); err != nil {
		return err
	}
	a.start(id, ids)
	return nil
}

// startInputs returns the inputs of the turn that starts from q, which is not
// empty: the first input, and, on a backend that owns the loop, every report
// of a child, because that backend reads no report in a turn that it runs.
func (a *Actor) startInputs(q []eventlog.InputAdmitted) []string {
	ids := []string{q[0].InputID}
	if !a.ownsLoop() {
		return ids
	}
	for _, in := range q[1:] {
		if isReport(in.Parts) {
			ids = append(ids, in.InputID)
		}
	}
	return ids
}

// isReport reports whether parts hold the report of a child. A caller cannot
// write a task report part, so this tells a report from a prompt that names
// source child.
func isReport(parts []eventlog.Part) bool {
	return slices.ContainsFunc(parts, func(p eventlog.Part) bool { return p.Type == eventlog.PartTaskReport })
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

func (a *Actor) endTurn(ctx context.Context, ended eventlog.TurnEnded, text string, after ...eventlog.Event) error {
	awaiting := ended.StopReason == eventlog.StopCompleted && len(a.state.Requests()) > 0
	events := a.closeOpen(ended.TurnID, text, awaiting)
	if awaiting {
		ended.StopReason = eventlog.StopAwaitingInput
	}
	if ended.Cause == eventlog.CauseCrashed {
		marker := eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: lostToRestart}}}
		events = append(events, eventlog.ItemCompleted{ItemID: newID("item"), TurnID: ended.TurnID, Message: marker})
	}
	events = append(events, ended)
	return a.appendCtx(ctx, append(events, after...)...)
}

// failedEnd is the end of a turn that err failed. A usage limit of the
// provider is a cause of its own, and its message keeps no sentinel. Any other
// failure holds its class and message, so a reader never parses the text.
func failedEnd(turnID string, err error) eventlog.TurnEnded {
	ended := eventlog.TurnEnded{TurnID: turnID, StopReason: eventlog.StopFailed}
	if errors.Is(err, turn.ErrExhausted) {
		ended.Cause = eventlog.CauseProviderExhausted
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
func withError(e eventlog.TurnEnded, msg string) eventlog.TurnEnded {
	e.Error = plugin.SanitizeSessionError(msg)
	if detail := boundedText(msg, reasonCap); detail != e.Error {
		e.ErrorDetail = detail
	}
	return e
}

// errorClass types a failure that is not a usage limit, in the order that the
// engine classified it: a rate limit that outlasted the retries counts as a
// wall of the account, any other retryable class keeps its name, and an error
// that a retry cannot resolve is permanent.
func errorClass(err error) eventlog.ErrorClass {
	class, retryable := provider.AsRetryable(err)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return eventlog.ErrorTimedOut
	case retryable && class == provider.RetryableRateLimited:
		return eventlog.ErrorRateLimited
	case retryable:
		return eventlog.ErrorClass(class)
	case provider.AsPermanent(err):
		return eventlog.ErrorPermanent
	}
	return eventlog.ErrorUnrecovered
}

// closeOpen dismisses every open request unless keep is set, which closes its
// tool call, and gives every other open tool call a result with text. A kept
// request holds only its own call open.
func (a *Actor) closeOpen(turnID, text string, keep bool) []eventlog.Event {
	var events []eventlog.Event
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
		part := eventlog.Part{Type: eventlog.PartToolResult, CallID: c.CallID, Name: c.Name, Text: text, IsError: true}
		msg := eventlog.Message{Role: eventlog.RoleTool, Parts: []eventlog.Part{part}}
		events = append(events, eventlog.ItemCompleted{ItemID: newID("item"), TurnID: turnID, Message: msg})
	}
	return events
}

func (a *Actor) dismissRequests() []eventlog.Event {
	var events []eventlog.Event
	for _, r := range a.state.Requests() {
		events = append(events, eventlog.RequestResolved{RequestID: r.RequestID, Resolution: eventlog.ResolutionDismissed})
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

func (a *Actor) cancel(ctx context.Context, why error) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		var events []eventlog.Event
		for _, in := range a.state.Queue() {
			events = append(events, eventlog.InputWithdrawn{InputID: in.InputID})
		}
		if len(events) > 0 {
			if err := a.append(events...); err != nil {
				reply(struct{}{}, err)
				return
			}
		}
		a.interrupt("", why, reply)
	})
	return err
}

func (a *Actor) interrupt(turnID string, why error, reply func(struct{}, error)) {
	r := a.run
	if r != nil && r.kind == kindJudge {
		r = nil
	}
	switch {
	case r == nil && turnID == "":
		reply(struct{}{}, nil)
	case r == nil || turnID != "" && turnID != r.id:
		reply(struct{}{}, ErrTurnMismatch)
	default:
		r.cancel(why)
		r.waiters = append(r.waiters, replyAppend(reply))
	}
}

// Release hands the session off: it suspends the running turn at an item
// boundary, stops the actor, waits for Sync to acknowledge the last record,
// and releases the ownership. An actor that Sync stopped returns the
// rejection.
func (a *Actor) Release(ctx context.Context) error {
	return a.halt(ctx, a.beginRelease)
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
		a.beginRelease(reply)
	})
	if errors.Is(err, ErrNotOwned) {
		return nil
	}
	return err
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
		if a.syncErr != nil {
			return a.syncErr
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Actor) stop(err error) {
	a.stopped = true
	for _, w := range a.releasing {
		w(struct{}{}, err)
	}
	a.releasing = nil
}
