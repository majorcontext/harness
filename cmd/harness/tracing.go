package main

import (
	"context"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// flushBudget bounds the export of the spans that wait when the process ends.
const flushBudget = 5 * time.Second

// tracing is the trace setup of a command: the provider that Options takes,
// and the flush at the end.
type tracing struct {
	provider trace.TracerProvider
	content  bool
	sdk      *sdktrace.TracerProvider
}

// startTracing returns the tracing of the process. With neither OTEL_EXPORTER_OTLP_ENDPOINT
// nor OTEL_EXPORTER_OTLP_TRACES_ENDPOINT set it records nothing. The exporter reads the rest of
// the OTEL_EXPORTER_OTLP_* variables itself, and sends no request before the first export.
func startTracing(ctx context.Context) (tracing, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return tracing{}, nil
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return tracing{}, err
	}
	res, err := sdkresource.New(ctx, sdkresource.WithAttributes(attribute.String("service.name", "harness"), attribute.String("service.version", version)),
		sdkresource.WithFromEnv())
	if err != nil {
		return tracing{}, err
	}
	sdk := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	content := strings.EqualFold(os.Getenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"), "true")
	return tracing{provider: sdk, content: content, sdk: sdk}, nil
}

// flush exports the spans that wait, within flushBudget.
func (t tracing) flush() {
	if t.sdk == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), flushBudget)
	defer cancel()
	_ = t.sdk.Shutdown(ctx)
}
