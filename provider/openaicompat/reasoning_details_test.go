package openaicompat

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func TestStreamReasoningDetailsExtracted(t *testing.T) {
	c := testClient(t, "bifrost", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseData(`{"id":"chunk_1","choices":[{"index":0,"delta":{"reasoning_details":[{"text":"analyzing problem"}]}}]}`))
		_, _ = io.WriteString(w, sseData(`{"id":"chunk_2","choices":[{"index":0,"delta":{"content":"solution is 42"}}]}`))
		_, _ = io.WriteString(w, sseData(`[DONE]`))
	})

	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-2.5-flash"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "calculate"}}}},
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var reasoningDeltas []string
	var done *provider.Event
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == provider.EventReasoningDelta {
			reasoningDeltas = append(reasoningDeltas, ev.Text)
		} else if ev.Type == provider.EventDone {
			done = &ev
		}
	}

	if len(reasoningDeltas) != 1 || reasoningDeltas[0] != "analyzing problem" {
		t.Fatalf("reasoning deltas = %v, want ['analyzing problem']", reasoningDeltas)
	}
	if done == nil {
		t.Fatal("missing EventDone")
	}
	if len(done.Message.Parts) != 2 {
		t.Fatalf("message parts = %+v, want 2 parts", done.Message.Parts)
	}
	rp, ok := done.Message.Parts[0].(*message.Reasoning)
	if !ok || rp.Text != "analyzing problem" {
		t.Fatalf("part 0 = %+v, want Reasoning with 'analyzing problem'", done.Message.Parts[0])
	}
}
