// Minimal, pattern-based secret masking applied to a retained tool result —
// review findings F4 (original) and N2/N3/N4/N5/N6/N11 (round-2 review of
// F4's first cut).
//
// This repo has no existing secret-masking utility to reuse (searched for
// maskSecrets/redact/text_utils-shaped helpers; none exist). This is
// deliberately minimal: it catches the obvious "KEY=value", "KEY: value"
// (space-YAML), quoted-JSON ("key": "value"), and "Authorization: Bearer
// TOKEN" shapes a command's stdout/stderr routinely contains — it is NOT a
// general secret scanner, does not understand arbitrary structured formats,
// and cannot catch a secret with no recognizable key name next to it (a bare
// API token pasted with no label). See the PR body for the documented
// residual risk.
//
// # Round-2 rewrite: N2 (data loss) and N4 (code corruption)
//
// The original pattern's value half was `\S+` — unbounded, greedy to the
// next whitespace. Two failure modes came from that:
//
//   - N2 (data loss): a single incidental key-shaped match (`&token=` inside
//     a URL, `token=<huge blob>` on one line with no other whitespace)
//     deleted everything from the match to the next whitespace — measured
//     2,097,164 bytes of a 4 MiB single-line retained result destroyed by
//     ONE masked "value" that was actually mostly unrelated adjacent
//     content, because \S+ does not stop at `&`, `?`, `,`, `"`, or any other
//     structural delimiter — only at whitespace.
//   - N4 (code corruption): `token:=lexer.Next()` (Go's `:=` short variable
//     declaration, not an assignment of a value TO a key named "token")
//     became `token:*** if...` — the old pattern treated the bare `:`
//     ahead of `=lexer.Next()` as a key/value separator and `\S+` ate the
//     rest of the statement.
//
// The value class here is bounded on BOTH axes: a character class that
// excludes exactly the delimiters that matter (`&`, `?`, `,`, `"`, `}`,
// whitespace, and code punctuation like `(`, `)`, `{`, `;`) so a match
// naturally stops at the next real delimiter instead of bleeding into
// adjacent content, AND a length cap ({8,1000})
// as a second, independent bound. The separator for the KEY=value /
// KEY: value shape is deliberately `=` (bare) or `:` followed by MANDATORY
// whitespace (`:\s+`) — never bare `:` alone — which is what excludes `:=`
// structurally: a Go short declaration has no whitespace between `:` and
// `=`, so neither separator alternative matches at that position, and
// there is nothing else in the pattern that could start a match there.
// TestMaskSecretsCodeCorpus pins this against realistic Go/Python/JS/TS
// source snippets, byte-identical.
//
// # Round 3: quoted env/YAML values
//
// A round-3 review round found `export TOKEN="secretvalue123"` — an
// UNQUOTED key with a QUOTED value, an extremely common shell/env-dump
// shape — slipping through entirely unmasked: the env/YAML alternative
// required its value class immediately after the separator, and the next
// byte there (`"`) is not in secretValueClass; the JSON alternative
// requires a QUOTED key, which a bare `TOKEN` lacks. Two more alternatives
// cover this (double- and single-quoted, spelled out separately — RE2 has
// no backreferences, so "whichever quote opened" cannot be one pattern).
//
// # N6: one combined pattern, one pass
//
// The three shapes (env/YAML, quoted-JSON, Bearer) are ONE regexp with
// alternation, walked ONCE via FindAllStringSubmatchIndex and rebuilt into
// one strings.Builder — not three sequential ReplaceAllString passes. Three
// separate full-text passes measured slower (556ms/4.4MB) than the ORIGINAL
// single unbounded pattern (352ms/4.4MB) despite matching less text per
// pass: each ReplaceAllString call re-scans the entire (already largely
// unchanged) string independently. One pass over the combined pattern
// measured well under the N6 100ms/4MB target — see
// TestMaskSecretsPerformance for the current number and the PR body for
// what was actually measured.
package engine

import (
	"regexp"
	"strings"
)

// secretKeyNames is the shared list of key-name substrings every pattern
// below recognizes, case-insensitively: secret, token, password, api_key
// (api-key, apikey), access_key, client_secret, private_key. Matched as a
// SUBSTRING of the key text, not anchored to a word boundary at the start —
// deliberately, so a compound identifier like "AWS_SECRET_ACCESS_KEY"
// still matches via its "access_key" (or "secret") tail even though Go's
// regexp \b treats the underscores as word characters and would refuse to
// place a boundary inside it. Safety against over-matching comes from
// requiring the SEPARATOR to sit immediately after this text (see the
// patterns below), not from a leading boundary.
const secretKeyNames = `secret|token|password|api[_-]?key|access[_-]?key|client[_-]?secret|private[_-]?key`

// secretValueClass is the bounded value character class (N2): alphanumeric
// plus the punctuation an ordinary token/key/base64(url) value legitimately
// contains (`_`, `-`, `.`, `/`, `+`, `=` for base64 padding). It excludes
// whitespace, quotes, and every common delimiter (`&`, `?`, `,`, `}`, `)`,
// `;`, ...) on purpose: a match stops there instead of consuming adjacent,
// unrelated content.
const secretValueClass = `[A-Za-z0-9_\-./+=]`

// secretMaskPattern is the ONE combined pattern (N6) covering all five
// recognized shapes, in this alternative order — each is structurally
// distinct enough (different leading character/shape: a bare key char, a
// `"`, literal "Authorization:", or an unquoted key immediately followed by
// a quote) that the alternatives never compete for the same match. Capture
// groups, 1-indexed (see maskSecrets):
//
//  1. env/YAML key            2. env/YAML separator (`=` or `:` + space)
//  3. env/YAML value (unused: replaced, never echoed)
//  4. JSON key (with quotes)  5. JSON separator (`\s*:\s*`)
//  6. Bearer prefix ("Authorization: Bearer ")
//  7. Bearer value (unused: replaced, never echoed)
//  8. quoted-env key (unquoted)   9. quoted-env separator — DOUBLE-quoted value
//
// 10. quoted-env key (unquoted)  11. quoted-env separator — SINGLE-quoted value
//
// RE2 (Go's regexp package) has no backreferences, so "the same quote
// character that opened" cannot be expressed as one alternative — groups
// 8-9 and 10-11 are the double- and single-quote cases spelled out
// separately instead of one pattern with `(["'])...\1`.
var secretMaskPattern = regexp.MustCompile(
	`(?i)(` + secretKeyNames + `)(=|:[ \t]+)(` + secretValueClass + `{8,1000})` +
		`|("[^"]*(?:` + secretKeyNames + `)[^"]*")(\s*:\s*)"[^"]*"` +
		`|(Authorization:\s*Bearer\s+)(` + secretValueClass + `{8,1000})` +
		`|(` + secretKeyNames + `)(=|:[ \t]+)"[^"]{0,1000}"` +
		`|(` + secretKeyNames + `)(=|:[ \t]+)'[^']{0,1000}'`,
)

// secretCandidateKeywords is the cheap pre-filter's keyword list (N6): the
// same key names secretKeyNames recognizes, plus "authorization", spelled
// out as plain lowercase substrings for a non-regex Contains check.
var secretCandidateKeywords = []string{
	"secret", "token", "password",
	"api_key", "api-key", "apikey",
	"access_key", "access-key", "accesskey",
	"client_secret", "clientsecret",
	"private_key", "privatekey",
	"authorization",
}

// containsSecretCandidate is a cheap (non-regex) case-insensitive check for
// whether s could possibly contain anything secretMaskPattern would match.
// It exists purely as a fast-reject: measured well over 50x cheaper than
// even a NO-MATCH run of secretMaskPattern itself over the same bytes (see
// TestMaskSecretsPerformance and the PR body) — the combined pattern's
// bounded repeats ({8,1000}) unroll into a large NFA, which Go's
// regexp package (RE2: no backtracking, but not free) walks byte-by-byte
// regardless of whether anything ever matches. A plain substring check
// skips that walk entirely for content with no candidate keyword at all —
// the common case for most retained tool output.
func containsSecretCandidate(s string) bool {
	lower := strings.ToLower(s)
	for _, kw := range secretCandidateKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// maskSecrets replaces the VALUE half of every recognized key/value shape
// with a fixed "***", preserving the key and separator so the masked
// output still reads as the shape it was (a model reading it back still
// sees "AWS_SECRET_ACCESS_KEY=***", not a mystery blank). Applied to BOTH
// what is written to disk and the inline preview the model sees (review
// finding N5) — the two must never disagree about which bytes are secret.
//
// N6 performance: two layers of fast-reject before the expensive regex
// ever runs.
//
//  1. A whole-text containsSecretCandidate check up front: no candidate
//     keyword anywhere at all (the common case for ordinary tool output)
//     returns text unchanged, unmodified, with no allocation beyond the
//     one lowercase scan.
//  2. Line-by-line: none of secretMaskPattern's three shapes ever spans a
//     newline (no alternative uses `.` or a multiline-crossing construct),
//     so it is exactly equivalent to run it once over the whole text or
//     once per line — but running it PER LINE means a line with no
//     candidate keyword is copied verbatim, at Contains cost, without ever
//     touching the regex engine. Retained tool output is almost always
//     multi-line, so this is where the real win is; only the pathological
//     F1 case (one multi-megabyte line with no newlines at all) falls back
//     to a single expensive full-text scan, same as before this
//     optimization — a documented residual, not a regression.
func maskSecrets(text string) string {
	if !containsSecretCandidate(text) {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 1 {
		return maskSecretsSpan(text)
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, line := range lines {
		if line == "" {
			continue
		}
		if containsSecretCandidate(line) {
			b.WriteString(maskSecretsSpan(line))
		} else {
			b.WriteString(line)
		}
	}
	return b.String()
}

// maskSecretsSpan runs the actual combined-pattern regex over one span of
// text (a single line, or — for the single-huge-line fallback in
// maskSecrets — the whole input) and rebuilds it with every match's value
// half replaced. FindAllStringSubmatchIndex walks the pattern ONCE (N6);
// the loop below copies everything BETWEEN matches verbatim and
// substitutes only key+separator+"***" (quoted to match the matched
// shape, where the shape was itself quoted) for each match. Group indices
// that did not participate in a given alternative come back -1 (Go's
// FindStringSubmatchIndex convention), which is how the switch below tells
// which of the five shapes matched without re-testing the text.
func maskSecretsSpan(text string) string {
	matches := secretMaskPattern.FindAllStringSubmatchIndex(text, -1)
	if matches == nil {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, m := range matches {
		b.WriteString(text[last:m[0]])
		switch {
		case m[2] >= 0: // env/YAML: group 1 (key) participated
			b.WriteString(text[m[2]:m[3]])
			b.WriteString(text[m[4]:m[5]])
			b.WriteString("***")
		case m[8] >= 0: // JSON: group 4 (quoted key) participated
			b.WriteString(text[m[8]:m[9]])
			b.WriteString(text[m[10]:m[11]])
			b.WriteString(`"***"`)
		case m[12] >= 0: // Bearer: group 6 (prefix) participated
			b.WriteString(text[m[12]:m[13]])
			b.WriteString("***")
		case m[16] >= 0: // quoted-env, double quotes: group 8 (unquoted key) participated
			b.WriteString(text[m[16]:m[17]])
			b.WriteString(text[m[18]:m[19]])
			b.WriteString(`"***"`)
		case m[20] >= 0: // quoted-env, single quotes: group 10 (unquoted key) participated
			b.WriteString(text[m[20]:m[21]])
			b.WriteString(text[m[22]:m[23]])
			b.WriteString(`'***'`)
		}
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}
