package anthropic

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// streamErr runs one Stream call against a handler that answers with
// status and body, and returns the resulting error.
func streamErr(t *testing.T, status int, body string) error {
	t.Helper()
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body) //nolint:errcheck
	})
	_, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatal("Stream err = nil, want an error")
	}
	return err
}

// errBody builds Anthropic's error envelope.
func errBody(errType, msg string) string {
	return `{"type":"error","error":{"type":"` + errType + `","message":"` + msg + `"}}`
}

// TestUsageLimitIsProviderExhausted is the red-first test for the live
// incident: the account-level usage limit arrives as an ordinary HTTP 400
// invalid_request_error, indistinguishable from a malformed request unless
// this adapter classifies it.
func TestUsageLimitIsProviderExhausted(t *testing.T) {
	const msg = "You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 UTC."
	err := streamErr(t, http.StatusBadRequest, errBody("invalid_request_error", msg))

	pe, ok := provider.AsProviderExhausted(err)
	if !ok {
		t.Fatalf("AsProviderExhausted(%v) = false, want true", err)
	}
	if pe.RecoverHint != "2026-09-01 at 00:00 UTC" {
		t.Errorf("RecoverHint = %q, want %q", pe.RecoverHint, "2026-09-01 at 00:00 UTC")
	}
	if !strings.Contains(err.Error(), "usage limits") {
		t.Errorf("err = %q, want the provider message preserved", err.Error())
	}
	// Exhaustion is not worth an in-turn retry: the wall lifts on the
	// provider's own schedule, not on a backoff.
	if !provider.AsPermanent(err) {
		t.Errorf("AsPermanent(%v) = false, want true", err)
	}
	if _, retryable := provider.AsRetryable(err); retryable {
		t.Errorf("AsRetryable(%v) = true, want false", err)
	}
}

// TestQuota429IsProviderExhaustedNotPlainRateLimit proves the quota shape
// of a 429 leaves the retryable-weather path: no backoff schedule lifts a
// spent monthly quota.
func TestQuota429IsProviderExhaustedNotPlainRateLimit(t *testing.T) {
	err := streamErr(t, http.StatusTooManyRequests,
		errBody("rate_limit_error", "You have exceeded your monthly quota for this organization"))

	if _, ok := provider.AsProviderExhausted(err); !ok {
		t.Fatalf("AsProviderExhausted(%v) = false, want true", err)
	}
	if _, retryable := provider.AsRetryable(err); retryable {
		t.Errorf("AsRetryable(%v) = true, want false (a spent quota is not weather)", err)
	}
}

// TestPlainRateLimitStaysRetryable is the surplus-direction guard: an
// ordinary 429 must NOT be classified exhausted, or every burst throttle
// would tell a parent its whole fleet is walled.
func TestPlainRateLimitStaysRetryable(t *testing.T) {
	err := streamErr(t, http.StatusTooManyRequests,
		errBody("rate_limit_error", "Number of request tokens has exceeded your per-minute rate limit"))

	if pe, ok := provider.AsProviderExhausted(err); ok {
		t.Errorf("AsProviderExhausted(%v) = true (hint %q), want false", err, pe.RecoverHint)
	}
	class, retryable := provider.AsRetryable(err)
	if !retryable || class != provider.RetryableRateLimited {
		t.Errorf("AsRetryable = (%q, %v), want (%q, true)", class, retryable, provider.RetryableRateLimited)
	}
}

// TestMalformedRequestStaysPlainPermanent is the other surplus-direction
// guard: the NEP-5272 orphaned-tool_use 400 must stay an unqualified
// permanent error, never an exhaustion a parent would wait out.
func TestMalformedRequestStaysPlainPermanent(t *testing.T) {
	err := streamErr(t, http.StatusBadRequest,
		errBody("invalid_request_error", "tool_use ids were found without tool_result blocks immediately after"))

	if _, ok := provider.AsProviderExhausted(err); ok {
		t.Errorf("AsProviderExhausted(%v) = true, want false", err)
	}
	if !provider.AsPermanent(err) {
		t.Errorf("AsPermanent(%v) = false, want true", err)
	}
}

// TestContextOverflowStaysOverflow proves the exhaustion check did not
// steal the 400 the overflow classifier already owns.
func TestContextOverflowStaysOverflow(t *testing.T) {
	err := streamErr(t, http.StatusBadRequest,
		errBody("invalid_request_error", "prompt is too long: 205102 tokens > 200000 maximum"))

	if !provider.IsContextOverflow(err) {
		t.Errorf("IsContextOverflow(%v) = false, want true", err)
	}
	if _, ok := provider.AsProviderExhausted(err); ok {
		t.Errorf("AsProviderExhausted(%v) = true, want false", err)
	}
}

// TestUsageLimitShapes pins every recognized message shape, and one that
// must not match — the list is meant to grow, so its contents are the
// contract, not the regexp source.
func TestUsageLimitShapes(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    bool
	}{
		{"specified api usage limits", "You have reached your specified API usage limits.", true},
		{"credit balance", "Your credit balance is too low to access the Anthropic API", true},
		{"monthly quota", "You have exceeded your monthly quota", true},
		{"quota exceeded", "Organization quota exceeded", true},
		{"spend limit", "This request would exceed your organization's monthly spend limit", true},
		{"plain rate limit", "Number of requests has exceeded your per-minute rate limit", false},
		{"malformed request", "messages.0: unexpected field", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := parseUsageExhaustion(http.StatusBadRequest, tc.message)
			if got != tc.want {
				t.Errorf("parseUsageExhaustion(%q) ok = %v, want %v", tc.message, got, tc.want)
			}
		})
	}
}

// TestUsageLimitNeedsRecognizedStatus proves the structural gate runs
// first: a 500 whose body happens to quote a quota message is server
// weather, not an account wall.
func TestUsageLimitNeedsRecognizedStatus(t *testing.T) {
	if _, ok := parseUsageExhaustion(http.StatusInternalServerError, "You have exceeded your monthly quota"); ok {
		t.Error("parseUsageExhaustion on HTTP 500 = true, want false")
	}
}

// TestInlineUsageLimitIsProviderExhausted covers the same wall arriving
// mid-stream as an "error" SSE event, where there is no HTTP status to
// gate on — the path apiError's own classification cannot reach.
func TestInlineUsageLimitIsProviderExhausted(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: error\ndata: "+ //nolint:errcheck
			errBody("invalid_request_error", "You have reached your specified API usage limits. You will regain access on 2026-09-01.")+
			"\n\n")
	})
	stream, err := c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "m"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	_, nextErr := stream.Next()
	if nextErr == nil {
		t.Fatal("Next err = nil, want the inline error")
	}
	pe, ok := provider.AsProviderExhausted(nextErr)
	if !ok {
		t.Fatalf("AsProviderExhausted(%v) = false, want true", nextErr)
	}
	if pe.RecoverHint != "2026-09-01" {
		t.Errorf("RecoverHint = %q, want %q", pe.RecoverHint, "2026-09-01")
	}
	if !provider.AsPermanent(nextErr) {
		t.Errorf("AsPermanent(%v) = false, want true", nextErr)
	}
}
