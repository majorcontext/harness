package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestPrompt_NoAdapterForProvider covers the case where Config.Model names a
// provider family absent from Config.Providers: streamTurn's
// provider.Registry.For call fails, and Prompt must surface that error
// cleanly — no panic, no history mutation beyond the user's own message, and
// a session.error event for subscribers.
func TestPrompt_NoAdapterForProvider(t *testing.T) {
	var events []Event
	s := NewSession(Config{
		// Providers deliberately does not contain "missing".
		Providers: provider.Registry{"other": &scriptedProvider{name: "other"}},
		Model:     message.ModelRef{Provider: "missing", Model: "m1"},
		OnEvent:   func(ev Event) { events = append(events, ev) },
	})

	final, err := s.Prompt(context.Background(), "hello")
	if err == nil {
		t.Fatal("Prompt returned nil error for a model with no registered provider adapter")
	}
	if final != nil {
		t.Fatalf("Prompt returned a non-nil message alongside an error: %+v", final)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q does not name the unregistered provider", err.Error())
	}

	// The user message is still recorded (Prompt appends it before calling
	// the provider), but no assistant turn was produced.
	h := s.History()
	if len(h) != 1 || h[0].Role != message.RoleUser {
		t.Fatalf("history = %+v, want exactly the user message", h)
	}

	// No EventMessage (assistant turn) event was ever emitted — the
	// provider was never called successfully.
	for _, ev := range events {
		if ev.Type == EventMessage {
			t.Errorf("unexpected message event after a no-adapter failure: %+v", ev)
		}
	}
}
