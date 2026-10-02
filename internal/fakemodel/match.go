package fakemodel

import "strings"

type Matcher func(Request) bool

func Any() Matcher { return func(Request) bool { return true } }

func LastUserText(substr string) Matcher {
	return func(r Request) bool { return strings.Contains(r.LastUserText(), substr) }
}

func SystemContains(substr string) Matcher {
	return func(r Request) bool { return strings.Contains(r.System, substr) }
}

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

func And(ms ...Matcher) Matcher {
	return func(r Request) bool {
		for _, m := range ms {
			if !m(r) {
				return false
			}
		}
		return true
	}
}
