package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestMarkStreamTruncatedClassifies(t *testing.T) {
	base := errors.New("EOF")
	wrapped := MarkStreamTruncated(base)

	class, ok := AsRetryable(wrapped)
	if !ok || class != RetryableStreamTruncated {
		t.Fatalf("AsRetryable(wrapped) = %q, %v; want %q, true", class, ok, RetryableStreamTruncated)
	}
	if !errors.Is(wrapped, base) {
		t.Errorf("errors.Is(wrapped, base) = false, want true (Unwrap must expose the original error)")
	}
	// The journaled reason must name the failure, not just re-surface a
	// cryptic "EOF" — the 2026-08-06 incident's goal.stalled records read
	// bare "EOF" and cost hours of diagnosis.
	if got := wrapped.Error(); !strings.Contains(got, "stream ended before completion") {
		t.Errorf("Error() = %q, want it to contain %q", got, "stream ended before completion")
	}
}

// TestMarkStreamTruncatedContextPassthrough: a context cancellation or
// deadline surfacing through the stream read is a deliberate abort (or the
// caller's own budget), never provider weather — it must pass through
// unwrapped so errors.Is(err, context.Canceled) keeps short-circuiting
// retry loops before any classification check.
func TestMarkStreamTruncatedContextPassthrough(t *testing.T) {
	for _, base := range []error{context.Canceled, context.DeadlineExceeded} {
		wrapped := fmt.Errorf("read tcp: %w", base)
		got := MarkStreamTruncated(wrapped)
		if got != wrapped { //nolint:errorlint // identity check is the point
			t.Errorf("MarkStreamTruncated(%v) = %v, want the error returned unchanged", base, got)
		}
		if _, ok := AsRetryable(got); ok {
			t.Errorf("AsRetryable(MarkStreamTruncated(%v)) = true, want false", base)
		}
	}
}

func TestMarkStreamTruncatedNilIsNil(t *testing.T) {
	if err := MarkStreamTruncated(nil); err != nil {
		t.Fatalf("MarkStreamTruncated(nil) = %v, want nil", err)
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

// TestMarkPermanentNilIsNil mirrors MarkRetryable's nil-passthrough
// convention (see MarkPermanent's doc comment), so an adapter can call it
// unconditionally.
func TestMarkPermanentNilIsNil(t *testing.T) {
	if err := MarkPermanent(nil); err != nil {
		t.Fatalf("MarkPermanent(nil) = %v, want nil", err)
	}
}

// TestAsPermanentRoundTrip mirrors TestAsRetryableRoundTrip: the
// classification round-trips through AsPermanent, and Unwrap still exposes
// the original error to errors.Is.
func TestAsPermanentRoundTrip(t *testing.T) {
	base := errors.New(`anthropic: messages.5: tool_use ids were found without tool_result blocks immediately after (invalid_request_error, HTTP 400)`)
	wrapped := MarkPermanent(base)

	if !AsPermanent(wrapped) {
		t.Fatalf("AsPermanent(wrapped) = false, want true")
	}
	if !errors.Is(wrapped, base) {
		t.Errorf("errors.Is(wrapped, base) = false, want true (Unwrap must expose the original error)")
	}
}

// TestAsPermanentFalseForOrdinaryError guards the negative case: a plain,
// unmarked error is never mistaken for a permanent classification.
func TestAsPermanentFalseForOrdinaryError(t *testing.T) {
	base := errors.New("anthropic: invalid request (invalid_request_error, HTTP 400)")
	if AsPermanent(base) {
		t.Fatalf("AsPermanent(ordinary error) = true, want false")
	}
}

// TestAsPermanentThroughWrapping mirrors TestAsRetryableThroughWrapping: the
// classification survives being wrapped by an unrelated error type via
// fmt.Errorf's %w, the same shape engine's interruptedTurnError wraps a
// stream error in.
func TestAsPermanentThroughWrapping(t *testing.T) {
	base := errors.New("bad request")
	permanent := MarkPermanent(base)
	outer := fmt.Errorf("engine: turn interrupted: %w", permanent)

	if !AsPermanent(outer) {
		t.Fatalf("AsPermanent(outer) = false, want true")
	}
}

// TestPermanentErrorMessageFormat pins the fixed "[permanent] " prefix
// PermanentError.Error renders. PermanentError deliberately carries no
// class enum (see its own doc comment) — unlike RetryableError, there is
// only one undifferentiated "do not retry this" bucket — so this checks a
// fixed prefix, not a named class.
func TestPermanentErrorMessageFormat(t *testing.T) {
	err := MarkPermanent(errors.New("bad request"))
	const want = "[permanent] bad request"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestPermanentAndRetryableAreMutuallyExclusive guards the structural
// boundary between the two classifications: an error marked permanent must
// never also report as retryable, and vice versa, since promptTurnWithRetry
// (engine/goal.go) chooses mutually exclusive branches — a fail-fast, single
// attempt for permanent versus a bounded, backed-off retry loop for
// retryable — off exactly this pair of predicates.
func TestPermanentAndRetryableAreMutuallyExclusive(t *testing.T) {
	permanent := MarkPermanent(errors.New("bad request"))
	if _, ok := AsRetryable(permanent); ok {
		t.Errorf("AsRetryable(MarkPermanent(...)) = true, want false")
	}
	retryable := MarkRetryable(errors.New("overloaded"), RetryableOverloaded)
	if AsPermanent(retryable) {
		t.Errorf("AsPermanent(MarkRetryable(...)) = true, want false")
	}
}
