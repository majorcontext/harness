package acp

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/majorcontext/harness/config"
)

type nameValue struct {
	Name  *string `json:"name"`
	Value *string `json:"value"`
}

// mcpServer holds a pointer for each field that the schema requires, so an
// omitted or null field differs from an empty one.
type mcpServer struct {
	Type    string       `json:"type"`
	Name    *string      `json:"name"`
	Command *string      `json:"command"`
	Args    *[]string    `json:"args"`
	Env     *[]nameValue `json:"env"`
	URL     *string      `json:"url"`
	Headers *[]nameValue `json:"headers"`
}

// pairs checks that every entry has a name and a value.
func pairs(list []nameValue) bool {
	for _, p := range list {
		if p.Name == nil || p.Value == nil {
			return false
		}
	}
	return true
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
		if s.Name == nil || *s.Name == "" {
			return nil, invalidParams(fmt.Sprintf("mcpServers[%d] has no name", i))
		}
		name := *s.Name
		if _, dup := servers[name]; dup {
			return nil, invalidParams(fmt.Sprintf("mcpServers names %q twice", name))
		}
		var spec config.MCPServerSpec
		switch s.Type {
		case "", "stdio":
			if s.Command == nil || *s.Command == "" || s.Args == nil || s.Env == nil || !pairs(*s.Env) {
				return nil, invalidParams(fmt.Sprintf("mcpServers[%d] needs command, args, and env", i))
			}
			spec.Command = append([]string{*s.Command}, *s.Args...)
			for _, e := range *s.Env {
				spec.Env = append(spec.Env, *e.Name+"="+*e.Value)
			}
		case "http":
			if s.URL == nil || *s.URL == "" || s.Headers == nil || !pairs(*s.Headers) {
				return nil, invalidParams(fmt.Sprintf("mcpServers[%d] needs url and headers", i))
			}
			spec.URL = *s.URL
			if len(*s.Headers) > 0 {
				spec.Headers = make(map[string]string, len(*s.Headers))
			}
			for _, h := range *s.Headers {
				key := http.CanonicalHeaderKey(*h.Name)
				if _, dup := spec.Headers[key]; dup {
					return nil, invalidParams(fmt.Sprintf("mcpServers[%d] sets the header %q twice", i, key))
				}
				spec.Headers[key] = *h.Value
			}
		default:
			return nil, invalidParams(fmt.Sprintf("mcpServers[%d] has the unsupported type %q", i, s.Type))
		}
		servers[name] = spec
	}
	return servers, nil
}
