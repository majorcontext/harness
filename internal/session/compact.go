package session

import (
	"cmp"
	"context"
	"errors"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
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
		done := func(runErr, appendErr error) {
			c, _ := a.state.Compaction()
			reply(result{c, true}, errors.Join(runErr, appendErr))
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
			a.run.waiters = append(a.run.waiters, func(runErr, appendErr error) {
				reply(result{eventlog.CompactionApplied{ByBackend: true}, true}, errors.Join(runErr, appendErr))
			})
		case !a.compact(cmp.Or(keep, a.cfg.KeepTurns), done):
			reply(result{}, nil)
		}
	})
	return r.c, r.ran, err
}

// autoCompact starts a compaction when overThreshold, and reports whether one started.
func (a *Actor) autoCompact() bool { return a.overThreshold() && a.compact(a.cfg.KeepTurns, nil) }

// overThreshold reports whether the newest context reading passes
// Config.Threshold of the window of the session model, or of the window of
// the reading when the model reports none, for a backend that does not own
// its context.
func (a *Actor) overThreshold() bool {
	c, caps := a.state.Context(), a.cfg.Backend.Capabilities(a.state.Model())
	window := contextWindow(caps.ContextWindow, c)
	return window > 0 && float64(c.Tokens) >= a.cfg.Threshold*float64(window) && !caps.OwnsContext
}

// compact runs a summary of the turns before the newest keep as the run of
// the actor, and reports false when no turn can fold. done, when it is not
// nil, receives the outcome.
func (a *Actor) compact(keep int, done func(runErr, appendErr error)) bool {
	id := newID("compaction")
	req, c, ok := a.fold(id, keep)
	if !ok {
		return false
	}
	metas := a.retained()
	r := a.newRun(kindCompaction, id)
	if done != nil {
		r.waiters = append(r.waiters, done)
	}
	a.run = r
	a.frame(protocol.KindStatus, protocol.StatusFrame{Status: protocol.StatusCompacting, TurnID: id})
	a.spawn(func() {
		summary, usage, err := turn.Summarize(r.ctx, a.cfg.Backend, req, a.cfg.Limits.Idle)
		c.Summary, c.Usage = a.indexed(summary, metas), usage
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
	req := turn.Request{SessionID: a.cfg.ID, TurnID: id, Model: a.state.Model(), Settings: a.state.Settings(), History: folded, Blob: a.blob}
	return req, c, true
}

// recordUsage records the usage of a model call that produced no other
// record. A call with no usage records nothing.
func (a *Actor) recordUsage(u eventlog.Usage) error {
	if u == (eventlog.Usage{}) {
		return nil
	}
	return a.append(eventlog.ContextMeasured{Usage: u})
}

// compacted appends the summary of run r, then starts the next queued
// input with no new compaction. A failed summary appends only its usage.
func (a *Actor) compacted(r *running, c eventlog.CompactionApplied, runErr error) {
	if a.run != r {
		return
	}
	a.run = nil
	r.cancel(nil)
	var appendErr error
	switch {
	case len(a.releasing) > 0:
		runErr = ErrNotOwned
	case runErr == nil:
		appendErr = a.append(c)
	}
	if runErr != nil {
		appendErr = a.recordUsage(c.Usage)
	}
	status := protocol.StatusIdle
	if runErr != nil || appendErr != nil {
		status = protocol.StatusCompactionFailed
	}
	a.frame(protocol.KindStatus, protocol.StatusFrame{Status: status, TurnID: r.id})
	a.finishRun(r, runErr, appendErr, func() error { return a.settle(false) })
}
