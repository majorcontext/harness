package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestContextOverflowClassifiedStructurally covers the OpenAI-style 400
// shape, which — unlike Anthropic's — carries a distinct structural signal:
// error.code == "context_length_exceeded". The adapter must prefer this
// structural check over any message matching (see provider.Error's doc
// comment: classify structurally where the API allows).
func TestContextOverflowClassifiedStructurally(t *testing.T) {
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		body, _ := json.Marshal(map[string]any{
			"error": map[string]string{
				"message": "This model's maximum context length is 8192 tokens. However, your messages resulted in 10191 tokens. Please reduce the length of the messages.",
				"type":    "invalid_request_error",
				"code":    "context_length_exceeded",
			},
		})
		w.Write(body) //nolint:errcheck
	})

	_, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: "openrouter", Model: "m1"},
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
	if pe.PromptTokens != 10191 || pe.TokenLimit != 8192 {
		t.Errorf("PromptTokens/TokenLimit = %d/%d, want 10191/8192", pe.PromptTokens, pe.TokenLimit)
	}
	wantMsg := "context exhausted: prompt 10191 tokens > limit 8192"
	if err.Error() != wantMsg {
		t.Errorf("err.Error() = %q, want %q", err.Error(), wantMsg)
	}
}

// TestContextOverflowClassifiedWithoutCode covers a compat deployment (some
// self-hosted vLLM/Ollama-style backend) that returns the same message shape
// but omits the "code" field entirely — the adapter's message-matching
// fallback (tolerated here, never in the engine) must still classify it.
func TestContextOverflowClassifiedWithoutCode(t *testing.T) {
	c := testClient(t, "vllm", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		body, _ := json.Marshal(map[string]any{
			"error": map[string]string{
				"message": "This model's maximum context length is 4096 tokens. However, your messages resulted in 5000 tokens.",
				"type":    "invalid_request_error",
			},
		})
		w.Write(body) //nolint:errcheck
	})

	_, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: "vllm", Model: "m1"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	})
	if !provider.IsContextOverflow(err) {
		t.Fatalf("err = %v, want IsContextOverflow", err)
	}
	var pe *provider.Error
	errors.As(err, &pe)
	if pe.PromptTokens != 5000 || pe.TokenLimit != 4096 {
		t.Errorf("PromptTokens/TokenLimit = %d/%d, want 5000/4096", pe.PromptTokens, pe.TokenLimit)
	}
}

// TestOrdinaryBadRequestNotClassified guards against over-classification: an
// invalid_request_error unrelated to context length must not be misread as
// one.
func TestOrdinaryBadRequestNotClassified(t *testing.T) {
	c := testClient(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		body, _ := json.Marshal(map[string]any{
			"error": map[string]string{
				"message": "'messages' is a required property",
				"type":    "invalid_request_error",
				"code":    "invalid_request_error",
			},
		})
		w.Write(body) //nolint:errcheck
	})

	_, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: "openrouter", Model: "m1"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("Stream succeeded, want an error")
	}
	if provider.IsContextOverflow(err) {
		t.Errorf("err = %v, misclassified as context overflow", err)
	}
}
