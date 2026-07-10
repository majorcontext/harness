package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestContextOverflowClassified reproduces the production incident from
// issue #62 verbatim: Anthropic rejects an oversized prompt with a plain
// HTTP 400 invalid_request_error whose message names the token limit
// ("prompt is too long: 205102 tokens > 200000 maximum") — there is no
// distinct error code for this on the wire, so the adapter must recognize
// it by message shape (tolerated here, inside the adapter, never in the
// engine) and return a classified *provider.Error the engine can act on
// without string-matching.
func TestContextOverflowClassified(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		body, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": "prompt is too long: 205102 tokens > 200000 maximum",
			},
		})
		w.Write(body) //nolint:errcheck
	})

	_, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: Family, Model: "claude-fable-5"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("Stream succeeded, want a context-overflow error")
	}
	if !provider.IsContextOverflow(err) {
		t.Fatalf("err = %v, want IsContextOverflow", err)
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, does not unwrap to *provider.Error", err)
	}
	if pe.PromptTokens != 205102 || pe.TokenLimit != 200000 {
		t.Errorf("PromptTokens/TokenLimit = %d/%d, want 205102/200000", pe.PromptTokens, pe.TokenLimit)
	}
	wantMsg := "context exhausted: prompt 205102 tokens > limit 200000"
	if err.Error() != wantMsg {
		t.Errorf("err.Error() = %q, want %q", err.Error(), wantMsg)
	}
}

// TestOrdinaryInvalidRequestNotClassified guards the structural boundary:
// an invalid_request_error whose message does NOT name a token limit (an
// unrelated bad request — a malformed tool schema, say) must NOT be
// misclassified as a context overflow. The classifier tolerates matching
// this one specific message shape; it must not become a blanket "any 400
// from Anthropic is context overflow" rule.
func TestOrdinaryInvalidRequestNotClassified(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		body, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": "messages: at least one message is required",
			},
		})
		w.Write(body) //nolint:errcheck
	})

	_, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: Family, Model: "claude-fable-5"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("Stream succeeded, want an error")
	}
	if provider.IsContextOverflow(err) {
		t.Errorf("err = %v, misclassified as context overflow", err)
	}
}

// TestOverloadedErrorNotClassifiedAsContextOverflow guards the other
// direction: a different invalid_request_error TYPE (e.g. overloaded_error,
// the transient case issue #61 covers) must never be classified as
// ErrKindContextOverflow.
func TestOverloadedErrorNotClassifiedAsContextOverflow(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		body, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "overloaded_error",
				"message": "Overloaded",
			},
		})
		w.Write(body) //nolint:errcheck
	})

	_, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: Family, Model: "claude-fable-5"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("Stream succeeded, want an error")
	}
	if provider.IsContextOverflow(err) {
		t.Errorf("err = %v, misclassified as context overflow", err)
	}
}
