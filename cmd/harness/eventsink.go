package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/server"
)

const (
	defaultEventSinkTimeout = 30 * time.Second
	eventSinkReplyMaxBytes  = 1 << 16
)

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
	Filtered   bool           `json:"filtered,omitempty"`
	Records    []server.Event `json:"records"`
}

type sinkReply struct {
	AppliedThrough int64 `json:"applied_through"`
}

func (h *httpEventSink) Deliver(ctx context.Context, batch server.EventBatch) (int64, error) {
	// A filtered checkpoint carries no records, and it must still encode
	// "records":[]. A null would make every receiver special-case the one
	// request that exists only to advance its cursor.
	records := batch.Records
	if records == nil {
		records = []server.Event{}
	}
	body, err := json.Marshal(sinkBody{
		Generation: h.generation,
		FromSeq:    batch.FromSeq,
		ToSeq:      batch.ToSeq,
		Filtered:   batch.Filtered,
		Records:    records,
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
		// net/url.Error includes the complete request URL, including query
		// parameters. Log only the underlying transport failure.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return 0, fmt.Errorf("event sink: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, eventSinkReplyMaxBytes+1))
		err := fmt.Errorf("event sink: receiver returned %d", resp.StatusCode)
		if readErr != nil {
			err = fmt.Errorf("event sink: receiver returned %d; read diagnostic: %w", resp.StatusCode, readErr)
		} else if code := eventSinkDiagnosticCode(body); code != "" {
			err = fmt.Errorf("event sink: receiver returned %d (%s)", resp.StatusCode, code)
		}
		// The status classifies the failure, not the diagnostic: a receiver
		// that answers a permanent status with no body is still permanent.
		if eventSinkPermanentStatus(resp.StatusCode) {
			return 0, fmt.Errorf("%w: %w", err, server.ErrEventSinkPermanent)
		}
		return 0, err
	}
	var reply sinkReply
	// A reply that does not parse is an error, not a zero cursor: treating
	// it as 0 would silently command a full re-ship on every malformed
	// response.
	if err := json.NewDecoder(io.LimitReader(resp.Body, eventSinkReplyMaxBytes)).Decode(&reply); err != nil {
		return 0, fmt.Errorf("event sink: decode reply: %w", err)
	}
	return reply.AppliedThrough, nil
}

// eventSinkPermanentStatus reports whether this status rejects the batch
// itself, so that retrying identical bytes cannot succeed. The set is fixed
// and small: a malformed body (400, 422), a refused credential (401, 403), a
// route that holds no receiver (404, 410), and a receiver that says the batch
// contradicts what it already applied (409).
//
// Every other status keeps the retry, including an unlisted 4xx. 408, 425,
// and 429 ask for the same batch later, and a 5xx is a receiver that a
// restart can fix, so treating either as permanent would cost every later
// record for one transient failure.
func eventSinkPermanentStatus(status int) bool {
	switch status {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusGone,
		http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// eventSinkDiagnosticCode extracts only a bounded machine code from an error
// response. Arbitrary receiver text can contain secrets and reaches logs through
// the pump's delivery error, so it must not be copied into the error.
func eventSinkDiagnosticCode(body []byte) string {
	if len(body) > eventSinkReplyMaxBytes {
		return ""
	}
	var diagnostic struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &diagnostic) != nil || diagnostic.Code == "" || len(diagnostic.Code) > 128 {
		return ""
	}
	for _, r := range diagnostic.Code {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return ""
		}
	}
	return diagnostic.Code
}
