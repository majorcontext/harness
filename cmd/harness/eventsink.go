package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/server"
)

const defaultEventSinkTimeout = 30 * time.Second

// httpEventSink is server.EventSink over HTTP. It lives here rather than in
// server/ so that package holds no outbound HTTP client and the pump can be
// tested against a fake.
type httpEventSink struct {
	url        string
	headers    map[string]string
	generation string
	client     *http.Client
}

func newHTTPEventSink(spec *config.EventSinkSpec) *httpEventSink {
	timeout := defaultEventSinkTimeout
	if spec.TimeoutS > 0 {
		timeout = time.Duration(spec.TimeoutS) * time.Second
	}
	return &httpEventSink{
		url:        spec.URL,
		headers:    spec.Headers,
		generation: spec.Generation,
		client:     &http.Client{Timeout: timeout},
	}
}

// sinkBody is the wire shape. It adds `generation` to what the server's own
// EventBatch carries: the server treats the generation as opaque and has no
// reason to hold it.
type sinkBody struct {
	Generation string         `json:"generation,omitempty"`
	FromSeq    int64          `json:"from_seq"`
	ToSeq      int64          `json:"to_seq"`
	Records    []server.Event `json:"records"`
}

type sinkReply struct {
	AppliedThrough int64 `json:"applied_through"`
}

func (h *httpEventSink) Deliver(ctx context.Context, batch server.EventBatch) (int64, error) {
	body, err := json.Marshal(sinkBody{
		Generation: h.generation,
		FromSeq:    batch.FromSeq,
		ToSeq:      batch.ToSeq,
		Records:    batch.Records,
	})
	if err != nil {
		return 0, fmt.Errorf("event sink: marshal batch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("event sink: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("event sink: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("event sink: %s returned %d", h.url, resp.StatusCode)
	}
	var reply sinkReply
	// A reply that does not parse is an error, not a zero cursor: treating
	// it as 0 would silently command a full re-ship on every malformed
	// response.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&reply); err != nil {
		return 0, fmt.Errorf("event sink: decode reply: %w", err)
	}
	return reply.AppliedThrough, nil
}
