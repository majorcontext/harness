package plugin

import (
	"strings"
	"testing"
)

func TestSanitizeSessionErrorCapsLength(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got := SanitizeSessionError(long)
	if len(got) > sessionErrorMaxLen+len("...[truncated]") {
		t.Fatalf("sanitized length = %d, want <= %d", len(got), sessionErrorMaxLen+len("...[truncated]"))
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 20)) {
		t.Fatalf("sanitized message lost its prefix: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("sanitized message = %q, want a truncation marker", got)
	}
}

func TestSanitizeSessionErrorRedactsBearerToken(t *testing.T) {
	msg := `request failed: Authorization: Bearer sk-live-abc123DEF456 returned 401`
	got := SanitizeSessionError(msg)
	if strings.Contains(got, "sk-live-abc123DEF456") {
		t.Fatalf("bearer token leaked: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("expected a redaction marker, got %q", got)
	}
}

func TestSanitizeSessionErrorRedactsKeyValueSecret(t *testing.T) {
	msg := `GET https://api.example.com/v1/models?api_key=sk-abcdef0123456789 failed with 403`
	got := SanitizeSessionError(msg)
	if strings.Contains(got, "sk-abcdef0123456789") {
		t.Fatalf("api key leaked: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("expected a redaction marker, got %q", got)
	}
}

func TestSanitizeSessionErrorRedactsAuthorizationHeader(t *testing.T) {
	msg := `dial error, headers: {"Authorization": "Basic dXNlcjpwYXNzd29yZA=="}`
	got := SanitizeSessionError(msg)
	if strings.Contains(got, "dXNlcjpwYXNzd29yZA==") {
		t.Fatalf("authorization header value leaked: %q", got)
	}
}

func TestSanitizeSessionErrorLeavesOrdinaryMessagesAlone(t *testing.T) {
	msg := "connection refused"
	if got := SanitizeSessionError(msg); got != msg {
		t.Errorf("SanitizeSessionError(%q) = %q, want unchanged", msg, got)
	}
}

func TestSanitizeSessionErrorRedactsSnakeCaseCredentialNames(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"access_token", `refresh failed: access_token=abc123DEF456 rejected`, "abc123DEF456"},
		{"refresh_token", `refresh failed: refresh_token=xyz789GHI012 rejected`, "xyz789GHI012"},
		{"upper_snake_secret", `config error: API_SECRET=topsecretvalue123 invalid`, "topsecretvalue123"},
		{"hyphenated_prefix", `token exchange: x-access-token=hyphensecretvalue failed`, "hyphensecretvalue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeSessionError(tc.msg)
			if strings.Contains(got, tc.want) {
				t.Fatalf("credential leaked: %q", got)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Fatalf("expected a redaction marker, got %q", got)
			}
		})
	}
}
