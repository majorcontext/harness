package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

const (
	cutOff      = "cut off before a result was recorded; check whether it took effect before running it again"
	interrupted = "interrupted before a result was recorded; check whether it took effect before running it again"
)

var (
	errStopTurn = errors.New("harness: turn stopped")
	errHandoff  = errors.New("harness: turn handed off")
)

type running struct {
	id       string
	ctx      context.Context
	cancel   context.CancelCauseFunc
	steering bool
	usage    eventlog.Usage
	waiters  []func(struct{}, error)
}

// Submit admits in and returns the seq of its input.admitted record. A
// repeated input ID returns the original seq.
func (a *Actor) Submit(ctx context.Context, in eventlog.InputAdmitted, expectedTurn string) (uint64, error) {
	return call(ctx, a, func(reply func(uint64, error)) { reply(a.admit(in, expectedTurn)) })
}

func (a *Actor) admit(in eventlog.InputAdmitted, expectedTurn string) (uint64, error) {
	if old, seq, ok := a.state.Input(in.InputID); ok {
		if !sameJSON(old, in) {
			return 0, ErrInputConflict
		}
		return seq, nil
	}
	if in.Delivery == eventlog.DeliverySteer && expectedTurn != "" && (a.run == nil || a.run.id != expectedTurn) {
		return 0, ErrTurnMismatch
	}
	events := append(a.dismissRequests(), in)
	seq := a.state.Head() + uint64(len(events))
	if a.run != nil {
		return seq, a.append(events...)
	}
	next := in.InputID
	if q := a.state.Queue(); len(q) > 0 {
		next = q[0].InputID
	}
	id := newID("turn")
	if err := a.append(append(events, eventlog.TurnStarted{TurnID: id, InputIDs: []string{next}})...); err != nil {
		return 0, err
	}
	a.start(id, []string{next}, 0)
	return seq, nil
}

func sameJSON(x, y any) bool {
	bx, errx := json.Marshal(x)
	by, erry := json.Marshal(y)
	return errx == nil && erry == nil && string(bx) == string(by)
}

func (a *Actor) start(id string, inputIDs []string, resumed int) {
	ctx, cancel := context.WithCancelCause(a.cfg.Base)
	req := turn.Request{SessionID: a.cfg.ID, TurnID: id, Model: a.state.Model(), Settings: a.state.Settings(),
		History: a.state.History(), Resumed: resumed}
	r := &running{id: id, ctx: ctx, cancel: cancel, steering: a.cfg.Backend.Capabilities(req.Model).Steering}
	for _, in := range inputIDs {
		ev, _, _ := a.state.Input(in)
		req.Input = append(req.Input, eventlog.Message{Role: eventlog.RoleUser, Parts: ev.Parts})
	}
	a.run = r
	a.cfg.Go(func() { turn.Run(ctx, a.cfg.Backend, req, a, a.cfg.Retries) })
}

// Item records one completed message of turnID. After a stop or a handoff
// starts, it admits no new tool call.
func (a *Actor) Item(turnID string, m eventlog.Message) error {
	_, err := call(context.Background(), a, func(reply func(struct{}, error)) { reply(struct{}{}, a.item(turnID, m)) })
	return err
}

func (a *Actor) item(turnID string, m eventlog.Message) error {
	r := a.run
	if r == nil || r.id != turnID {
		return ErrTurnMismatch
	}
	if err := context.Cause(r.ctx); err != nil && slices.ContainsFunc(m.Parts, isCall) {
		return err
	}
	return a.append(eventlog.ItemCompleted{ItemID: newID("item"), TurnID: turnID, Message: m})
}

// Telemetry adds the usage in t to turnID.
func (a *Actor) Telemetry(turnID string, t turn.Telemetry) {
	_, _ = call(context.Background(), a, func(reply func(struct{}, error)) {
		if r := a.run; r != nil && r.id == turnID {
			r.usage = r.usage.Add(t.Usage)
		}
		reply(struct{}{}, nil)
	})
}

// Steer promotes the queued steer inputs into turnID and returns them. A
// backend without Steering, or a turn that is stopping, gets none.
func (a *Actor) Steer(turnID string) ([]eventlog.Message, error) {
	return call(context.Background(), a, func(reply func([]eventlog.Message, error)) { reply(a.steer(turnID)) })
}

func (a *Actor) steer(turnID string) ([]eventlog.Message, error) {
	r := a.run
	if r == nil || r.id != turnID {
		return nil, ErrTurnMismatch
	}
	if !r.steering || r.ctx.Err() != nil {
		return nil, nil
	}
	var events []eventlog.Event
	var steered []eventlog.Message
	for _, in := range a.state.Queue() {
		if in.Delivery == eventlog.DeliverySteer {
			events = append(events, eventlog.InputPromoted{InputID: in.InputID, TurnID: turnID})
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

// Ended ends turnID by the cause of its stop, then starts the next queued
// input after a normal end or a user interrupt.
func (a *Actor) Ended(turnID string, runErr error) {
	_, _ = call(context.Background(), a, func(reply func(struct{}, error)) {
		a.ended(turnID, runErr)
		reply(struct{}{}, nil)
	})
}

func (a *Actor) ended(turnID string, runErr error) {
	r := a.run
	if r == nil || r.id != turnID {
		return
	}
	a.run = nil
	cause := context.Cause(r.ctx)
	r.cancel(nil)
	var err error
	next := false
	switch {
	case runErr == nil:
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopCompleted, "", cutOff, r.usage)
		next = true
	case errors.Is(cause, errHandoff):
		err = a.append(append(a.closeOpen(turnID, cutOff), eventlog.TurnSuspended{TurnID: turnID, Cause: eventlog.CauseHandoff})...)
	case errors.Is(cause, errStopTurn):
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopInterrupted, string(eventlog.CauseStopped), interrupted, r.usage)
		next = true
	default:
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopFailed, runErr.Error(), cutOff, r.usage)
	}
	for _, w := range r.waiters {
		w(struct{}{}, err)
	}
	if len(a.releasing) > 0 {
		a.stop(err)
		return
	}
	if next && err == nil {
		a.next()
	}
}

func (a *Actor) next() {
	q := a.state.Queue()
	if len(q) == 0 {
		return
	}
	id := newID("turn")
	if err := a.append(eventlog.TurnStarted{TurnID: id, InputIDs: []string{q[0].InputID}}); err == nil {
		a.start(id, []string{q[0].InputID}, 0)
	}
}

func (a *Actor) endTurn(ctx context.Context, turnID string, reason eventlog.StopReason, cause, text string, u eventlog.Usage) error {
	events := append(a.closeOpen(turnID, text), eventlog.TurnEnded{TurnID: turnID, StopReason: reason, Error: cause, Usage: u})
	return a.appendCtx(ctx, events...)
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
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		r := a.run
		switch {
		case r == nil && turnID == "":
			reply(struct{}{}, nil)
		case r == nil || turnID != "" && turnID != r.id:
			reply(struct{}{}, ErrTurnMismatch)
		default:
			r.cancel(errStopTurn)
			r.waiters = append(r.waiters, reply)
		}
	})
	return err
}

// Release hands the session off: it suspends the running turn at an item
// boundary, stops the actor, waits for Sync to acknowledge the last record,
// and releases the ownership.
func (a *Actor) Release(ctx context.Context) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		a.releasing = append(a.releasing, reply)
		if a.run == nil {
			a.stop(nil)
			return
		}
		a.run.cancel(errHandoff)
	})
	if err != nil {
		return err
	}
	select {
	case <-a.done:
		return a.syncErr
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
