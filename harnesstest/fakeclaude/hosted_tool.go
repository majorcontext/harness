package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const toolUseMeta = "claudecode/toolUseId"

// callHostedTool runs when FAKE_CLAUDE_CALL_TOOL names a tool and
// FAKE_CLAUDE_TOOL_LOG a file. It waits for harness to close stdin, which it
// does after it has recorded the result, calls the tool on the harness MCP
// server of --mcp-config, and appends the response body to the file.
func callHostedTool(f *fake) {
	name, log := os.Getenv("FAKE_CLAUDE_CALL_TOOL"), os.Getenv("FAKE_CLAUDE_TOOL_LOG")
	if name == "" || log == "" {
		return
	}
	for {
		if _, ok := f.readLine(); !ok {
			break
		}
	}
	srv, ok := hostedServer()
	if !ok {
		appendFile(log, "no harness server in --mcp-config\n")
		return
	}
	body, _ := json.Marshal(obj{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": obj{"name": name, "arguments": obj{}, "_meta": obj{toolUseMeta: "toolu_hosted"}}})
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	if err != nil {
		appendFile(log, err.Error()+"\n")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range srv.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		appendFile(log, err.Error()+"\n")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	appendFile(log, fmt.Sprintf("%s\n", bytes.TrimSpace(out)))
}

type hostedEntry struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// hostedServer returns the first server of --mcp-config named harness*.
func hostedServer() (hostedEntry, bool) {
	for i, a := range os.Args {
		if a != "--mcp-config" || i+1 >= len(os.Args) {
			continue
		}
		data, err := os.ReadFile(os.Args[i+1])
		if err != nil {
			return hostedEntry{}, false
		}
		var cfg struct {
			MCPServers map[string]hostedEntry `json:"mcpServers"`
		}
		if json.Unmarshal(data, &cfg) != nil {
			return hostedEntry{}, false
		}
		for name, e := range cfg.MCPServers {
			if strings.HasPrefix(name, "harness") {
				return e, true
			}
		}
	}
	return hostedEntry{}, false
}
