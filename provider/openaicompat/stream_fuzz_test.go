package openaicompat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// maxFuzzIterations bounds the Next() drive loop per exec. The decode loop
// must always advance toward EOF or an error; exceeding this cap means it
// isn't — an infinite or non-advancing loop — and that is itself the FAILURE
// the fuzz target exists to catch, not a limit to raise.
const maxFuzzIterations = 100000

// newFuzzStream builds a stream directly over data, reaching the decode
// layer beneath Stream/Next with no HTTP involved at all: stream's body/r/
// model/family fields are unexported but reachable from within the package,
// so this drives the real readDataLine+handle loop with zero per-exec I/O
// overhead. (Preferred per the plan's Task 2: a direct reader-level entry
// exists here, so httptest is unnecessary.)
func newFuzzStream(data []byte) *stream {
	return &stream{
		body:   io.NopCloser(bytes.NewReader(nil)),
		r:      bufio.NewReader(bytes.NewReader(data)),
		model:  message.ModelRef{Provider: "fuzz", Model: "fuzz-model"},
		family: "fuzz",
	}
}

func FuzzStreamDecode(f *testing.F) {
	// Happy path, lifted verbatim from stream_test.go's fixture.
	f.Add([]byte(streamFixture))
	// Truncated mid-stream: cuts the happy path off partway through a chunk.
	f.Add([]byte(streamFixture[:len(streamFixture)/2]))
	// Interleaved/malformed: partial content immediately followed by a
	// mid-stream error chunk (no [DONE]), mirroring
	// TestStreamMidStreamErrorClassifiedRetryable.
	f.Add([]byte(sseData(`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"partial"}}]}`) +
		sseData(`{"error":{"message":"upstream overloaded, please retry","code":529}}`)))
	// Empty input.
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			t.Skip()
		}
		s := newFuzzStream(data)
		for i := 0; ; i++ {
			if i >= maxFuzzIterations {
				t.Fatalf("Next() did not terminate within %d iterations; input = %q", maxFuzzIterations, data)
			}
			ev, err := s.Next()
			if err != nil {
				return // EOF or a decode/protocol error: both are valid terminal outcomes.
			}
			checkFuzzEvent(t, ev)
		}
	})
}

// checkFuzzEvent asserts that any event a successful Next() call hands back
// is protocol-plausible: a known type, with the fields its type requires.
func checkFuzzEvent(t *testing.T, ev provider.Event) {
	t.Helper()
	switch ev.Type {
	case provider.EventTextDelta, provider.EventReasoningDelta:
		// Text is arbitrary provider-controlled content; nothing to assert.
	case provider.EventToolCall:
		if ev.ToolCall == nil {
			t.Fatalf("tool_call event with nil ToolCall: %+v", ev)
		}
	case provider.EventDone:
		checkFuzzDoneMessage(t, ev.Message)
	default:
		t.Fatalf("unknown event type %q", ev.Type)
	}
}

// checkFuzzDoneMessage asserts a Done event's message is well-formed per the
// message package's own invariants. Normalize is the package's documented
// ingest-time well-formedness gate (see Message.Normalize's doc comment):
// every message the engine accepts passes through it before it is safe to
// marshal. Requiring a clean Marshal after Normalize is exactly the
// guarantee the message package promises for any message a provider adapter
// can produce, not an invented deeper semantic check.
func checkFuzzDoneMessage(t *testing.T, msg *message.Message) {
	t.Helper()
	if msg == nil {
		t.Fatal("done event with nil Message")
	}
	if msg.Role != message.RoleAssistant {
		t.Fatalf("done message role = %q, want %q", msg.Role, message.RoleAssistant)
	}
	msg.Normalize()
	if _, err := json.Marshal(msg); err != nil {
		t.Fatalf("done message failed to marshal after Normalize: %v (message %+v)", err, msg)
	}
}
