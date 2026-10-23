package tracing

import (
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Client returns a copy of hc that sends the trace context of a model call. A nil hc is the default client.
func Client(hc *http.Client) *http.Client {
	if hc == nil {
		return &http.Client{Transport: Transport(nil)}
	}
	c := *hc
	c.Transport = Transport(hc.Transport)
	return &c
}

// Transport returns a RoundTripper over base that sends the traceparent and
// tracestate of the model call span in the request context, and records the
// server and each response on that span. A WebSocket upgrade carries none,
// because one connection serves many calls. A nil base is http.DefaultTransport.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return transport{base}
}

type transport struct{ base http.RoundTripper }

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	span := trace.SpanFromContext(ctx)
	if ctx.Value(callKey{}) == nil || !span.SpanContext().IsValid() || req.Header.Get("Upgrade") != "" {
		return t.base.RoundTrip(req)
	}
	req = req.Clone(ctx)
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	if span.IsRecording() {
		span.SetAttributes(serverOf(req)...)
	}
	resp, err := t.base.RoundTrip(req)
	switch {
	case err != nil:
		span.AddEvent("http.request", trace.WithAttributes(attribute.Bool("error", true)))
	default:
		span.AddEvent("http.request", trace.WithAttributes(attribute.Int("http.response.status_code", resp.StatusCode)))
	}
	return resp, err
}

func serverOf(req *http.Request) []attribute.KeyValue {
	port := req.URL.Port()
	if port == "" {
		port = "80"
		if req.URL.Scheme == "https" {
			port = "443"
		}
	}
	n, _ := strconv.Atoi(port)
	return []attribute.KeyValue{attribute.String("server.address", req.URL.Hostname()), attribute.Int("server.port", n)}
}
