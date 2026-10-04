package toolresult

import (
	"regexp"
	"strings"
)

// secretKeys match as a substring of the key, so AWS_SECRET_ACCESS_KEY
// matches through its tail. The separator after the key keeps the match narrow.
const secretKeys = `secret|token|password|api[_-]?key|access[_-]?key|client[_-]?secret|private[_-]?key`

// secretValue excludes every delimiter, so a match never eats the text
// after the value. The separator `=` or `:` plus a blank never matches the
// Go `:=` operator.
const secretValue = `[A-Za-z0-9_\-./+=]`

// secretPattern has five shapes: env or YAML, quoted JSON, Bearer, and an
// unquoted key with a double- or a single-quoted value. RE2 has no
// backreferences, so each quote has its own alternative.
var secretPattern = regexp.MustCompile(
	`(?i)(` + secretKeys + `)(=|:[ \t]+)(` + secretValue + `{8,1000})` +
		`|("[^"]*(?:` + secretKeys + `)[^"]*")(\s*:\s*)"[^"]*"` +
		`|(Authorization:\s*Bearer\s+)(` + secretValue + `{8,1000})` +
		`|(` + secretKeys + `)(=|:[ \t]+)"[^"]{0,1000}"` +
		`|(` + secretKeys + `)(=|:[ \t]+)'[^']{0,1000}'`,
)

var secretKeywords = []string{
	"secret", "token", "password",
	"api_key", "api-key", "apikey",
	"access_key", "access-key", "accesskey",
	"client_secret", "clientsecret",
	"private_key", "privatekey",
	"authorization",
}

// maskWindow is the number of lines after a candidate line that one regexp
// pass sees. A key always comes before its value, so no pass looks back.
const maskWindow = 3

func candidate(s string) bool {
	lower := strings.ToLower(s)
	for _, kw := range secretKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// Mask replaces the value of each recognized key and value with "***" and
// keeps the key and the separator. It is not a general secret scanner: a
// value with no key name next to it stays. A line with no candidate
// keyword never reaches the regexp, which keeps large output cheap.
func Mask(text string) string {
	if !candidate(text) {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 1 {
		return maskSpan(text)
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, g := range groups(lines) {
		if candidate(g) {
			b.WriteString(maskSpan(g))
		} else {
			b.WriteString(g)
		}
	}
	return b.String()
}

// groups joins each candidate line with the next maskWindow lines, and
// merges windows that overlap. Every other line is its own group.
func groups(lines []string) []string {
	n := len(lines)
	out := make([]string, 0, n)
	for i := 0; i < n; {
		if !candidate(lines[i]) {
			out = append(out, lines[i])
			i++
			continue
		}
		end := min(i+maskWindow+1, n)
		for j := i + 1; j < end; j++ {
			if candidate(lines[j]) {
				end = max(end, min(j+maskWindow+1, n))
			}
		}
		out = append(out, strings.Join(lines[i:end], ""))
		i = end
	}
	return out
}

func maskSpan(text string) string {
	matches := secretPattern.FindAllStringSubmatchIndex(text, -1)
	if matches == nil {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, m := range matches {
		b.WriteString(text[last:m[0]])
		switch {
		case m[2] >= 0:
			b.WriteString(text[m[2]:m[3]] + text[m[4]:m[5]] + "***")
		case m[8] >= 0:
			b.WriteString(text[m[8]:m[9]] + text[m[10]:m[11]] + `"***"`)
		case m[12] >= 0:
			b.WriteString(text[m[12]:m[13]] + "***")
		case m[16] >= 0:
			b.WriteString(text[m[16]:m[17]] + text[m[18]:m[19]] + `"***"`)
		case m[20] >= 0:
			b.WriteString(text[m[20]:m[21]] + text[m[22]:m[23]] + `'***'`)
		}
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}
