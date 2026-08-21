// Minimal, pattern-based secret masking applied to a retained tool result
// before it is written to disk (see toolresult.go's writeRetainedToolResult
// and review finding F4).
//
// This repo has no existing secret-masking utility to reuse (searched for
// maskSecrets/redact/text_utils-shaped helpers; none exist). This is
// deliberately minimal: it catches the obvious "KEY=value" /
// "KEY: value" env-dump and config-dump shapes a command's stdout/stderr
// routinely contains (an env dump, a printed config, a leaked credential in
// a log line) — it is NOT a general secret scanner, does not understand
// structured formats (JSON, YAML) beyond the same key[=:]value shape
// appearing inline, and cannot catch a secret with no recognizable key name
// next to it (a bare API token pasted with no label). See the PR body for
// the documented residual risk.
package engine

import "regexp"

// secretMaskPattern matches a key ending in (or containing, case-
// insensitively) one of the obvious secret-shaped names, immediately
// followed by "=" or ":" and a non-whitespace value. Matching on the KEY
// SUFFIX (not requiring it to start the token) is deliberate:
// "AWS_SECRET_ACCESS_KEY=AKIA..." must mask via the "access_key" (or
// "secret") alternative even though neither starts the identifier.
var secretMaskPattern = regexp.MustCompile(`(?i)(secret|token|password|api_key|access_key)([=:])\S+`)

// maskSecrets replaces the VALUE half of every key[=:]value match with a
// fixed "***", preserving the key name and separator so the masked output
// still reads as the shape it was (a model reading it back still sees
// "AWS_SECRET_ACCESS_KEY=***", not a mystery blank).
func maskSecrets(text string) string {
	return secretMaskPattern.ReplaceAllString(text, "${1}${2}***")
}
