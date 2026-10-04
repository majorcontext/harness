package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

const (
	cutOff      = "cut off before a result was recorded; check whether it took effect before running it again"
	interrupted = "interrupted before a result was recorded; check whether it took effect before running it again"
)

var errStopTurn = errors.New("harness: turn stopped")

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
		next := in.InputID
		if q := a.state.Queue(); len(q) > 0 {
			next = q[0].InputID
		}
		id := newID("turn")
		if err := a.append(append(events, eventlog.TurnStarted{TurnID: id, InputIDs: []string{next}})...); err != nil {
			return 0, err
		}
		a.start(id, []string{next})
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
	req := turn.Request{SessionID: a.cfg.ID, TurnID: id, Model: a.state.Model(), Settings: a.state.Settings(), Instructions: a.cfg.Prompt(a.state.Agent()),
		History: a.state.History(), AllowedTools: a.state.AllowedTools()}
	caps := a.cfg.Backend.Capabilities(req.Model)
	r.steering, r.ownsLoop = caps.Steering || !caps.OwnsLoop, caps.OwnsLoop
	if r.steering {
		r.steered = make(chan struct{}, 1)
		req.Steered = r.steered
	}
	for _, in := range inputIDs {
		ev, _, _ := a.state.Input(in)
		req.Input = append(req.Input, eventlog.Message{Role: eventlog.RoleUser, Parts: ev.Parts})
	}
	a.run = r
	tools, src := a.turnTools(r)
	t := &turnRun{a: a, r: r}
	a.spawn(func() {
		a.awaitWarm(r.ctx)
		turn.Run(r.ctx, r.step, a.cfg.Backend, req, tools, src, t, a.cfg.Limits)
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
	m.Usage, m.SubscriptionUsage = t.Usage, t.SubscriptionUsage
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
	var steered []eventlog.Message
	for _, in := range a.state.Queue() {
		if in.Delivery == eventlog.DeliverySteer {
			events = append(events, eventlog.InputPromoted{InputID: in.InputID, TurnID: r.id})
			steered = append(steered, eventlog.Message{Role: eventlog.RoleUser, Parts: in.Parts})
		}
	}
	if len(events) == 0 {
		return nil, nil
	}
	if err := a.append(events...); err != nil {
		return nil, err
	}
	return steered, nil
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
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopCompleted, "", cutOff)
		next = true
	case errors.Is(cause, turn.ErrHandoff):
		err = a.append(append(a.closeOpen(turnID, cutOff), eventlog.TurnSuspended{TurnID: turnID, Cause: eventlog.CauseHandoff})...)
	case errors.Is(cause, errStopTurn):
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopInterrupted, string(eventlog.CauseStopped), interrupted)
		next = true
	case errors.Is(cause, errGoalCleared):
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopInterrupted, string(eventlog.CauseGoalCleared), interrupted)
		next = true
	case errors.Is(runErr, turn.ErrExhausted):
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopFailed, string(eventlog.CauseProviderExhausted), cutOff, a.goalStop(runErr)...)
	default:
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopFailed, runErr.Error(), cutOff, a.goalStop(runErr)...)
		next = true
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
	id := newID("turn")
	if err := a.append(eventlog.TurnStarted{TurnID: id, InputIDs: []string{q[0].InputID}}); err != nil {
		return err
	}
	a.start(id, []string{q[0].InputID})
	return nil
}

func (a *Actor) endTurn(ctx context.Context, turnID string, reason eventlog.StopReason, cause, text string, after ...eventlog.Event) error {
	events := append(a.closeOpen(turnID, text), eventlog.TurnEnded{TurnID: turnID, StopReason: reason, Error: cause})
	if err := a.appendCtx(ctx, append(events, after...)...); err != nil {
		return err
	}
	a.report()
	return nil
}

// closeOpen dismisses every open request, which closes its tool call, and
// gives every other open tool call a result with text.
func (a *Actor) closeOpen(turnID, text string) []eventlog.Event {
	events := a.dismissRequests()
	asked := map[string]bool{}
	for _, r := range a.state.Requests() {
		asked[r.ItemID] = true
	}
	for _, c := range a.state.OpenToolCalls() {
		if asked[c.ItemID] {
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
	_, err := call(ctx, a, func(reply func(struct{}, error)) { a.interrupt(turnID, reply) })
	return err
}

// Cancel withdraws each queued input and stops the running turn in one
// step, so no queued input starts, and returns after the turn has ended.
func (a *Actor) Cancel(ctx context.Context) error {
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
		a.interrupt("", reply)
	})
	return err
}

func (a *Actor) interrupt(turnID string, reply func(struct{}, error)) {
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
		r.cancel(errStopTurn)
		r.waiters = append(r.waiters, replyAppend(reply))
	}
}

// Release hands the session off: it suspends the running turn at an item
// boundary, stops the actor, waits for Sync to acknowledge the last record,
// and releases the ownership. An actor that Sync stopped returns the
// rejection.
func (a *Actor) Release(ctx context.Context) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		a.releasing = append(a.releasing, reply)
		if a.run == nil {
			a.stop(nil)
			return
		}
		a.run.handoff(turn.ErrHandoff)
	})
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
