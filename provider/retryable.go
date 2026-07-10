package provider

import "errors"

// RetryableClass names why an adapter considers an error transient provider
// weather — worth an automatic retry — rather than a deterministic failure
// that will never succeed no matter how many times it is retried.
type RetryableClass string

const (
	// RetryableOverloaded marks a provider-reported capacity/overload
	// condition (Anthropic's HTTP 529 / "overloaded_error").
	RetryableOverloaded RetryableClass = "overloaded"
	// RetryableRateLimited marks an HTTP 429.
	RetryableRateLimited RetryableClass = "rate_limited"
	// RetryableServerError marks a generic provider-side 5xx (or an
	// Anthropic inline "api_error" stream event, which is the same failure
	// mode delivered mid-stream instead of as an HTTP status).
	RetryableServerError RetryableClass = "server_error"
)

// RetryableError marks an adapter error as retryable provider weather (an
// overload, a rate limit, a 5xx) as opposed to a deterministic failure (a
// bad request, an auth failure) that will fail identically on every retry.
// It wraps the original error (Unwrap) and never replaces it — every
// existing caller of err.Error() still sees the original message, just
// prefixed with the class so it is visible without decoding anything (see
// Error below).
//
// The engine never string-matches provider error text to decide whether to
// retry: adapters construct RetryableError explicitly (see MarkRetryable)
// at the one place they have the HTTP status code or wire error type in
// hand, and callers recover it with errors.As (see AsRetryable) — the
// classification travels as a typed value through any number of wrapping
// layers (e.g. engine's interruptedTurnError) exactly the way any other
// wrapped error does.
type RetryableError struct {
	Err   error
	Class RetryableClass
}

// Error prefixes the wrapped error's message with the retryable class, so
// any consumer that only ever calls Error() (a journaled goal.stalled
// reason, a turn.end error, a session.error message) still surfaces the
// classification without needing to unwrap anything — this is what makes
// "last_turn/error names the retryable class" true everywhere for free.
func (e *RetryableError) Error() string {
	return "[retryable:" + string(e.Class) + "] " + e.Err.Error()
}

// Unwrap exposes the original error to errors.Is/errors.As.
func (e *RetryableError) Unwrap() error { return e.Err }

// MarkRetryable wraps err as a RetryableError of the given class, or
// returns nil unchanged if err is nil (mirrors fmt.Errorf's %w nil
// handling convention, so adapters can call it unconditionally).
func MarkRetryable(err error, class RetryableClass) error {
	if err == nil {
		return nil
	}
	return &RetryableError{Err: err, Class: class}
}

// AsRetryable reports whether err (or any error it wraps, per errors.As)
// was marked retryable by an adapter, returning the class it was marked
// with. This is the ONLY sanctioned way for the engine to decide whether a
// provider error is worth a long backoff — never string-matching.
func AsRetryable(err error) (RetryableClass, bool) {
	var re *RetryableError
	if errors.As(err, &re) {
		return re.Class, true
	}
	return "", false
}
