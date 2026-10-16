package protocol

import "github.com/majorcontext/harness/config"

// MCPServerSet is the body of PUT /mcp/servers: the whole live set of runtime MCP servers, each value an
// entry of the config key mcp_servers. A missing mcp_servers is invalid; an empty object removes every server.
type MCPServerSet struct {
	MCPServers map[string]config.MCPServerSpec `json:"mcp_servers"`
}

// MCPServerStatus is the live state of one MCP server. Reason is absent for a connected server.
type MCPServerStatus struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Attempts  int    `json:"attempts"`
	Parked    bool   `json:"parked"`
	Reason    string `json:"reason,omitempty"`
}

// MCPServers is the answer of PUT /mcp/servers and the result of the status action of the mcp tool.
type MCPServers struct {
	Servers []MCPServerStatus `json:"servers"`
}
