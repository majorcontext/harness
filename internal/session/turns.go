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
	steerBuffer = 16
)

var (
	errStopTurn = errors.New("harness: turn stopped")
	errHandoff  = errors.New("harness: turn handed off")
)

type running struct {
	id      string
	ctx     context.Context
	cancel  context.CancelCauseFunc
	steer   chan eventlog.Message
	waiters []func(struct{}, error)
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
	r := &running{id: id, ctx: ctx, cancel: cancel}
	req := turn.Request{SessionID: a.cfg.ID, TurnID: id, Model: a.state.Model(), Resumed: resumed}
	if a.cfg.Backend.Capabilities(req.Model).Steering {
		r.steer = make(chan eventlog.Message, steerBuffer)
		req.Steer = r.steer
	}
	for _, in := range inputIDs {
		ev, _, _ := a.state.Input(in)
		req.Input = append(req.Input, eventlog.Message{Role: eventlog.RoleUser, Parts: ev.Parts})
	}
	a.run = r
	a.cfg.Go(func() { turn.Run(ctx, a.cfg.Backend, req, a) })
}

// Item records one completed message of turnID and folds queued steer inputs
// into the turn. After a stop or a handoff starts, it admits no new tool call.
func (a *Actor) Item(ctx context.Context, turnID string, m eventlog.Message) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) { reply(struct{}{}, a.item(turnID, m)) })
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
	events := []eventlog.Event{eventlog.ItemCompleted{ItemID: newID("item"), TurnID: turnID, Message: m}}
	var steered []eventlog.Message
	for _, in := range a.state.Queue() {
		if r.steer == nil || r.ctx.Err() != nil || len(r.steer)+len(steered) == cap(r.steer) {
			break
		}
		if in.Delivery == eventlog.DeliverySteer {
			events = append(events, eventlog.InputPromoted{InputID: in.InputID, TurnID: turnID})
			steered = append(steered, eventlog.Message{Role: eventlog.RoleUser, Parts: in.Parts})
		}
	}
	if err := a.append(events...); err != nil {
		return err
	}
	for _, in := range steered {
		r.steer <- in
	}
	return nil
}

func isCall(p eventlog.Part) bool { return p.Type == eventlog.PartToolCall }

// Ended ends turnID by the cause of its stop, then starts the next queued
// input after a normal end or a user interrupt.
func (a *Actor) Ended(turnID string, res turn.Result, runErr error) {
	_, _ = call(context.Background(), a, func(reply func(struct{}, error)) {
		a.ended(turnID, res, runErr)
		reply(struct{}{}, nil)
	})
}

func (a *Actor) ended(turnID string, res turn.Result, runErr error) {
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
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopCompleted, "", cutOff, res.Usage)
		next = true
	case errors.Is(cause, errHandoff):
		err = a.append(append(a.closeOpen(turnID, cutOff), eventlog.TurnSuspended{TurnID: turnID, Cause: eventlog.CauseHandoff})...)
	case errors.Is(cause, errStopTurn):
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopInterrupted, string(eventlog.CauseStopped), interrupted, res.Usage)
		next = true
	default:
		err = a.endTurn(a.cfg.Base, turnID, eventlog.StopFailed, runErr.Error(), cutOff, res.Usage)
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
// boundary, stops the actor, and releases the ownership.
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
		return nil
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
