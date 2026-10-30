package session

import (
	"cmp"
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/majorcontext/harness/internal/tracing"
	"github.com/majorcontext/harness/protocol"
)

// startSpan starts the span of turn id. A turn that starts from one input
// with a trace context continues that trace as a child of it. A turn that
// starts from none or from several is a root that links to the trace of each
// such input, so a turn that runs after a handoff still joins the request that
// sent its input.
func (a *Actor) startSpan(id string, inputIDs []string) (context.Context, trace.Span) {
	var links []trace.Link
	for _, in := range inputIDs {
		if ev, _, ok := a.state.Input(in); ok {
			if l, ok := inputLink(ev); ok {
				links = append(links, l)
			}
		}
	}
	base := a.cfg.Base
	if len(links) == 1 {
		base = trace.ContextWithRemoteSpanContext(base, links[0].SpanContext)
		links = nil
	}
	agent := cmp.Or(a.state.Agent(), "harness")
	return a.cfg.Limits.Trace.Start(base, "invoke_agent "+agent, trace.WithSpanKind(trace.SpanKindInternal), trace.WithLinks(links...),
		trace.WithAttributes(attribute.String("gen_ai.operation.name", "invoke_agent"), attribute.String("gen_ai.agent.name", agent),
			attribute.String("gen_ai.conversation.id", a.cfg.ID), attribute.String("gen_ai.request.model", tracing.ModelID(a.state.Model())),
			attribute.String("harness.turn_id", id), attribute.StringSlice("harness.input_ids", inputIDs)))
}

// turnSpan is the span context of turn id.
type turnSpan struct {
	id string
	sc trace.SpanContext
}

// endedSpan returns the span context of the last ended turn, or the zero value.
func (a *Actor) endedSpan() trace.SpanContext {
	if id := a.state.LastEnded().TurnID; id != "" && id == a.lastTurn.id {
		return a.lastTurn.sc
	}
	return trace.SpanContext{}
}

// causedBy sets the trace context of in to sc, which ties the turn of in to its cause.
func causedBy(in protocol.InputAdmitted, sc trace.SpanContext) protocol.InputAdmitted {
	in.Traceparent, in.Tracestate = tracing.Format(sc)
	return in
}

// inputLink is the link to the trace context that in carries.
func inputLink(in protocol.InputAdmitted) (trace.Link, bool) {
	sc, err := tracing.Parse(in.Traceparent, in.Tracestate)
	return trace.Link{SpanContext: sc}, err == nil && sc.IsValid()
}
