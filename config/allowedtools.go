package config

import "strings"

// ServerPattern returns the server of an allowed-tools entry of the form mcp__<server>__*.
func ServerPattern(entry string) (server string, ok bool) {
	rest, ok := strings.CutPrefix(entry, "mcp__")
	if !ok {
		return "", false
	}
	server, ok = strings.CutSuffix(rest, "__*")
	if !ok || checkMCPServerName(server) != nil {
		return "", false
	}
	return server, true
}

// ToolAllowed reports whether allowed keeps the tool name: nil keeps every tool, and an entry keeps its own name or, in the form mcp__<server>__*, each tool of that server.
func ToolAllowed(allowed []string, name string) bool {
	if allowed == nil {
		return true
	}
	for _, e := range allowed {
		if e == name {
			return true
		}
		if server, ok := ServerPattern(e); ok {
			if tool, ok := strings.CutPrefix(name, "mcp__"+server+"__"); ok && tool != "" {
				return true
			}
		}
	}
	return false
}
