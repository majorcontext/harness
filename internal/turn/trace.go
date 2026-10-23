package turn

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/majorcontext/harness/internal/tracing"
	"github.com/majorcontext/harness/protocol"
)

type seqKey struct{}

// callIdent names one logical model call: its ID, and its turn and its place in it.
type callIdent struct {
	callID string
	// turnID and seq are empty and zero for a call outside a turn. seq counts from 1 in the turn.
	turnID string
	seq    int
}

// nextIdent names a new model call under ctx. A retry of the call keeps its ident.
func nextIdent(ctx context.Context) callIdent {
	id := callIdent{callID: "call_" + strings.ToLower(rand.Text())}
	if id.turnID = ID(ctx); id.turnID != "" {
		if n, ok := ctx.Value(seqKey{}).(*atomic.Int64); ok {
			id.seq = int(n.Add(1))
		}
	}
	return id
}

// recorder reports the model call that its callInfo names: the turn_metrics
// line of each completed attempt, and the span of the call. Both come from
// callArgs, so they cannot drift.
type recorder struct {
	log  *slog.Logger
	call callInfo
	tr   tracing.Tracer
	// ctx is the context of the call. span is the span of the call, nil for a backend that owns its
	// loop, which reports a span for each call when it completes.
	ctx  context.Context
	span trace.Span
	// start holds the attributes that the request names.
	start []attribute.KeyValue
	// finished is set when a completed attempt named its stop reason.
	finished bool
}

// begin returns the recorder of the model call of req and the context that its requests run under.
func begin(ctx context.Context, lim Limits, b Backend, req Request, ownsLoop bool) (context.Context, *recorder) {
	r := &recorder{log: cmp.Or(lim.Log, slog.Default()), tr: lim.Trace, ctx: ctx}
	r.call = callOf(req, callIdent{}, 0, ownsLoop)
	r.start = startAttrs(b.Capabilities(req.Model), req, r.call)
	if !ownsLoop {
		r.call.callIdent = nextIdent(ctx)
		ctx, r.span = lim.Trace.StartCall(ctx, "chat "+tracing.ModelID(req.Model), trace.WithAttributes(r.start...))
	}
	return ctx, r
}

func startAttrs(caps Capabilities, req Request, c callInfo) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", tracing.ModelID(req.Model)),
		attribute.Bool("gen_ai.request.stream", true),
	}
	if caps.Provider != "" {
		attrs = append(attrs, attribute.String("gen_ai.provider.name", caps.Provider))
	}
	if req.SessionID != "" {
		attrs = append(attrs, attribute.String("gen_ai.conversation.id", req.SessionID))
	}
	if c.effort != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.reasoning.level", c.effort))
	}
	if req.MaxTokens > 0 {
		attrs = append(attrs, attribute.Int("gen_ai.request.max_tokens", req.MaxTokens))
	}
	if req.Compacted {
		attrs = append(attrs, attribute.Bool("gen_ai.conversation.compacted", true))
	}
	return attrs
}

// retry names the attempt that follows a failed one.
func (r *recorder) retry(n int) { r.call.retry = n }

// attempt adds the end of an attempt to the span.
func (r *recorder) attempt(n int, err error) {
	if r.span == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.Int("attempt", n)}
	if err != nil {
		attrs = append(attrs, attribute.String("error.type", errorType(err)))
	}
	r.span.AddEvent("attempt", trace.WithAttributes(attrs...))
}

// record logs the turn_metrics line of a completed attempt and sets its facts on the span.
func (r *recorder) record(t Telemetry) {
	owns := r.span == nil
	if owns {
		r.call.callIdent = nextIdent(r.ctx)
	}
	args := callArgs(r.call, t)
	r.log.Info("turn_metrics", args...)
	attrs := spanAttrs(args, t)
	r.finished = r.finished || t.Call.StopReason != ""
	if !owns {
		r.span.SetAttributes(attrs...)
		return
	}
	end := time.Now()
	_, span := r.tr.StartCall(r.ctx, "chat "+tracing.ModelID(r.call.model), trace.WithTimestamp(end.Add(-t.Call.TTFT-t.Call.Stream)),
		trace.WithAttributes(append(attrs, r.start...)...))
	span.End(trace.WithTimestamp(end))
}

// spanAttrs are the facts of a completed attempt: the keys of its turn_metrics
// line under harness., and the gen_ai attributes that name the same facts.
func spanAttrs(args []any, t Telemetry) []attribute.KeyValue {
	m, u := t.Call, t.Usage
	attrs := append(tracing.Pairs("harness.", args, spanSkip),
		attribute.Int64("gen_ai.usage.input_tokens", u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens),
		attribute.Int64("gen_ai.usage.output_tokens", u.OutputTokens),
		attribute.Float64("gen_ai.response.time_to_first_chunk", m.TTFT.Seconds()))
	if u.CacheReadTokens > 0 {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.cache_read.input_tokens", u.CacheReadTokens))
	}
	if u.CacheWriteTokens > 0 {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.cache_write.input_tokens", u.CacheWriteTokens))
	}
	if m.ResponseID != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.id", m.ResponseID))
	}
	if m.StopReason != "" {
		attrs = append(attrs, attribute.StringSlice("gen_ai.response.finish_reasons", []string{m.StopReason}))
	}
	if m.MaxOutputTokens > 0 {
		attrs = append(attrs, attribute.Int("gen_ai.request.max_tokens", m.MaxOutputTokens))
	}
	if m.Chained() && m.PreviousResponseID != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.previous_response.id", m.PreviousResponseID))
	}
	return attrs
}

// spanSkip lists the keys of a turn_metrics line that a gen_ai attribute carries.
var spanSkip = map[string]bool{
	"session_id": true, "model": true, "ttft_ms": true, "input_tokens": true, "output_tokens": true,
	"cache_read_tokens": true, "cache_write_tokens": true, "effort": true, "response_id": true, "stop_reason": true,
}

// end ends the span of the call. items are the messages the call produced.
func (r *recorder) end(req Request, items []protocol.LogMessage, err error) {
	if r.span == nil {
		return
	}
	if err != nil {
		tracing.Fail(r.span, errorType(err))
	}
	if err != nil && !r.finished {
		r.span.SetAttributes(attribute.StringSlice("gen_ai.response.finish_reasons", []string{"error"}))
	}
	if r.tr.Content() && r.span.IsRecording() {
		attrs := []attribute.KeyValue{
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.system_instructions", tracing.SystemInstructions(req.Instructions)),
			attribute.String("gen_ai.input.messages", tracing.Messages(req.History)),
			attribute.String("gen_ai.tool.definitions", tracing.ToolDefinitions(req.Tools)),
		}
		if len(items) > 0 {
			attrs = append(attrs, attribute.String("gen_ai.output.messages", tracing.Output(items, finishOf(items))))
		}
		r.span.AddEvent(tracing.DetailsEvent, trace.WithAttributes(attrs...))
	}
	r.span.End()
}

// finishOf is the finish reason of the output messages: a call that asks for a tool, or not.
func finishOf(items []protocol.LogMessage) string {
	if len(toolCalls(items)) > 0 {
		return "tool_call"
	}
	return "stop"
}

func errorType(err error) string {
	switch {
	case errors.Is(err, ErrContextOverflow):
		return "context_overflow"
	case errors.Is(err, ErrExhausted):
		return "provider_exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrRetryable):
		return "retryable"
	}
	return "_OTHER"
}
