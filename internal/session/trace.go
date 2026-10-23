package session

import (
	"cmp"
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/majorcontext/harness/internal/tracing"
	"github.com/majorcontext/harness/protocol"
)

// startSpan starts the span of turn id. It links to the trace of each input
// that started the turn, so a turn that runs after a handoff still joins the
// request that sent its input.
func (a *Actor) startSpan(id string, inputIDs []string) (context.Context, trace.Span) {
	var links []trace.Link
	for _, in := range inputIDs {
		if ev, _, ok := a.state.Input(in); ok {
			if l, ok := inputLink(ev); ok {
				links = append(links, l)
			}
		}
	}
	agent := cmp.Or(a.state.Agent(), "harness")
	return a.cfg.Limits.Trace.Start(a.cfg.Base, "invoke_agent "+agent, trace.WithSpanKind(trace.SpanKindInternal), trace.WithLinks(links...),
		trace.WithAttributes(attribute.String("gen_ai.operation.name", "invoke_agent"), attribute.String("gen_ai.agent.name", agent),
			attribute.String("gen_ai.conversation.id", a.cfg.ID), attribute.String("gen_ai.request.model", tracing.ModelID(a.state.Model())),
			attribute.String("harness.turn_id", id), attribute.StringSlice("harness.input_ids", inputIDs)))
}

// inputLink is the link to the trace context that in carries.
func inputLink(in protocol.InputAdmitted) (trace.Link, bool) {
	sc, err := tracing.Parse(in.Traceparent, in.Tracestate)
	return trace.Link{SpanContext: sc}, err == nil && sc.IsValid()
}
