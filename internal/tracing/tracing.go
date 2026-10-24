// Package tracing builds the OpenTelemetry spans of harness. The zero Tracer
// records nothing and sends no trace context.
package tracing

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/majorcontext/harness/internal/modelref"
)

const scope = "github.com/majorcontext/harness"

// maxTracestate is the longest tracestate that the W3C Trace Context allows.
const maxTracestate = 512

var nop = noop.NewTracerProvider().Tracer(scope)

// Tracer starts the spans of harness.
type Tracer struct {
	tr      trace.Tracer
	content bool
}

// New returns a Tracer over tp. A nil tp gives the zero Tracer. With content,
// a model call records its messages and a tool call its arguments and result.
func New(tp trace.TracerProvider, content bool) Tracer {
	if tp == nil {
		return Tracer{}
	}
	return Tracer{tr: tp.Tracer(scope), content: content}
}

// Content reports whether spans carry the messages, arguments, and results.
func (t Tracer) Content() bool { return t.content }

// Start starts a span.
func (t Tracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if t.tr == nil {
		return nop.Start(ctx, name, opts...)
	}
	return t.tr.Start(ctx, name, opts...)
}

type callKey struct{}

type linkKey struct{}

// WithLink returns ctx whose model calls link to sc. An invalid sc adds none.
func WithLink(ctx context.Context, sc trace.SpanContext) context.Context {
	if !sc.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, linkKey{}, sc)
}

// ContextFromHeaders returns ctx with the remote span context of the request
// headers h, or ctx when they hold none.
func ContextFromHeaders(ctx context.Context, h http.Header) context.Context {
	tp, ts := FromHeaders(h)
	sc, err := Parse(tp, ts)
	if err != nil || !sc.IsValid() {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

// FromHeaders returns the traceparent and tracestate of the request headers h.
// A traceparent that is not valid gives empty strings. A tracestate that is too
// long is dropped.
func FromHeaders(h http.Header) (traceparent, tracestate string) {
	tp := h.Get("traceparent")
	if tp == "" {
		return "", ""
	}
	if _, err := Parse(tp, ""); err != nil {
		return "", ""
	}
	if ts := h.Get("tracestate"); len(ts) <= maxTracestate {
		return tp, ts
	}
	return tp, ""
}

// StartCall starts the CLIENT span of a model call. The Transport of an HTTP
// client sends its trace context on each request made under the returned ctx.
func (t Tracer) StartCall(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if sc, ok := ctx.Value(linkKey{}).(trace.SpanContext); ok {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
	}
	ctx, span := t.Start(ctx, name, append(opts, trace.WithSpanKind(trace.SpanKindClient))...)
	return context.WithValue(ctx, callKey{}, true), span
}

// Scope is a Tracer with the span that spans of a backend that calls tools
// from its own goroutines hang from.
type Scope struct {
	Tracer
	parent trace.Span
}

// Scope returns the Scope of the span in ctx.
func (t Tracer) Scope(ctx context.Context) Scope {
	return Scope{Tracer: t, parent: trace.SpanFromContext(ctx)}
}

// Start starts a span under ctx, or under the span of the Scope when ctx has none.
func (s Scope) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if s.parent != nil && !trace.SpanContextFromContext(ctx).IsValid() {
		ctx = trace.ContextWithSpan(ctx, s.parent)
	}
	return s.Tracer.Start(ctx, name, opts...)
}

// Parse returns the span context of a traceparent and a tracestate. Both empty
// give the zero value. A traceparent that is not valid is an error.
func Parse(traceparent, tracestate string) (trace.SpanContext, error) {
	if traceparent == "" {
		return trace.SpanContext{}, nil
	}
	if len(tracestate) > maxTracestate {
		return trace.SpanContext{}, fmt.Errorf("tracestate exceeds %d bytes", maxTracestate)
	}
	h := http.Header{}
	h.Set("traceparent", traceparent)
	h.Set("tracestate", tracestate)
	sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(h)))
	if !sc.IsValid() {
		return trace.SpanContext{}, fmt.Errorf("traceparent is not valid")
	}
	return sc, nil
}

// Headers returns the traceparent and tracestate of the span in ctx, or empty strings.
func Headers(ctx context.Context) (traceparent, tracestate string) {
	h := http.Header{}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(h))
	return h.Get("traceparent"), h.Get("tracestate")
}

// Format returns the traceparent and tracestate of sc, or empty strings when sc is not valid.
func Format(sc trace.SpanContext) (traceparent, tracestate string) {
	return Headers(trace.ContextWithSpanContext(context.Background(), sc))
}

// metaCarrier is a propagation carrier over a map.
type metaCarrier map[string]string

// Get implements propagation.TextMapCarrier.
func (c metaCarrier) Get(k string) string { return c[k] }

// Set implements propagation.TextMapCarrier.
func (c metaCarrier) Set(k, v string) { c[k] = v }

// Keys implements propagation.TextMapCarrier.
func (c metaCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// Meta returns the trace context of the span in ctx for the _meta of an MCP request, or nil.
func Meta(ctx context.Context) map[string]string {
	m := metaCarrier{}
	propagation.TraceContext{}.Inject(ctx, m)
	if len(m) == 0 {
		return nil
	}
	return m
}

// ModelID returns the model id of a model reference, without its provider.
func ModelID(model string) string {
	if ref, err := modelref.ParseModelRef(model); err == nil {
		return ref.Model
	}
	return model
}

// Fail marks span as failed with the low-cardinality error type errType.
func Fail(span trace.Span, errType string) {
	span.SetAttributes(attribute.String("error.type", errType))
	span.SetStatus(codes.Error, "")
}

// Pairs turns the alternating keys and values of a log line into attributes
// named prefix and key. A key in skip is left out.
func Pairs(prefix string, args []any, skip map[string]bool) []attribute.KeyValue {
	var out []attribute.KeyValue
	for i := 0; i+1 < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok || skip[k] {
			continue
		}
		key := attribute.Key(prefix + k)
		switch v := args[i+1].(type) {
		case string:
			out = append(out, key.String(v))
		case int:
			out = append(out, key.Int(v))
		case int64:
			out = append(out, key.Int64(v))
		case bool:
			out = append(out, key.Bool(v))
		default:
			out = append(out, key.String(fmt.Sprint(v)))
		}
	}
	return out
}
