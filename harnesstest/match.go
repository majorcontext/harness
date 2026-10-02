package harnesstest

import "strings"

// Matcher reports whether a Step answers a Request.
type Matcher func(Request) bool

// LastUserText matches a request whose last user text contains substr.
func LastUserText(substr string) Matcher {
	return func(r Request) bool { return strings.Contains(r.LastUserText(), substr) }
}

// SystemContains matches a request whose system prompt contains substr.
func SystemContains(substr string) Matcher {
	return func(r Request) bool { return strings.Contains(r.System, substr) }
}

// LastToolResult matches a request whose last message holds a result for the named tool.
func LastToolResult(toolName string) Matcher {
	return func(r Request) bool {
		if len(r.Messages) == 0 {
			return false
		}
		for _, p := range r.Messages[len(r.Messages)-1].Parts {
			if p.Kind == "tool_result" && p.ToolName == toolName {
				return true
			}
		}
		return false
	}
}
