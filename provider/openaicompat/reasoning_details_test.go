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
			e := ev
			done = &e
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

// TestStreamReasoningDetailsWithReasoningContentSameText pins the kimi-k3
// live shape: a chunk that echoes the same reasoning text in BOTH
// reasoning_content and reasoning_details must emit it once. The old
// additive behavior doubled every character on that route.
func TestStreamReasoningDetailsWithReasoningContentSameText(t *testing.T) {
	c := testClient(t, "bifrost", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseData(`{"id":"chunk_1","choices":[{"index":0,"delta":{"reasoning_content":"You're right ","reasoning_details":[{"text":"You're right "}]}}]}`))
		_, _ = io.WriteString(w, sseData(`{"id":"chunk_2","choices":[{"index":0,"delta":{"content":"final"}}]}`))
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
			e := ev
			done = &e
		}
	}

	if len(reasoningDeltas) != 1 || reasoningDeltas[0] != "You're right " {
		t.Fatalf("reasoning deltas = %v, want the text once", reasoningDeltas)
	}
	if done == nil {
		t.Fatal("missing EventDone")
	}
	rp, ok := done.Message.Parts[0].(*message.Reasoning)
	if !ok || rp.Text != "You're right " {
		t.Fatalf("part 0 = %+v, want Reasoning with the text once", done.Message.Parts[0])
	}
}

// A reasoning_details entry with no text (metadata/encrypted) must not
// suppress the alias text on the same chunk.
func TestStreamReasoningDetailsEmptyDoesNotSuppressAlias(t *testing.T) {
	c := testClient(t, "bifrost", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseData(`{"id":"chunk_1","choices":[{"index":0,"delta":{"reasoning_content":"alias text","reasoning_details":[{"text":""}]}}]}`))
		_, _ = io.WriteString(w, sseData(`[DONE]`))
	})
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-2.5-flash"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}}},
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got []string
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == provider.EventReasoningDelta {
			got = append(got, ev.Text)
		}
	}
	if len(got) != 1 || got[0] != "alias text" {
		t.Fatalf("reasoning deltas = %v, want ['alias text'] once", got)
	}
}

// Distinct texts in both encodings on one chunk stay ADDITIVE; only a
// verbatim repeat is deduped.
func TestStreamReasoningDetailsDistinctTextsAreAdditive(t *testing.T) {
	c := testClient(t, "bifrost", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseData(`{"id":"chunk_1","choices":[{"index":0,"delta":{"reasoning_content":"step 1: ","reasoning_details":[{"text":"step 2"}]}}]}`))
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
			e := ev
			done = &e
		}
	}
	if len(reasoningDeltas) != 2 || (reasoningDeltas[0] != "step 2" && reasoningDeltas[1] != "step 1: ") {
		t.Fatalf("reasoning deltas = %v, want both distinct texts", reasoningDeltas)
	}
	rp, ok := done.Message.Parts[0].(*message.Reasoning)
	if !ok || rp.Text != "step 2step 1: " {
		t.Fatalf("part 0 = %+v, want Reasoning with both texts", done.Message.Parts[0])
	}
}
