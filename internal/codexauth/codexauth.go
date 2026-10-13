// Package codexauth holds the ChatGPT subscription login of the Codex backend:
// the OAuth client of the Codex CLI, the credential file that `harness login
// codex` writes, the refresh of an access token, and the transport that binds
// the credentials to the endpoint of the provider.
//
// The package performs no I/O until a command or a model request needs it.
package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
)

const (
	// DefaultIssuer is the OAuth issuer of the Codex CLI. Source: codex-rs/login/src/server.rs DEFAULT_ISSUER.
	DefaultIssuer = "https://auth.openai.com"
	// ClientID is the public OAuth client id of the Codex CLI. Source: codex-rs/login/src/auth/manager.rs CLIENT_ID.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// Originator names this client to the Codex backend. The Codex CLI sends codex_cli_rs
	// (codex-rs/login/src/auth/default_client.rs DEFAULT_ORIGINATOR).
	Originator = "harness"
	// LoginCommand is the command that creates the login.
	LoginCommand = "harness login codex"

	refreshSkew = 5 * time.Minute
	authClaim   = "https://api.openai.com/auth"
)

// Credentials is the Codex login. The stored form is the JSON of this struct.
type Credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id"`
}

// String hides the tokens, so a format verb or a log call never prints them.
func (Credentials) String() string { return "codexauth.Credentials{redacted}" }

// GoString hides the tokens for the %#v verb.
func (c Credentials) GoString() string { return c.String() }

// LogValue hides the tokens in a structured log.
func (c Credentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Expiry is the exp claim of the access token. ok is false for a token that is not a JWT with exp.
func (c Credentials) Expiry() (t time.Time, ok bool) {
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if !jwtClaims(c.AccessToken, &claims) || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}

// expiring reports whether the access token ends within the refresh skew of now.
// A token without exp has an unknown expiry: only a 401 refreshes it.
func (c Credentials) expiring(now time.Time) bool {
	exp, ok := c.Expiry()
	return ok && !exp.After(now.Add(refreshSkew))
}

// remaining is the time until the access token ends. ok is false when the expiry is unknown or past.
func (c Credentials) remaining(now time.Time) (d time.Duration, ok bool) {
	exp, known := c.Expiry()
	if !known || !exp.After(now) {
		return 0, false
	}
	return exp.Sub(now), true
}

// AccountFromToken reads the chatgpt_account_id claim of a token.
func AccountFromToken(token string) string {
	var claims map[string]json.RawMessage
	if !jwtClaims(token, &claims) {
		return ""
	}
	var auth struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	_ = json.Unmarshal(claims[authClaim], &auth)
	return auth.AccountID
}

// jwtClaims decodes the payload of an unverified JWT into v.
func jwtClaims(token string, v any) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	return err == nil && json.Unmarshal(raw, v) == nil
}

// LoginRequiredError says that the Codex login is missing or unusable. A retry cannot resolve it.
type LoginRequiredError struct {
	// Reason says why, with no token in it.
	Reason string

	refreshRejected bool
}

func (e *LoginRequiredError) Error() string {
	msg := "not logged in to Codex; run `" + LoginCommand + "`"
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	return msg
}

// Permanent marks the error for provider.Do: it is not sent again.
func (e *LoginRequiredError) Permanent() bool { return true }

// IsLoginRequired reports whether err is a LoginRequiredError.
func IsLoginRequired(err error) bool {
	var le *LoginRequiredError
	return errors.As(err, &le)
}
