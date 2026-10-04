package session

import "context"

// runKind names what the actor runs: a turn, a compaction, or a goal
// evaluation. The actor runs one at a time.
type runKind string

const (
	kindTurn       runKind = "turn"
	kindCompaction runKind = "compaction"
	kindJudge      runKind = "judge"
)

// running is the run of the actor. An interrupt or a lost ownership ends
// ctx, which stops the running tools. A handoff ends only step: running
// tools finish. A compaction and an evaluation have no tools, so both end
// together.
type running struct {
	id       string
	kind     runKind
	ctx      context.Context
	cancel   context.CancelCauseFunc
	step     context.Context
	handoff  context.CancelCauseFunc
	steering bool
	ownsLoop bool
	steered  chan struct{}
	// waiters receive the outcome of the run: the error of the run, and the
	// error of the append that records its end.
	waiters []func(runErr, appendErr error)
}

// newRun returns a run of kind with a ctx that Config.Base bounds.
func (a *Actor) newRun(kind runKind, id string) *running {
	ctx, cancel := context.WithCancelCause(a.cfg.Base)
	r := &running{id: id, kind: kind, ctx: ctx, cancel: cancel, step: ctx, handoff: cancel}
	if kind == kindTurn {
		r.step, r.handoff = context.WithCancelCause(ctx)
	}
	return r
}

// replyAppend answers a waiter that wants only the error of the append.
func replyAppend(reply func(struct{}, error)) func(runErr, appendErr error) {
	return func(_, appendErr error) { reply(struct{}{}, appendErr) }
}

// finishRun ends the run r after its end is recorded: it answers each
// waiter, re-arms the retry of a paused goal, and then stops the actor when
// a release waits, or calls next, which may be nil.
func (a *Actor) finishRun(r *running, runErr, appendErr error, next func() error) {
	a.retryLater()
	for _, w := range r.waiters {
		w(runErr, appendErr)
	}
	if len(a.releasing) > 0 {
		a.stop(appendErr)
		return
	}
	if next != nil {
		_ = next()
	}
}
