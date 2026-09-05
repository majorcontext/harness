package provider

import (
	"errors"
	"fmt"
)

// ErrorKind classifies an error reported by a provider.
type ErrorKind int

const (
	// ErrKindUnknown is an unclassified error.
	ErrKindUnknown ErrorKind = iota

	// ErrKindContextOverflow marks a request that exceeds the model input limit.
	ErrKindContextOverflow

	// ErrKindRetryable marks a transient provider error.
	ErrKindRetryable

	// ErrKindProviderExhausted marks an account limit that can recover later.
	ErrKindProviderExhausted
)

// Error is a classified provider error.
//
// Adapters classify their own wire errors. Callers must use its typed fields,
// not provider error text.
type Error struct {
	Kind ErrorKind

	// Raw is the provider error text.
	Raw string

	// PromptTokens and TokenLimit are parsed for a context overflow when known.
	PromptTokens int
	TokenLimit   int

	// RecoverHint is provider text that states when exhausted access may return.
	RecoverHint string
}

// Error returns a normalized context-overflow message when token counts exist.
// It returns Raw for all other errors.
func (e *Error) Error() string {
	if e.Kind == ErrKindContextOverflow && e.PromptTokens > 0 && e.TokenLimit > 0 {
		return fmt.Sprintf("context exhausted: prompt %d tokens > limit %d", e.PromptTokens, e.TokenLimit)
	}
	return e.Raw
}

// Unwrap returns nil because Error is a leaf error.
func (e *Error) Unwrap() error { return nil }

// IsContextOverflow reports whether err wraps an ErrKindContextOverflow.
func IsContextOverflow(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Kind == ErrKindContextOverflow
}

// AsProviderExhausted returns the exhaustion error wrapped by err, if any.
func AsProviderExhausted(err error) (*Error, bool) {
	var pe *Error
	if errors.As(err, &pe) && pe.Kind == ErrKindProviderExhausted {
		return pe, true
	}
	return nil, false
}
