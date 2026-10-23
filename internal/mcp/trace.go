package mcp

import (
	"context"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// annotateCall adds the MCP attributes of a tools/call request to the span in ctx, which is the span of the tool call.
func annotateCall(ctx context.Context, method string, requestID int64, sessionID string) {
	span := trace.SpanFromContext(ctx)
	if method != methodToolsCall || !span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("mcp.method.name", method), attribute.String("jsonrpc.request.id", strconv.FormatInt(requestID, 10))}
	if sessionID != "" {
		attrs = append(attrs, attribute.String("mcp.session.id", sessionID))
	}
	span.SetAttributes(attrs...)
}
