package provider

import (
	"errors"
	"fmt"
)

// ErrorKind classifies a provider error for callers that need to branch on
// more than "it failed" — the goal loop (engine/goal.go) in particular,
// which must fail fast and permanently on a deterministic error instead of
// burning its retry budget on one that can never succeed.
//
// This is deliberately a single shared enum rather than a bespoke type per
// classification need: issue #62 (this file) needs "context overflow is
// deterministic, don't retry"; the concurrently in-flight
// fix/retryable-provider-backoff branch (issue #61) needs "overload/rate
// limit/5xx is transient, retry longer". Both are provider-error
// classifications an adapter is best placed to make (only it sees the wire
// shape) and both are consumed the same way by the engine (a type switch/
// errors.As on one *Error, never a string match) — so they belong on one
// Kind enum and one wrapper type rather than two independent ad hoc ones
// that the engine would have to know about separately. If that branch lands
// first, converge by adding its kind(s) here (e.g. ErrKindRetryable) rather
// than introducing a second classification type; if this lands first, that
// branch should do the same in reverse.
type ErrorKind int

const (
	// ErrKindUnknown is the zero value: an ordinary, unclassified error.
	// Existing callers that don't type-assert against *Error see no change
	// in behavior — plain errors flow through exactly as before.
	ErrKindUnknown ErrorKind = iota

	// ErrKindContextOverflow marks a deterministic prompt/context-window
	// overflow: the request as built cannot fit the model's input limit, so
	// retrying the identical request will fail identically. Callers (the
	// goal loop's promptTurnWithRetry) must fail fast on this — a retry
	// attempt is pure waste, not resilience.
	ErrKindContextOverflow

	// ErrKindRetryable is reserved for issue #61's classification
	// (overloaded_error/429/5xx and similar transient provider weather). No
	// adapter sets it yet; it exists here so the enum has a fixed home for
	// it the moment that branch lands — see the type doc comment above.
	ErrKindRetryable

	// ErrKindProviderExhausted marks an ACCOUNT-level supply wall: the API
	// key's usage limit, credit balance, quota, or spend cap is spent, so
	// the provider refuses the request until its own clock rolls over.
	// This is a THIRD thing, distinct from both neighbors it would
	// otherwise be filed under:
	//
	//   - Not deterministic-permanent. A malformed request fails the same
	//     way forever; this one succeeds again, unchanged, once the wall
	//     lifts. Adapters still mark it permanent for RETRY purposes (no
	//     backoff schedule outlives a monthly quota), but the work is
	//     resumable, not doomed.
	//   - Not transient weather. An overload or a burst rate limit clears
	//     in seconds and a second request may well succeed; this one is
	//     ACCOUNT-wide, so every concurrent session on the same key hits
	//     the identical wall at the identical moment.
	//
	// That last property is why the engine needs the classification at
	// all: a supervising parent whose child dies on provider weather may
	// reasonably retry or respawn, and a parent whose child dies on an
	// account wall must do neither — it must preserve the child and resume
	// it later (see engine's FailKindProviderExhausted).
	ErrKindProviderExhausted
)

// Error is a classified provider error. Adapters construct it only when they
// can classify structurally (a distinct error code/type the API contract
// guarantees) or, failing that, by matching the provider's own message text
// — message-matching happens ONLY inside the adapter that owns that wire
// format; the engine must never string-match a provider error itself, or
// every provider integration would need its own copy of that logic (and any
// wording change upstream would silently stop being detected).
type Error struct {
	Kind ErrorKind

	// Raw is the untouched, already provider-prefixed error text (e.g.
	// "anthropic: prompt is too long: 205102 tokens > 200000 maximum
	// (invalid_request_error, HTTP 400)") — always populated, and what
	// Error() falls back to when no better rendering applies.
	Raw string

	// PromptTokens and TokenLimit are the request size and the model's
	// input limit, parsed from the provider's message when
	// Kind==ErrKindContextOverflow and the adapter could extract them (both
	// zero otherwise — a message wording change upstream degrades to "still
	// classified, detail unavailable" rather than losing the classification
	// entirely).
	PromptTokens int
	TokenLimit   int

	// RecoverHint is the provider's own statement of WHEN access returns
	// ("2026-09-01 at 00:00 UTC"), parsed from the message when
	// Kind==ErrKindProviderExhausted and the adapter could extract it.
	// Empty otherwise, including for an exhaustion whose message names no
	// time at all — the classification never depends on the hint, so a
	// wording change upstream costs the hint, never the classification
	// (the same degrade rule PromptTokens/TokenLimit follow).
	//
	// Verbatim provider text, quoted into model-visible guidance by the
	// engine. It is never parsed into a time.Time: the formats vary by
	// plan and by endpoint, and a parent only needs to know roughly when
	// to come back, not to schedule against it.
	RecoverHint string
}

// Error renders a human-readable message. For a classified context overflow
// with both token counts known, it returns the deterministic, orchestrator-
// legible form ("context exhausted: prompt N tokens > limit M") the goal
// loop and last_turn.error surface — see docs/design (issue #62) — rather
// than the provider's own wording, which varies by vendor and by model.
// Every other case falls back to Raw unchanged.
func (e *Error) Error() string {
	if e.Kind == ErrKindContextOverflow && e.PromptTokens > 0 && e.TokenLimit > 0 {
		return fmt.Sprintf("context exhausted: prompt %d tokens > limit %d", e.PromptTokens, e.TokenLimit)
	}
	return e.Raw
}

// Unwrap lets errors.Is/errors.As see through to nothing further — Error is
// a leaf; Raw already carries whatever wrapping context an adapter wanted
// (e.g. "anthropic: ..."). Defined explicitly (returning nil) only to
// document that this is a deliberate leaf, not an oversight.
func (e *Error) Unwrap() error { return nil }

// IsContextOverflow reports whether err is (or wraps, via errors.As) a
// *provider.Error classified as ErrKindContextOverflow — the one place
// outside an adapter allowed to know this classification exists, so the
// engine's goal loop can fail fast without string-matching.
func IsContextOverflow(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Kind == ErrKindContextOverflow
}

// AsProviderExhausted reports whether err is (or wraps, via errors.As) a
// *provider.Error classified as ErrKindProviderExhausted, returning it so
// a caller can read RecoverHint. The IsContextOverflow precedent applied
// to the second classification the engine consumes: the adapter that owns
// the wire format decides, the engine only reads the typed value, and no
// engine code ever matches a provider message itself.
func AsProviderExhausted(err error) (*Error, bool) {
	var pe *Error
	if errors.As(err, &pe) && pe.Kind == ErrKindProviderExhausted {
		return pe, true
	}
	return nil, false
}
