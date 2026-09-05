package provider

import (
	"context"
	"errors"
	"fmt"
)

// RetryableClass identifies a transient provider error.
type RetryableClass string

const (
	// RetryableOverloaded marks a provider capacity error.
	RetryableOverloaded RetryableClass = "overloaded"
	// RetryableRateLimited marks an HTTP 429.
	RetryableRateLimited RetryableClass = "rate_limited"
	// RetryableServerError marks a provider 5xx error.
	RetryableServerError RetryableClass = "server_error"
	// RetryableStreamTruncated marks a stream that ends before its terminal event.
	RetryableStreamTruncated RetryableClass = "stream_truncated"
)

// RetryableError wraps a transient provider error and its class.
type RetryableError struct {
	Err   error
	Class RetryableClass
}

// Error prefixes the wrapped error message with its class.
func (e *RetryableError) Error() string {
	return "[retryable:" + string(e.Class) + "] " + e.Err.Error()
}

// Unwrap exposes the original error to errors.Is/errors.As.
func (e *RetryableError) Unwrap() error { return e.Err }

// MarkRetryable wraps err with class. It returns nil when err is nil.
func MarkRetryable(err error, class RetryableClass) error {
	if err == nil {
		return nil
	}
	return &RetryableError{Err: err, Class: class}
}

// MarkStreamTruncated wraps a stream error that occurs before completion.
// It preserves nil, cancellation, and deadline errors.
func MarkStreamTruncated(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &RetryableError{
		Err:   fmt.Errorf("provider stream ended before completion: %w", err),
		Class: RetryableStreamTruncated,
	}
}

// AsRetryable reports whether err wraps a retryable provider error.
func AsRetryable(err error) (RetryableClass, bool) {
	var re *RetryableError
	if errors.As(err, &re) {
		return re.Class, true
	}
	return "", false
}

// PermanentError wraps an error that a retry cannot resolve.
type PermanentError struct {
	Err error
}

// Error prefixes the wrapped error message with "[permanent]".
func (e *PermanentError) Error() string {
	return "[permanent] " + e.Err.Error()
}

// Unwrap exposes the original error to errors.Is/errors.As.
func (e *PermanentError) Unwrap() error { return e.Err }

// MarkPermanent wraps err. It returns nil when err is nil.
func MarkPermanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// AsPermanent reports whether err (or any error it wraps, per errors.As) was
// marked permanent by an adapter. Mirrors AsRetryable's shape exactly; this
// is the ONLY sanctioned way for the engine to fail fast on a
// deterministically-unrecoverable provider error — never string-matching.
// A PermanentError and a RetryableError are mutually exclusive: an adapter
// never marks the same error both ways (see classifyStatus/apiError in
// provider/anthropic), so AsRetryable and AsPermanent never both report true
// for the same error.
func AsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}
