package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestMarkRetryableNilIsNil(t *testing.T) {
	if err := MarkRetryable(nil, RetryableOverloaded); err != nil {
		t.Fatalf("MarkRetryable(nil, ...) = %v, want nil", err)
	}
}

func TestAsRetryableRoundTrip(t *testing.T) {
	base := errors.New("anthropic: Overloaded (overloaded_error, HTTP 529)")
	wrapped := MarkRetryable(base, RetryableOverloaded)

	class, ok := AsRetryable(wrapped)
	if !ok || class != RetryableOverloaded {
		t.Fatalf("AsRetryable(wrapped) = %q, %v; want %q, true", class, ok, RetryableOverloaded)
	}
	if !errors.Is(wrapped, base) {
		t.Errorf("errors.Is(wrapped, base) = false, want true (Unwrap must expose the original error)")
	}
}

func TestAsRetryableFalseForOrdinaryError(t *testing.T) {
	base := errors.New("anthropic: invalid request (invalid_request_error, HTTP 400)")
	if class, ok := AsRetryable(base); ok {
		t.Fatalf("AsRetryable(ordinary error) = %q, true; want false", class)
	}
}

// TestAsRetryableThroughWrapping proves the classification survives being
// wrapped by an unrelated error type via fmt.Errorf's %w — the same shape
// engine's interruptedTurnError wraps a stream error in.
func TestAsRetryableThroughWrapping(t *testing.T) {
	base := errors.New("connection reset")
	retryable := MarkRetryable(base, RetryableServerError)
	outer := fmt.Errorf("engine: turn interrupted: %w", retryable)

	class, ok := AsRetryable(outer)
	if !ok || class != RetryableServerError {
		t.Fatalf("AsRetryable(outer) = %q, %v; want %q, true", class, ok, RetryableServerError)
	}
}

func TestRetryableErrorMessageNamesClass(t *testing.T) {
	err := MarkRetryable(errors.New("Overloaded"), RetryableOverloaded)
	const want = "[retryable:overloaded] Overloaded"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
