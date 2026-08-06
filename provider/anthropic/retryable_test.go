package anthropic

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestStreamHTTPErrorClassification is the red-first test for GitHub issue
// #61: an Anthropic HTTP 529 (overloaded_error), 429 (rate limit), or any
// 5xx must come back from Stream marked provider.RetryableError so the goal
// loop's long backoff (engine/goal.go) can apply — never by the engine
// string-matching "overloaded_error" out of the error text. Every other
// status (400s, auth) must stay unmarked, so it keeps failing fast exactly
// as before.
func TestStreamHTTPErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		errType   string
		wantClass provider.RetryableClass
		wantRetry bool
	}{
		{"overloaded 529", 529, "overloaded_error", provider.RetryableOverloaded, true},
		{"rate limit 429", http.StatusTooManyRequests, "rate_limit_error", provider.RetryableRateLimited, true},
		{"internal 500", http.StatusInternalServerError, "api_error", provider.RetryableServerError, true},
		{"bad gateway 502", http.StatusBadGateway, "api_error", provider.RetryableServerError, true},
		{"bad request 400", http.StatusBadRequest, "invalid_request_error", "", false},
		{"auth 401", http.StatusUnauthorized, "authentication_error", "", false},
		{"not found 404", http.StatusNotFound, "not_found_error", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"type":"error","error":{"type":"`+tc.errType+`","message":"boom"}}`) //nolint:errcheck
			})
			_, err := c.Stream(context.Background(), &provider.Request{
				Model:     message.ModelRef{Provider: Family, Model: "m"},
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

// TestStreamInlineErrorClassification covers Anthropic's mid-stream "error"
// SSE event (no HTTP status to key off of — only the wire error "type"),
// which is exactly the shape the GitHub issue #61 incidents hit ("engine:
// goal loop stalled: anthropic: Overloaded (overloaded_error)").
func TestStreamInlineErrorClassification(t *testing.T) {
	cases := []struct {
		errType   string
		wantClass provider.RetryableClass
		wantRetry bool
	}{
		{"overloaded_error", provider.RetryableOverloaded, true},
		{"rate_limit_error", provider.RetryableRateLimited, true},
		{"api_error", provider.RetryableServerError, true},
		{"invalid_request_error", "", false},
		{"authentication_error", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.errType, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse("message_start", `{"type":"message_start","message":{"id":"msg_02","usage":{"input_tokens":1}}}`)) //nolint:errcheck
				io.WriteString(w, sse("error", `{"type":"error","error":{"type":"`+tc.errType+`","message":"boom"}}`))                   //nolint:errcheck
			})
			s, err := c.Stream(context.Background(), &provider.Request{
				Model:     message.ModelRef{Provider: Family, Model: "m"},
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
				if streamErr == io.EOF {
					t.Fatal("stream ended without an error")
				}
			}
			class, ok := provider.AsRetryable(streamErr)
			if ok != tc.wantRetry {
				t.Fatalf("AsRetryable(%v) ok = %v, want %v", streamErr, ok, tc.wantRetry)
			}
			if ok && class != tc.wantClass {
				t.Errorf("class = %q, want %q", class, tc.wantClass)
			}
		})
	}
}

// TestStreamTruncationClassification is the red-first test for the
// 2026-08-06 nimble-pizza incident: a stream cut mid-turn — the connection
// dying before message_stop, with no HTTP error status and no inline error
// event (the gateway's ~111s ceiling returned HTTP 200 and then severed the
// body) — surfaced as a bare io.EOF, which the goal loop classified
// deterministic and parked after ~5s of retries. A dead stream is transient
// provider weather: it must come back classified
// provider.RetryableStreamTruncated.
func TestStreamTruncationClassification(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Everything up to and including a COMPLETE tool_use block, then
		// the body ends with no message_stop — the incident fingerprint
		// (EOF exactly as a write_file call finished streaming).
		io.WriteString(w, sse("message_start", `{"type":"message_start","message":{"id":"msg_01","usage":{"input_tokens":100}}}`))                                     //nolint:errcheck
		io.WriteString(w, sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_77","name":"bash","input":{}}}`)) //nolint:errcheck
		io.WriteString(w, sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`))    //nolint:errcheck
		io.WriteString(w, sse("content_block_stop", `{"type":"content_block_stop","index":0}`))                                                                                //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
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
			t.Fatal("stream reported EventDone despite missing message_stop")
		}
	}
	class, ok := provider.AsRetryable(streamErr)
	if !ok || class != provider.RetryableStreamTruncated {
		t.Fatalf("AsRetryable(%v) = %q, %v; want %q, true", streamErr, class, ok, provider.RetryableStreamTruncated)
	}
}

// TestStreamMidEventTruncationClassification: the cut can land ANYWHERE,
// including mid-line — TCP fragmentation makes the boundary a coin flip.
// readSSE used to hand an unterminated trailing event up as if complete;
// handle's JSON parse of the fragment then failed with a raw, deterministic
// -looking error that dodged the truncation classification entirely. Per
// the SSE spec an event whose terminator never arrived is discarded — and
// here that means it surfaces as the same classified truncation a
// clean-boundary cut does.
func TestStreamMidEventTruncationClassification(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse("message_start", `{"type":"message_start","message":{"id":"msg_01","usage":{"input_tokens":100}}}`)) //nolint:errcheck
		// Cut mid-JSON, no trailing newline, no blank-line terminator.
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_bl") //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
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

// TestStreamActivityDuringToolArgumentStreaming: wire events that queue no
// content event — pings, input_json_delta while a tool call's arguments
// stream, message_start — used to be swallowed inside Next's internal loop,
// so the engine saw NOTHING between one content event and the next. The
// engine's idle-stream watchdog kicks once per Next return; a large
// write_file argument block streams for minutes with zero content events,
// so a healthy request got cut at the idle timeout. Every handled wire
// event must now surface — as its content event when it has one, as
// EventActivity when it does not.
func TestStreamActivityDuringToolArgumentStreaming(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse("message_start", `{"type":"message_start","message":{"id":"msg_01","usage":{"input_tokens":100}}}`))                                             //nolint:errcheck
		io.WriteString(w, sse("ping", `{"type":"ping"}`))                                                                                                                      //nolint:errcheck
		io.WriteString(w, sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_77","name":"bash","input":{}}}`)) //nolint:errcheck
		io.WriteString(w, sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}`))                 //nolint:errcheck
		io.WriteString(w, sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"and\":\"ls\"}"}}`))           //nolint:errcheck
		io.WriteString(w, sse("content_block_stop", `{"type":"content_block_stop","index":0}`))                                                                                //nolint:errcheck
		io.WriteString(w, sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`))                                    //nolint:errcheck
		io.WriteString(w, sse("message_stop", `{"type":"message_stop"}`))                                                                                                      //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var activityBeforeToolCall, sawToolCall bool
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
			if !sawToolCall {
				activityBeforeToolCall = true
			}
		case provider.EventToolCall:
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Fatal("tool call never surfaced")
	}
	if !activityBeforeToolCall {
		t.Error("no EventActivity surfaced while the tool call's arguments were streaming — the idle watchdog is blind for the whole block")
	}
}

// TestStreamCommentHeartbeatCountsAsActivity: SSE comment lines are the
// keepalive shape gateways actually send (bifrost's SendHeartbeat emits
// ": heartbeat\n" every second on idle streams — maximhq/bifrost#5010,
// added because intermediaries like Cloudflare cut silent connections).
// readSSE used to skip comments silently inside its read loop, so a
// heartbeat kept every OTHER timer on the path alive while this client's
// own idle watchdog stayed blind and cut the healthy stream. A comment
// line must surface as EventActivity.
func TestStreamCommentHeartbeatCountsAsActivity(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse("message_start", `{"type":"message_start","message":{"id":"msg_01","usage":{"input_tokens":1}}}`)) //nolint:errcheck
		io.WriteString(w, ": heartbeat\n")                                                                                       //nolint:errcheck
		io.WriteString(w, ": heartbeat\n")                                                                                       //nolint:errcheck
		io.WriteString(w, sse("message_stop", `{"type":"message_stop"}`))                                                        //nolint:errcheck
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
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
	// message_start contributes one activity event (it queues no content);
	// the two heartbeat comments must contribute two more.
	if activity < 3 {
		t.Errorf("activity events = %d, want >= 3 (comment heartbeats must count as wire activity)", activity)
	}
}
