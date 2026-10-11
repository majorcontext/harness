package prompt

import "strings"

// EngineContextOpenTag and EngineContextCloseTag wrap an engine-context block
// on the wire. Only RenderEngineContext emits them.
const (
	EngineContextOpenTag  = "<harness-engine-context>"
	EngineContextCloseTag = "</harness-engine-context>"
)

const (
	neutralizedOpenTag  = "(harness-engine-context)"
	neutralizedCloseTag = "(/harness-engine-context)"
)

// RenderEngineContext returns the wire text of an engine-context block. The
// body is neutralized, so it cannot forge a nested block.
func RenderEngineContext(body string) string {
	return EngineContextOpenTag + "\n" + NeutralizeEngineContextSentinel(body) + "\n" + EngineContextCloseTag
}

// NeutralizeEngineContextSentinel replaces a literal sentinel tag in s with a
// parenthesized form, so only RenderEngineContext emits the sentinel.
func NeutralizeEngineContextSentinel(s string) string {
	if !strings.Contains(s, EngineContextOpenTag) && !strings.Contains(s, EngineContextCloseTag) {
		return s
	}
	s = strings.ReplaceAll(s, EngineContextOpenTag, neutralizedOpenTag)
	s = strings.ReplaceAll(s, EngineContextCloseTag, neutralizedCloseTag)
	return s
}
