// Package wirescrub replaces identifiers, tokens, and personal data in a
// recorded wire stream with stable placeholders. It keeps the structure and
// the field names of the stream.
package wirescrub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Marker is the text that every placeholder identifier carries.
const Marker = "FIXTURE"

var (
	prefixedID = regexp.MustCompile(`\b(msg|toolu|srvtoolu|req|resp|rs|fc|call|ws|chatcmpl|fp)([_-])([A-Za-z0-9]{12,})`)
	uuid       = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	email      = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	secret     = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}|Bearer\s+[A-Za-z0-9._~+/=-]{8,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}(\.[A-Za-z0-9_-]*)?`)
	opaque     = regexp.MustCompile(`"(signature|encrypted_content|obfuscation|accountUuid|organizationUuid|account_id|organization_id|email|user_id|userID|organizationName|displayName)"\s*:\s*"([^"\\]*)"`)
	worktree   = regexp.MustCompile(`/(?:Users|home)/[A-Za-z0-9._-]+`)
)

// Scrubber maps each distinct identifier to one placeholder. One Scrubber
// across the files of a recording keeps the mapping stable.
type Scrubber struct {
	ids     map[string]string
	extra   []string
	counter map[string]int
}

// New returns a Scrubber. Each element of literals is replaced by a
// placeholder wherever it appears; use it for a path or a name that a
// pattern cannot find.
func New(literals ...string) *Scrubber {
	s := &Scrubber{ids: map[string]string{}, counter: map[string]int{}}
	if home, err := os.UserHomeDir(); err == nil {
		literals = append(literals, home)
	}
	if u, err := user.Current(); err == nil {
		literals = append(literals, u.Username)
	}
	if host, err := os.Hostname(); err == nil {
		literals = append(literals, host)
	}
	for _, l := range literals {
		if real, err := filepath.EvalSymlinks(l); err == nil && real != l {
			literals = append(literals, real)
		}
	}
	for _, l := range literals {
		literals = append(literals, strings.NewReplacer("/", "-", ".", "-").Replace(l))
	}
	for _, l := range literals {
		if len(l) >= 3 {
			s.extra = append(s.extra, l)
		}
	}
	slices.SortFunc(s.extra, func(a, b string) int { return len(b) - len(a) })
	return s
}

func (s *Scrubber) placeholder(class, original, format string) string {
	key := class + "\x00" + original
	if p, ok := s.ids[key]; ok {
		return p
	}
	s.counter[class]++
	p := fmt.Sprintf(format, s.counter[class])
	s.ids[key] = p
	return p
}

// Bytes returns data with every identifier and secret replaced.
func (s *Scrubber) Bytes(data []byte) []byte { return []byte(s.String(string(data))) }

// String returns text with every identifier and secret replaced.
func (s *Scrubber) String(text string) string {
	text = secret.ReplaceAllString(text, "REDACTED")
	text = email.ReplaceAllStringFunc(text, func(m string) string {
		return s.placeholder("email", m, "user%d@example.com")
	})
	for _, l := range s.extra {
		text = strings.ReplaceAll(text, l, "/home/u")
	}
	text = worktree.ReplaceAllString(text, "/home/u")
	text = opaque.ReplaceAllStringFunc(text, func(m string) string {
		sub := opaque.FindStringSubmatch(m)
		if sub[2] == "" {
			return m
		}
		repl := s.placeholder("opaque:"+sub[1], sub[2], strings.ToUpper(sub[1])+"_"+Marker+"_%d")
		return strings.Replace(m, `"`+sub[2]+`"`, `"`+repl+`"`, 1)
	})
	text = uuid.ReplaceAllStringFunc(text, func(m string) string {
		return s.placeholder("uuid", strings.ToLower(m), "00000000-0000-4000-8000-%012d")
	})
	return prefixedID.ReplaceAllStringFunc(text, func(m string) string {
		sub := prefixedID.FindStringSubmatch(m)
		if !isID(sub[3]) {
			return m
		}
		return sub[1] + sub[2] + s.placeholder("id:"+sub[1], m, Marker+"%d")
	})
}

// Leaks lists the personal data and secrets that data still holds. A scrubbed
// recording has none.
func Leaks(data []byte) []string {
	text := string(data)
	var out []string
	for _, m := range secret.FindAllString(text, -1) {
		if m != "REDACTED" {
			out = append(out, "secret-like text: "+m[:min(len(m), 6)]+"...")
		}
	}
	for _, m := range email.FindAllString(text, -1) {
		if !strings.HasSuffix(m, "@example.com") {
			out = append(out, "email address")
		}
	}
	for _, m := range worktree.FindAllString(text, -1) {
		if m != "/home/u" {
			out = append(out, "home directory path")
		}
	}
	for _, m := range uuid.FindAllString(text, -1) {
		if !strings.HasPrefix(m, "00000000-0000-4000-8000-") {
			out = append(out, "unmapped uuid")
		}
	}
	for _, m := range prefixedID.FindAllStringSubmatch(text, -1) {
		if isID(m[3]) {
			out = append(out, "unmapped id with prefix "+m[1])
		}
	}
	for _, m := range opaque.FindAllStringSubmatch(text, -1) {
		if m[2] != "" && !strings.Contains(m[2], Marker) && !strings.HasSuffix(m[2], "@example.com") {
			out = append(out, "unmapped value of "+m[1])
		}
	}
	return out
}

// isID reports whether tail is the random part of a real identifier: it has a
// digit and is not a placeholder.
func isID(tail string) bool {
	return !strings.Contains(tail, Marker) && strings.ContainsAny(tail, "0123456789")
}

// TruncateJSONL shortens each string value of a JSON Lines stream that is
// longer than max bytes, and keeps every key and value type. Keys come out in
// sorted order.
func TruncateJSONL(data []byte, max int) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if err := enc.Encode(truncate(v, max)); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

func truncate(v any, max int) any {
	switch x := v.(type) {
	case string:
		if len(x) > max {
			return strings.ToValidUTF8(x[:max], "") + "..."
		}
	case []any:
		for i := range x {
			x[i] = truncate(x[i], max)
		}
	case map[string]any:
		for k := range x {
			x[k] = truncate(x[k], max)
		}
	}
	return v
}
