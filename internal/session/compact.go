package session

import (
	"cmp"
	"context"
	"errors"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// ErrBusy reports a Compact while a turn or a compaction runs, or inputs wait.
var ErrBusy = errors.New("harness: session busy")

// compactCommand asks a backend that owns its context to compact it.
const compactCommand = "/compact"

// Compact folds the turns before the newest Config.KeepTurns into a
// summary, or runs compactCommand as a turn of a backend that owns its
// context. It returns when the compaction ends. When too few turns exist,
// it appends nothing.
func (a *Actor) Compact(ctx context.Context) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		switch {
		case a.run != nil || len(a.state.Queue()) > 0:
			reply(struct{}{}, ErrBusy)
		case a.cfg.Backend.Capabilities(a.state.Model()).OwnsContext:
			in := eventlog.InputAdmitted{InputID: newID("input"), Delivery: eventlog.DeliveryQueue, Source: "harness",
				Parts: []eventlog.Part{{Type: eventlog.PartText, Text: compactCommand}}}
			if _, err := a.admit(in, ""); err != nil {
				reply(struct{}{}, err)
				return
			}
			a.run.done = reply
		case !a.compact(reply):
			reply(struct{}{}, nil)
		}
	})
	return err
}

// autoCompact starts a compaction when overThreshold, and reports whether one started.
func (a *Actor) autoCompact() bool { return a.overThreshold() && a.compact(nil) }

// overThreshold reports whether the newest context reading passes
// Config.Threshold of its window, for a backend that does not own its context.
func (a *Actor) overThreshold() bool {
	c := a.state.Context()
	over := c.Window > 0 && float64(c.Tokens) >= a.cfg.Threshold*float64(c.Window)
	return over && !a.cfg.Backend.Capabilities(a.state.Model()).OwnsContext
}

// compact runs a summary of the folded turns as the run of the actor, and
// reports false when no turn can fold. done receives the outcome.
func (a *Actor) compact(done func(struct{}, error)) bool {
	id := newID("compaction")
	req, c, ok := a.fold(id)
	if !ok {
		return false
	}
	ctx, cancel := context.WithCancelCause(a.cfg.Base)
	r := &running{id: id, ctx: ctx, cancel: cancel, step: ctx, handoff: cancel, done: done}
	a.run = r
	a.cfg.Go(func() {
		summary, err := turn.Summarize(ctx, a.cfg.Backend, req, a.cfg.Limits.Idle)
		c.Summary = summary
		_, _ = call(context.Background(), a, func(reply func(struct{}, error)) {
			a.compacted(r, c, err)
			reply(struct{}{}, nil)
		})
	})
	return true
}

// fold returns the summary request of the turns before the newest
// Config.KeepTurns, and the compaction that records the summary.
func (a *Actor) fold(id string) (turn.Request, eventlog.CompactionApplied, bool) {
	folded, to, ok := a.state.Fold(a.cfg.KeepTurns)
	if !ok {
		return turn.Request{}, eventlog.CompactionApplied{}, false
	}
	c := eventlog.CompactionApplied{FromSeq: 1, ToSeq: to}
	if prev, ok := a.state.Compaction(); ok {
		c.FromSeq = prev.ToSeq + 1
	}
	req := turn.Request{SessionID: a.cfg.ID, TurnID: id, Model: a.state.Model(), Settings: a.state.Settings(), History: folded}
	return req, c, true
}

// CompactTurn folds the turns before the newest Config.KeepTurns into a
// summary while turnID runs, and returns the new history. ok is false when
// no turn can fold. When ctx ends, it appends nothing.
func (a *Actor) CompactTurn(ctx context.Context, turnID string) ([]eventlog.Message, bool, error) {
	type folding struct {
		req turn.Request
		c   eventlog.CompactionApplied
		ok  bool
	}
	f, err := call(ctx, a, func(reply func(folding, error)) {
		if r := a.run; r == nil || r.id != turnID {
			reply(folding{}, ErrTurnMismatch)
			return
		}
		req, c, ok := a.fold(turnID)
		reply(folding{req, c, ok}, nil)
	})
	if err != nil || !f.ok {
		return nil, false, err
	}
	if f.c.Summary, err = turn.Summarize(ctx, a.cfg.Backend, f.req, a.cfg.Limits.Idle); err != nil {
		return nil, false, err
	}
	h, err := call(ctx, a, func(reply func([]eventlog.Message, error)) {
		if r := a.run; r == nil || r.id != turnID || ctx.Err() != nil {
			reply(nil, cmp.Or(context.Cause(ctx), ErrTurnMismatch))
			return
		}
		err := a.append(f.c)
		reply(a.state.History(), err)
	})
	return h, err == nil, err
}

// compacted appends the summary of run r, then starts the next queued
// input with no new compaction. A failed summary appends nothing.
func (a *Actor) compacted(r *running, c eventlog.CompactionApplied, err error) {
	if a.run != r {
		return
	}
	a.run = nil
	r.cancel(nil)
	if len(a.releasing) > 0 {
		err = ErrNotOwned
	}
	if err == nil {
		err = a.append(c)
	}
	for _, w := range r.waiters {
		w(struct{}{}, nil)
	}
	if r.done != nil {
		r.done(struct{}{}, err)
	}
	if len(a.releasing) > 0 {
		a.stop(nil)
		return
	}
	_ = a.next(false)
}
