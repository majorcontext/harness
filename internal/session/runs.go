package session

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel/trace"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/turn"
)

// runKind names what the actor runs: a turn, a compaction, or a goal
// evaluation. The actor runs one at a time.
type runKind = turn.Purpose

const (
	kindTurn       = turn.PurposeTurn
	kindCompaction = turn.PurposeCompaction
	kindJudge      = turn.PurposeJudge
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
	// span is the span of a turn; nil for another kind of run.
	span trace.Span
	// turnTexts is the turn context that the turn took, in input order, and
	// turnPins is the part of it that a model call placed. Only the actor
	// goroutine reads and writes them.
	turnTexts []string
	turnPins  []turn.Pin
	// statusPins holds the transient notices that the model calls of the turn placed. A kind keeps the text and
	// the place of its first call for the rest of the turn.
	statusPins []turn.Pin
}

// takeContext moves the turn context of input id from the actor into r.
func (r *running) takeContext(a *Actor, id string) {
	if text, ok := a.turnCtx[id]; ok {
		r.turnTexts = append(r.turnTexts, text)
		delete(a.turnCtx, id)
	}
}

// steerContext appends to the first of msgs, which holds the steer inputs that are no report, as engine context, each
// turn context that r took and no message carried, for a backend that owns the loop: it reads a steer input only in
// the message of that input.
func (r *running) steerContext(msgs []protocol.LogMessage) []protocol.LogMessage {
	if len(msgs) == 0 {
		return msgs
	}
	first := &msgs[0]
	first.Parts = slices.Clip(first.Parts)
	for _, text := range r.turnTexts[len(r.turnPins):] {
		first.Parts = append(first.Parts, protocol.LogPart{Type: protocol.LogPartEngineContext, Text: prompt.RenderEngineContext(text)})
		r.turnPins = append(r.turnPins, turn.Pin{Kind: turn.TurnContextKind, Text: text})
	}
	return msgs
}

// moveContext puts each placed turn context after the first n history messages.
func (r *running) moveContext(n int) {
	for i := range r.turnPins {
		r.turnPins[i].At = n
	}
	for i := range r.statusPins {
		r.statusPins[i].At = n
	}
}

// placeStatus places each transient notice of ns that the turn has not placed yet after the first at messages and
// returns every placed one as a pin.
func (r *running) placeStatus(ns []turn.Notice, at int) []turn.Pin {
	for _, n := range ns {
		if n.Transient && n.Text != "" && !slices.ContainsFunc(r.statusPins, func(p turn.Pin) bool { return p.Kind == n.Kind }) {
			r.statusPins = append(r.statusPins, turn.Pin{Kind: n.Kind, Text: n.Text, At: at})
		}
	}
	return r.statusPins
}

// placeContext places the turn context that no model call placed after the
// first at messages and returns every placed text as a pin. A text keeps its
// place for the rest of the turn.
func (r *running) placeContext(at int) []turn.Pin {
	for _, text := range r.turnTexts[len(r.turnPins):] {
		r.turnPins = append(r.turnPins, turn.Pin{Kind: turn.TurnContextKind, Text: text, At: at})
	}
	return r.turnPins
}

// newRun returns a run of kind with a ctx that Config.Base bounds.
func (a *Actor) newRun(kind runKind, id string) *running {
	return a.newRunUnder(a.cfg.Base, kind, id)
}

// newRunUnder returns a run of kind with a ctx that base bounds.
func (a *Actor) newRunUnder(base context.Context, kind runKind, id string) *running {
	ctx, cancel := context.WithCancelCause(base)
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
