package provider

import "testing"

func TestErrorContextOverflowMessage(t *testing.T) {
	e := &Error{Kind: ErrKindContextOverflow, Raw: "anthropic: prompt is too long: 205102 tokens > 200000 maximum (invalid_request_error, HTTP 400)", PromptTokens: 205102, TokenLimit: 200000}
	want := "context exhausted: prompt 205102 tokens > limit 200000"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestErrorContextOverflowFallsBackWithoutTokens(t *testing.T) {
	e := &Error{Kind: ErrKindContextOverflow, Raw: "anthropic: prompt is too long (invalid_request_error, HTTP 400)"}
	if got := e.Error(); got != e.Raw {
		t.Errorf("Error() = %q, want raw fallback %q", got, e.Raw)
	}
}

func TestIsContextOverflow(t *testing.T) {
	if IsContextOverflow(nil) {
		t.Error("nil classified as context overflow")
	}
	plain := &Error{Kind: ErrKindUnknown, Raw: "boom"}
	if IsContextOverflow(plain) {
		t.Error("unknown-kind error classified as context overflow")
	}
	overflow := &Error{Kind: ErrKindContextOverflow, Raw: "x", PromptTokens: 1, TokenLimit: 1}
	if !IsContextOverflow(overflow) {
		t.Error("context-overflow error not classified")
	}
}
