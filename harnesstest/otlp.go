package harnesstest

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Span is a span that an OTLP receiver decoded. An attribute is a string, an int64, a float64, a bool, or a []string.
type Span struct {
	Name                          string
	Kind                          string
	TraceID, SpanID, ParentSpanID string
	Attrs                         map[string]any
	// Links holds the "traceID-spanID" of each link.
	Links  []string
	Events []SpanEvent
	Failed bool
}

// SpanEvent is an event of a Span.
type SpanEvent struct {
	Name  string
	Attrs map[string]any
}

// OTLP is a fake OTLP/HTTP trace receiver. It keeps every span that it receives.
type OTLP struct {
	srv   *httptest.Server
	mu    sync.Mutex
	spans []Span
}

// NewOTLP starts a receiver that t closes.
func NewOTLP(t testing.TB) *OTLP {
	t.Helper()
	o := &OTLP{}
	o.srv = httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(o.srv.Close)
	return o
}

// URL is the endpoint for OTEL_EXPORTER_OTLP_ENDPOINT.
func (o *OTLP) URL() string { return o.srv.URL }

// Spans returns the spans received so far, in arrival order.
func (o *OTLP) Spans() []Span {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Span(nil), o.spans...)
}

func (o *OTLP) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	var req coltrace.ExportTraceServiceRequest
	if err != nil || r.URL.Path != "/v1/traces" || proto.Unmarshal(body, &req) != nil {
		http.Error(w, "harnesstest: not an OTLP trace export", http.StatusBadRequest)
		return
	}
	o.mu.Lock()
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				o.spans = append(o.spans, spanOf(s))
			}
		}
	}
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
}

func spanOf(s *tracepb.Span) Span {
	out := Span{Name: s.Name, Kind: s.Kind.String(), TraceID: hex.EncodeToString(s.TraceId), SpanID: hex.EncodeToString(s.SpanId),
		Attrs: attrsOf(s.Attributes), Failed: s.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR}
	if len(s.ParentSpanId) > 0 {
		out.ParentSpanID = hex.EncodeToString(s.ParentSpanId)
	}
	for _, l := range s.Links {
		out.Links = append(out.Links, hex.EncodeToString(l.TraceId)+"-"+hex.EncodeToString(l.SpanId))
	}
	for _, e := range s.Events {
		out.Events = append(out.Events, SpanEvent{Name: e.Name, Attrs: attrsOf(e.Attributes)})
	}
	return out
}

func attrsOf(kvs []*commonpb.KeyValue) map[string]any {
	out := map[string]any{}
	for _, kv := range kvs {
		switch v := kv.Value.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			out[kv.Key] = v.StringValue
		case *commonpb.AnyValue_IntValue:
			out[kv.Key] = v.IntValue
		case *commonpb.AnyValue_DoubleValue:
			out[kv.Key] = v.DoubleValue
		case *commonpb.AnyValue_BoolValue:
			out[kv.Key] = v.BoolValue
		case *commonpb.AnyValue_ArrayValue:
			var items []string
			for _, item := range v.ArrayValue.Values {
				items = append(items, item.GetStringValue())
			}
			out[kv.Key] = items
		}
	}
	return out
}
