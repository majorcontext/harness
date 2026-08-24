package plugin

import "regexp"

// sessionErrorMaxLen caps the length (in runes) of a session.error message
// sent to plugins. Provider adapters sometimes embed full response bodies in
// errors; a cap keeps a single bad error from ballooning an event payload.
const sessionErrorMaxLen = 256

const truncationMarker = "...[truncated]"

// credentialPatterns matches obvious credential shapes that show up in
// wrapped HTTP errors: Authorization header values (any scheme), bare
// bearer tokens, and key=value query-string style secrets (api keys,
// tokens, secrets, passwords). Each pattern captures the leading "prefix"
// (header name / scheme / key name plus separator) in group 1 so the
// replacement can keep the shape of the message while dropping only the
// credential value itself. This is a best-effort pattern set, not a
// guarantee — see SanitizeSessionError and PROTOCOL.md.
var credentialPatterns = []*regexp.Regexp{
	// Authorization: <scheme> <value>  (header form, any casing/quoting)
	regexp.MustCompile(`(?i)(Authorization"?\s*:?\s*"?(?:Bearer|Basic|Token|Digest)\s+)[^\s",}]+`),
	// bearer <token> outside a recognized "Authorization:" prefix
	regexp.MustCompile(`(?i)(\bBearer\s+)[A-Za-z0-9._~+/=-]+`),
	// key=value secrets, e.g. ?api_key=..., access_token=..., secret=...,
	// password=..., access_key=... in query strings or free text. The
	// credential name may carry an arbitrary snake_case/kebab-case prefix
	// (access_, refresh_, x-access-, API_, ...): `_` is a word character,
	// so `\b` alone doesn't separate "access" from "_token" and the
	// pattern would otherwise miss access_token=/refresh_token=/API_SECRET=
	// while still matching the hyphenated equivalents. Matching an
	// optional `[A-Za-z0-9_-]*` prefix before the credential word covers
	// both cases without relying on a word boundary there.
	regexp.MustCompile(`(?i)(\b[A-Za-z0-9_-]*(?:api[_-]?key|access[_-]?key|tokens?|secrets?|passwords?|passwd)\s*=\s*)[^&\s"']+`),
}

// SanitizeSessionError best-effort sanitizes an error message before it
// crosses the plugin boundary as a session.error event: it redacts obvious
// credential shapes (bearer tokens, Authorization header values, key=value
// secrets such as api_key=... query params) and caps the result at
// sessionErrorMaxLen runes so a runaway provider response body embedded in
// an error can't blow up the event payload.
//
// This is best-effort, not a guarantee: err.Error() text is free-form, and a
// fixed pattern set cannot catch every credential shape a provider adapter
// might embed. Callers (and plugins) should still treat session.error
// messages as untrusted, potentially-sensitive strings.
func SanitizeSessionError(msg string) string {
	for _, pat := range credentialPatterns {
		msg = pat.ReplaceAllString(msg, "${1}[REDACTED]")
	}

	if r := []rune(msg); len(r) > sessionErrorMaxLen {
		msg = string(r[:sessionErrorMaxLen]) + truncationMarker
	}
	return msg
}
