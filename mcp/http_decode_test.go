package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fixedJSONResponseServer answers every POST with the given raw JSON body,
// regardless of what was sent, so tests can drive the client straight into
// a specific decodeResult branch.
func fixedJSONResponseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDecodeResult_IDMismatch drives the client's very first request
// (initialize, whose id is always 1) into a server response bearing a
// different id, exercising decodeResult's id-mismatch branch.
func TestDecodeResult_IDMismatch(t *testing.T) {
	srv := fixedJSONResponseServer(t, `{"jsonrpc":"2.0","id":999,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"fake","version":"1"}}}`)

	c, err := NewClient(&HTTPTransport{Endpoint: srv.URL}, Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Initialize(context.Background())
	if err == nil {
		t.Fatal("Initialize returned nil error for a response with a mismatched id")
	}
	if !strings.Contains(err.Error(), "does not match request id") {
		t.Errorf("error %q does not describe an id mismatch", err.Error())
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		t.Errorf("id-mismatch error unexpectedly unwraps to an *RPCError: %v", rpcErr)
	}
}

// TestDecodeResult_ErrorObject drives the client's first request into a
// server response carrying a matching id but a JSON-RPC error object,
// exercising decodeResult's error branch, and asserts the caller gets back
// a typed *RPCError with the fields intact.
func TestDecodeResult_ErrorObject(t *testing.T) {
	srv := fixedJSONResponseServer(t, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params: protocolVersion required"}}`)

	c, err := NewClient(&HTTPTransport{Endpoint: srv.URL}, Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Initialize(context.Background())
	if err == nil {
		t.Fatal("Initialize returned nil error for a JSON-RPC error response")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error %v does not unwrap to *RPCError", err)
	}
	if rpcErr.Code != -32602 {
		t.Errorf("RPCError.Code = %d, want -32602", rpcErr.Code)
	}
	if rpcErr.Message != "invalid params: protocolVersion required" {
		t.Errorf("RPCError.Message = %q", rpcErr.Message)
	}
}
