package session

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"go.opentelemetry.io/otel/trace"

	"github.com/majorcontext/harness/internal/tracing"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// ErrBusy reports a Compact while a turn or a compaction runs, or inputs wait.
var ErrBusy = &protocol.Error{Code: protocol.CodeSessionBusy, Message: "harness: session busy"}

// ErrKeepTurns reports a keep count for a backend that owns its context.
var ErrKeepTurns = errors.New("keep_turns does not apply to a backend that owns its context")

// compactCommand asks a backend that owns its context to compact it.
const compactCommand = "/compact"

// Compact folds the turns before the newest keep, or Config.KeepTurns when
// keep is 0, into a summary, or runs compactCommand as a turn of a backend
// that owns its context. The compaction links to the valid span context in
// ctx: the input of the command holds it, and the summary call of a
// compaction that the actor runs links to it. It returns when the compaction
// ends, with the compaction that it appended, or one with only ByBackend for
// a backend that owns its context. It fails with ErrTurnMismatch when
// expectTurn is not empty and is not the newest ended turn. When too few turns exist, it
// appends nothing and ran is false.
func (a *Actor) Compact(ctx context.Context, keep int, expectTurn string) (c protocol.CompactionApplied, ran bool, err error) {
	type result struct {
		c   protocol.CompactionApplied
		ran bool
	}
	link := trace.SpanContextFromContext(ctx)
	traceparent, tracestate := tracing.Format(link)
	r, err := call(ctx, a, func(reply func(result, error)) {
		done := func(runErr, appendErr error) {
			c, _ := a.state.Compaction()
			reply(result{c, true}, errors.Join(runErr, appendErr))
		}
		owns := a.cfg.Backend.Capabilities(a.state.Model()).OwnsContext
		switch {
		case a.run != nil || len(a.state.Queue()) > 0:
			reply(result{}, ErrBusy)
		case expectTurn != "" && a.state.LastEnded().TurnID != expectTurn:
			reply(result{}, ErrTurnMismatch)
		case owns && keep != 0:
			reply(result{}, ErrKeepTurns)
		case owns:
			in := protocol.InputAdmitted{InputID: newID("input"), Delivery: protocol.DeliveryQueue, Source: "harness",
				Traceparent: traceparent, Tracestate: tracestate,
				Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: compactCommand}}}
			a.compactInput = in.InputID
			if _, err := a.admit(in, "", a.dismissRequests()...); err != nil {
				a.compactInput = ""
				reply(result{}, err)
				return
			}
			a.run.waiters = append(a.run.waiters, func(runErr, appendErr error) {
				reply(result{protocol.CompactionApplied{ByBackend: true}, true}, errors.Join(runErr, appendErr))
			})
		case !a.compact(cmp.Or(keep, a.cfg.KeepTurns), done, link):
			reply(result{}, nil)
		}
	})
	return r.c, r.ran, err
}

// autoCompact starts a compaction when compactDue, and reports whether one started.
func (a *Actor) autoCompact() bool {
	return a.compactDue() && a.compact(a.cfg.KeepTurns, func(runErr, appendErr error) {
		a.latched = appendErr == nil && (runErr == nil || errors.Is(runErr, turn.ErrEmptySummary))
	}, trace.SpanContext{})
}

// compactDue reports whether the context passes Config.Threshold of the
// window of the session model, or of the window of the newest reading when the
// model reports none, for a backend that does not own its context. The context
// is the newest reading, or an estimate of the history when a turn has ended and
// the newest turn recorded no reading. After an automatic compaction applied, or paid
// for an empty summary, the context must fall below the threshold once before
// another one is due: the pressure sits in the kept turns.
func (a *Actor) compactDue() bool {
	c, caps := a.state.Context(), a.cfg.Backend.Capabilities(a.state.Model())
	window := contextWindow(caps.ContextWindow, c, caps.OwnsContext)
	if window <= 0 || caps.OwnsContext {
		return false
	}
	tokens := c.Tokens
	if !a.state.Measured() && a.state.LastEnded().TurnID != "" {
		tokens = estimateTokens(a.state.History())
	}
	over := float64(tokens) >= a.cfg.Threshold*float64(window)
	if !over {
		a.latched = false
	}
	return over && !a.latched
}

// compact runs a summary of the turns before the newest keep as the run of
// the actor, and reports false when no turn can fold. done, when it is not
// nil, receives the outcome. The summary call links to link when it is valid.
func (a *Actor) compact(keep int, done func(runErr, appendErr error), link trace.SpanContext) bool {
	id := newID("compaction")
	req, c, ok := a.fold(id, keep)
	if !ok {
		return false
	}
	metas := a.retained()
	r := a.newRun(kindCompaction, id)
	src := a.source(r)
	if done != nil {
		r.waiters = append(r.waiters, done)
	}
	a.run = r
	a.frame(protocol.KindStatus, protocol.StatusFrame{Status: protocol.StatusCompacting, TurnID: id})
	a.spawn(func() {
		summary, usage, err := a.summarize(tracing.WithLink(r.ctx, link), req, src)
		c.Summary, c.Index, c.Usage = summary, a.resultIndex(metas), usage
		_, _ = call(context.Background(), a, func(reply func(struct{}, error)) {
			a.compacted(r, c, err)
			reply(struct{}{}, nil)
		})
	})
	return true
}

// summarize runs the summary request req, described from src as a turn is.
// When the request overflows the context window, it drops the oldest message
// after the first of the history, with the tool results that answer it, and
// runs again, until a summary comes back or only the first message is left.
func (a *Actor) summarize(ctx context.Context, req turn.Request, src turn.Source) (string, protocol.Usage, error) {
	req, err := turn.SummaryRequest(ctx, a.cfg.Backend, req, src)
	if err != nil {
		return "", protocol.Usage{}, err
	}
	var total protocol.Usage
	for {
		text, usage, err := turn.Summarize(ctx, a.cfg.Backend, req, a.cfg.Limits)
		total = total.Add(usage)
		if !errors.Is(err, turn.ErrContextOverflow) || len(req.History) < 2 {
			return text, total, err
		}
		rest := req.History[2:]
		for len(rest) > 0 && rest[0].Role == protocol.RoleTool {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return "", total, err
		}
		dropped := len(req.History) - 1 - len(rest)
		req.History = append([]protocol.LogMessage{req.History[0]}, rest...)
		req.Pins = slices.Clone(req.Pins)
		for i := range req.Pins {
			if req.Pins[i].At > 1 {
				req.Pins[i].At = max(1, req.Pins[i].At-dropped)
			}
		}
	}
}

// fold returns the summary request, which holds the whole history of the
// session, and the compaction that records the summary of the turns before the
// newest keep.
func (a *Actor) fold(id string, keep int) (turn.Request, protocol.CompactionApplied, bool) {
	to, ok := a.state.Fold(keep)
	if !ok {
		return turn.Request{}, protocol.CompactionApplied{}, false
	}
	c := protocol.CompactionApplied{FromSeq: 1, ToSeq: to}
	if prev, ok := a.state.Compaction(); ok {
		c.FromSeq = prev.ToSeq + 1
	}
	req := a.callRequest(id)
	req.Purpose = turn.PurposeCompaction
	return req, c, true
}

// recordUsage records the usage of a model call that produced no other
// record. A call with no usage records nothing.
func (a *Actor) recordUsage(u protocol.Usage) error {
	if u == (protocol.Usage{}) {
		return nil
	}
	return a.append(protocol.ContextMeasured{Usage: u})
}

// compactionOutcome returns the status that ends a failed compaction whose run stopped with cause: canceled when cause is an
// interrupt or the end of an ancestor session, else status.
func compactionOutcome(status protocol.Status, cause error) protocol.Status {
	if status == protocol.StatusCompactionFailed && stopsByRequest(cause) {
		return protocol.StatusCompactionCanceled
	}
	return status
}

// stopsByRequest reports whether cause is an interrupt or the end of an ancestor session, the stops that a compaction reports as canceled.
func stopsByRequest(cause error) bool {
	return errors.Is(cause, errStopTurn) || errors.Is(cause, errEndTurn)
}

// compacted appends the summary of run r, then starts the next queued
// input with no new compaction. A failed summary appends only its usage.
func (a *Actor) compacted(r *running, c protocol.CompactionApplied, runErr error) {
	if a.run != r {
		return
	}
	a.run = nil
	cause := context.Cause(r.ctx)
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
		status = compactionOutcome(protocol.StatusCompactionFailed, cause)
	}
	a.frame(protocol.KindStatus, protocol.StatusFrame{Status: status, TurnID: r.id})
	a.finishRun(r, runErr, appendErr, func() error { return a.settle(false) })
}
