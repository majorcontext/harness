package acp

import (
	"encoding/json"
	"fmt"

	"github.com/majorcontext/harness/config"
)

type nameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type mcpServer struct {
	Type    string      `json:"type"`
	Name    string      `json:"name"`
	Command string      `json:"command"`
	Args    []string    `json:"args"`
	Env     []nameValue `json:"env"`
	URL     string      `json:"url"`
	Headers []nameValue `json:"headers"`
}

// sessionServers converts the mcpServers of session/new to the map of
// harness.WithSessionMCP. A stdio server has no type. The type sse is not
// advertised, so it is invalid. A name that two entries share is invalid.
func sessionServers(raw []json.RawMessage) (map[string]config.MCPServerSpec, error) {
	servers := make(map[string]config.MCPServerSpec, len(raw))
	for i, r := range raw {
		var s mcpServer
		if err := json.Unmarshal(r, &s); err != nil {
			return nil, invalidParams(fmt.Sprintf("mcpServers[%d] is not an MCP server", i))
		}
		if s.Name == "" {
			return nil, invalidParams(fmt.Sprintf("mcpServers[%d] has no name", i))
		}
		if _, dup := servers[s.Name]; dup {
			return nil, invalidParams(fmt.Sprintf("mcpServers names %q twice", s.Name))
		}
		var spec config.MCPServerSpec
		switch s.Type {
		case "", "stdio":
			if s.Command == "" {
				return nil, invalidParams(fmt.Sprintf("mcpServers[%d] has no command", i))
			}
			spec.Command = append([]string{s.Command}, s.Args...)
			for _, e := range s.Env {
				spec.Env = append(spec.Env, e.Name+"="+e.Value)
			}
		case "http":
			if s.URL == "" {
				return nil, invalidParams(fmt.Sprintf("mcpServers[%d] has no url", i))
			}
			spec.URL = s.URL
			if len(s.Headers) > 0 {
				spec.Headers = make(map[string]string, len(s.Headers))
			}
			for _, h := range s.Headers {
				if _, dup := spec.Headers[h.Name]; dup {
					return nil, invalidParams(fmt.Sprintf("mcpServers[%d] sets the header %q twice", i, h.Name))
				}
				spec.Headers[h.Name] = h.Value
			}
		default:
			return nil, invalidParams(fmt.Sprintf("mcpServers[%d] has the unsupported type %q", i, s.Type))
		}
		servers[s.Name] = spec
	}
	return servers, nil
}
