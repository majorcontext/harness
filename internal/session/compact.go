package session

import (
	"cmp"
	"context"
	"errors"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
)

// ErrBusy reports a Compact while a turn or a compaction runs, or inputs wait.
var ErrBusy = errors.New("harness: session busy")

// ErrKeepTurns reports a keep count for a backend that owns its context.
var ErrKeepTurns = errors.New("keep_turns does not apply to a backend that owns its context")

// compactCommand asks a backend that owns its context to compact it.
const compactCommand = "/compact"

// Compact folds the turns before the newest keep, or Config.KeepTurns when
// keep is 0, into a summary, or runs compactCommand as a turn of a backend
// that owns its context. It returns when the compaction ends, with the
// compaction that it appended, or one with only ByBackend for a backend
// that owns its context. When too few turns exist, it appends nothing and
// ran is false.
func (a *Actor) Compact(ctx context.Context, keep int) (c eventlog.CompactionApplied, ran bool, err error) {
	type result struct {
		c   eventlog.CompactionApplied
		ran bool
	}
	r, err := call(ctx, a, func(reply func(result, error)) {
		done := func(_ struct{}, err error) {
			c, _ := a.state.Compaction()
			reply(result{c, true}, err)
		}
		owns := a.cfg.Backend.Capabilities(a.state.Model()).OwnsContext
		switch {
		case a.run != nil || len(a.state.Queue()) > 0:
			reply(result{}, ErrBusy)
		case owns && keep != 0:
			reply(result{}, ErrKeepTurns)
		case owns:
			in := eventlog.InputAdmitted{InputID: newID("input"), Delivery: eventlog.DeliveryQueue, Source: "harness",
				Parts: []eventlog.Part{{Type: eventlog.PartText, Text: compactCommand}}}
			if _, err := a.admit(in, ""); err != nil {
				reply(result{}, err)
				return
			}
			a.run.done = func(_ struct{}, err error) { reply(result{eventlog.CompactionApplied{ByBackend: true}, true}, err) }
		case !a.compact(cmp.Or(keep, a.cfg.KeepTurns), done):
			reply(result{}, nil)
		}
	})
	return r.c, r.ran, err
}

// autoCompact starts a compaction when overThreshold, and reports whether one started.
func (a *Actor) autoCompact() bool { return a.overThreshold() && a.compact(a.cfg.KeepTurns, nil) }

// overThreshold reports whether the newest context reading passes
// Config.Threshold of its window, for a backend that does not own its context.
func (a *Actor) overThreshold() bool {
	c := a.state.Context()
	over := c.Window > 0 && float64(c.Tokens) >= a.cfg.Threshold*float64(c.Window)
	return over && !a.cfg.Backend.Capabilities(a.state.Model()).OwnsContext
}

// compact runs a summary of the turns before the newest keep as the run of
// the actor, and reports false when no turn can fold. done receives the outcome.
func (a *Actor) compact(keep int, done func(struct{}, error)) bool {
	id := newID("compaction")
	req, c, ok := a.fold(id, keep)
	if !ok {
		return false
	}
	metas := a.retained()
	ctx, cancel := context.WithCancelCause(a.cfg.Base)
	r := &running{id: id, ctx: ctx, cancel: cancel, step: ctx, handoff: cancel, done: done}
	a.run = r
	a.cfg.Go(func() {
		summary, err := turn.Summarize(ctx, a.cfg.Backend, req, a.cfg.Limits.Idle)
		c.Summary = a.indexed(summary, metas)
		_, _ = call(context.Background(), a, func(reply func(struct{}, error)) {
			a.compacted(r, c, err)
			reply(struct{}{}, nil)
		})
	})
	return true
}

// fold returns the summary request of the turns before the newest keep,
// and the compaction that records the summary.
func (a *Actor) fold(id string, keep int) (turn.Request, eventlog.CompactionApplied, bool) {
	folded, to, ok := a.state.Fold(keep)
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
		req   turn.Request
		c     eventlog.CompactionApplied
		ok    bool
		metas []toolresult.Meta
	}
	f, err := call(ctx, a, func(reply func(folding, error)) {
		if r := a.run; r == nil || r.id != turnID {
			reply(folding{}, ErrTurnMismatch)
			return
		}
		req, c, ok := a.fold(turnID, a.cfg.KeepTurns)
		reply(folding{req, c, ok, a.retained()}, nil)
	})
	if err != nil || !f.ok {
		return nil, false, err
	}
	summary, err := turn.Summarize(ctx, a.cfg.Backend, f.req, a.cfg.Limits.Idle)
	if err != nil {
		return nil, false, err
	}
	f.c.Summary = a.indexed(summary, f.metas)
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
	_ = a.settle(false)
}
