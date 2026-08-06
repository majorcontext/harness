package openaicompat

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestStreamHTTPErrorClassification is the red-first test for GitHub issue
// #61 on the openaicompat family: a 429 or any 5xx must come back from
// Stream marked provider.RetryableError so the goal loop's long backoff
// (engine/goal.go) can apply. Every other status (400s, auth) must stay
// unmarked, so it keeps failing fast exactly as before.
func TestStreamHTTPErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantClass provider.RetryableClass
		wantRetry bool
	}{
		{"rate limit 429", http.StatusTooManyRequests, provider.RetryableRateLimited, true},
		{"internal 500", http.StatusInternalServerError, provider.RetryableServerError, true},
		{"bad gateway 502", http.StatusBadGateway, provider.RetryableServerError, true},
		{"service unavailable 503", http.StatusServiceUnavailable, provider.RetryableServerError, true},
		{"bad request 400", http.StatusBadRequest, "", false},
		{"auth 401", http.StatusUnauthorized, "", false},
		{"not found 404", http.StatusNotFound, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"error":{"message":"boom","type":"some_error","code":"x"}}`) //nolint:errcheck
			})
			_, err := c.Stream(context.Background(), &provider.Request{
				Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
				Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
				MaxTokens: 10,
			})
			if err == nil {
				t.Fatal("err = nil, want an HTTP error")
			}
			class, ok := provider.AsRetryable(err)
			if ok != tc.wantRetry {
				t.Fatalf("AsRetryable(%v) ok = %v, want %v", err, ok, tc.wantRetry)
			}
			if ok && class != tc.wantClass {
				t.Errorf("class = %q, want %q", class, tc.wantClass)
			}
		})
	}
}

// TestStreamTruncationClassification mirrors provider/anthropic's test of
// the same name (see the 2026-08-06 incident described there): a stream cut
// before the [DONE] sentinel — no HTTP error, no error payload, the body
// just ends — must be classified provider.RetryableStreamTruncated, never
// surface as a bare, deterministic-looking io.EOF.
func TestStreamTruncationClassification(t *testing.T) {
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A complete tool call, then the body ends with no finish_reason
		// chunk and no [DONE].
		io.WriteString(w, sseData(`{"id":"chatcmpl_1","model":"some/model","choices":[{"index":0,"delta":{"role":"assistant"}}]}`))                                             //nolint:errcheck
		io.WriteString(w, sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_77","function":{"name":"bash","arguments":"{}"}}]}}]}`)) //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var streamErr error
	for streamErr == nil {
		var ev provider.Event
		ev, streamErr = s.Next()
		if streamErr == nil && ev.Type == provider.EventDone {
			t.Fatal("stream reported EventDone despite the body ending before [DONE]")
		}
	}
	class, ok := provider.AsRetryable(streamErr)
	if !ok || class != provider.RetryableStreamTruncated {
		t.Fatalf("AsRetryable(%v) = %q, %v; want %q, true", streamErr, class, ok, provider.RetryableStreamTruncated)
	}
}

// TestStreamMidChunkTruncationClassification mirrors provider/anthropic's
// TestStreamMidEventTruncationClassification: a data line cut mid-JSON with
// no trailing newline must classify as stream truncation, not surface as a
// raw "bad chunk" parse error the retry tiers treat as deterministic.
func TestStreamMidChunkTruncationClassification(t *testing.T) {
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)) //nolint:errcheck
		io.WriteString(w, `data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"par`)          //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var streamErr error
	for streamErr == nil {
		_, streamErr = s.Next()
	}
	class, ok := provider.AsRetryable(streamErr)
	if !ok || class != provider.RetryableStreamTruncated {
		t.Fatalf("AsRetryable(%v) = %q, %v; want %q, true", streamErr, class, ok, provider.RetryableStreamTruncated)
	}
}

// TestStreamActivityDuringToolArgumentStreaming mirrors provider/anthropic's
// test of the same name: chunks whose only content is tool_call argument
// deltas (buffered until the finish chunk) must surface as EventActivity so
// the engine's idle-stream watchdog sees the wire is alive.
func TestStreamActivityDuringToolArgumentStreaming(t *testing.T) {
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_77","function":{"name":"bash","arguments":""}}]}}]}`)) //nolint:errcheck
		io.WriteString(w, sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"ls\"}"}}]}}]}`))          //nolint:errcheck
		io.WriteString(w, sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))                                                       //nolint:errcheck
		io.WriteString(w, sseDone)                                                                                                                                             //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var activity, sawDone bool
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case provider.EventActivity:
			activity = true
		case provider.EventDone:
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("stream never completed")
	}
	if !activity {
		t.Error("no EventActivity surfaced while tool-call arguments were streaming")
	}
}

// TestStreamCommentHeartbeatCountsAsActivity mirrors provider/anthropic's
// test of the same name: a ": heartbeat" SSE comment line (bifrost's
// keepalive shape, maximhq/bifrost#5010) must surface as EventActivity.
func TestStreamCommentHeartbeatCountsAsActivity(t *testing.T) {
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": heartbeat\n")                                                                                        //nolint:errcheck
		io.WriteString(w, sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)) //nolint:errcheck
		io.WriteString(w, ": heartbeat\n")                                                                                        //nolint:errcheck
		io.WriteString(w, sseDone)                                                                                                //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "openrouter", Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var activity int
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == provider.EventActivity {
			activity++
		}
	}
	if activity < 2 {
		t.Errorf("activity events = %d, want >= 2 (comment heartbeats must count as wire activity)", activity)
	}
}
