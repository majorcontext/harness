package provider

import (
	"context"
	"errors"
	"fmt"
)

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
	// RetryableStreamTruncated marks a response stream that died before
	// its terminal event (Anthropic message_stop, OpenAI-compat [DONE],
	// Responses response.completed): the connection was cut, reset, or
	// closed mid-body. This is the one transient failure that carries NO
	// structured provider response to classify from — no HTTP status (the
	// header already said 200), no inline error event — so it gets its own
	// mark (MarkStreamTruncated) at the adapters' stream-read boundary
	// rather than riding classifyStatus/classifyErrorType. Field data:
	// the 2026-08-06 incident's gateway cut streams at a ~111s ceiling
	// with HTTP 200 and a handful of chunks delivered; the resulting bare
	// io.EOF was classified deterministic and parked a goal loop that a
	// prompt re-issue minutes later showed was perfectly healthy.
	RetryableStreamTruncated RetryableClass = "stream_truncated"
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

// MarkStreamTruncated wraps a stream-read error as RetryableStreamTruncated,
// giving the bare transport error (typically io.EOF, or a "connection reset"
// net error) a message that names what actually happened. Adapters call it
// at exactly one place each: the stream-read error return in Next, BEFORE
// the terminal event was seen — never on the post-terminal io.EOF that
// signals normal end-of-iteration.
//
// A context cancellation or deadline is returned unchanged: that is the
// caller's own abort (POST /abort, shutdown, a stream watchdog's parent
// deadline), not provider weather, and callers like the goal loop check
// errors.Is(err, context.Canceled) to stop retrying — a check that would
// still work through RetryableError's Unwrap, but wrapping would lie about
// the failure being provider-side. Nil is returned unchanged, mirroring
// MarkRetryable.
func MarkStreamTruncated(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &RetryableError{
		Err:   fmt.Errorf("provider stream ended before completion: %w", err),
		Class: RetryableStreamTruncated,
	}
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

// PermanentError marks an adapter error as PERMANENTLY, deterministically
// failing — a request that will fail identically no matter how many times
// it is retried — as opposed to RetryableError's transient provider weather
// (an overload, a rate limit, a 5xx). It mirrors RetryableError's shape
// exactly (wrap via Unwrap, recover via errors.As, never string-matched by
// the engine), minus a class enum: unlike retryable weather, which comes in
// several named shapes an engine caller may want to distinguish (overloaded
// vs rate-limited vs stream-truncated), "permanent" is a single, undifferentiated
// bucket — the only thing a caller needs to know is "do not retry this",
// never which specific kind of unrecoverable request shape it was.
//
// Field motivation (NEP-5272, 2026-08-07): an orphaned tool_use left in
// session history by an earlier bug made every subsequent model call fail
// with the identical HTTP 400 invalid_request_error ("tool_use ids were
// found without tool_result blocks immediately after") — a request shape no
// amount of waiting or retrying can ever fix, since the malformed history is
// still exactly as malformed on attempt 2 as it was on attempt 1. Before this
// type existed, that error was classified deterministic-but-retryable (see
// goalWorkerRetries in engine/goal.go) and burned a full 3-attempt retry
// budget — three identical, guaranteed-to-fail model calls — before parking.
// See provider/anthropic/anthropic.go's apiError for the one place this gets
// marked, and engine/goal.go's promptTurnWithRetry for the fail-fast
// consumer, which mirrors the existing provider.IsContextOverflow precedent
// (see that function's doc comment) exactly: one stall record, no backoff,
// no further attempt.
type PermanentError struct {
	Err error
}

// Error prefixes the wrapped error's message with "[permanent]", mirroring
// RetryableError.Error's convention so any consumer that only ever calls
// Error() (a journaled goal.stalled reason, a turn.end error, a
// session.error message) still surfaces the classification without needing
// to unwrap anything.
func (e *PermanentError) Error() string {
	return "[permanent] " + e.Err.Error()
}

// Unwrap exposes the original error to errors.Is/errors.As.
func (e *PermanentError) Unwrap() error { return e.Err }

// MarkPermanent wraps err as a PermanentError, or returns nil unchanged if
// err is nil (mirrors MarkRetryable's nil-passthrough convention, so
// adapters can call it unconditionally).
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
