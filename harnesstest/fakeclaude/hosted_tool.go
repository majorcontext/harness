package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

const toolUseMeta = "claudecode/toolUseId"

// callHostedTool runs when FAKE_CLAUDE_CALL_TOOL names a tool and
// FAKE_CLAUDE_TOOL_LOG a file (the call takes the JSON arguments of
// FAKE_CLAUDE_CALL_ARGS), or when FAKE_CLAUDE_LIST_TOOLS names a file.
// It waits for harness to close stdin, which it does after it has recorded
// the result, then calls the tool or lists the tools on the harness MCP
// server of --mcp-config, and appends the response body to the file.
func callHostedTool(f *fake) {
	name, log := os.Getenv("FAKE_CLAUDE_CALL_TOOL"), os.Getenv("FAKE_CLAUDE_TOOL_LOG")
	args := obj{}
	if raw := os.Getenv("FAKE_CLAUDE_CALL_ARGS"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &args)
	}
	method, params := "tools/call", obj{"name": name, "arguments": args, "_meta": obj{toolUseMeta: "toolu_hosted"}}
	if list := os.Getenv("FAKE_CLAUDE_LIST_TOOLS"); list != "" {
		name, log, method, params = "tools/list", list, "tools/list", obj{}
	}
	if name == "" || log == "" {
		return
	}
	for {
		if _, ok := f.readLine(); !ok {
			break
		}
	}
	appendFile(log, hostedRPC(method, params)+"\n")
}

// hostedRPC posts one JSON-RPC request to the harness MCP server of
// --mcp-config and returns the response body, or the text of the failure.
func hostedRPC(method string, params obj) string {
	srv, ok := hostedServer()
	if !ok {
		return "no harness server in --mcp-config"
	}
	out, err := hostedPost(srv, method, params)
	if err != nil {
		return err.Error()
	}
	return string(out)
}

// hangAfterListing lists the tools of the harness MCP server into the file of
// FAKE_CLAUDE_LIST_TOOLS, prints a text, and hangs, so a test reads the tools
// that the host offers a CLI that is still running.
func hangAfterListing(f *fake) {
	if log := os.Getenv("FAKE_CLAUDE_LIST_TOOLS"); log != "" {
		appendFile(log, hostedRPC("tools/list", obj{})+"\n")
	}
	f.emit(say("Working on it."))
	hang(f)
}

// hostedPost sends one JSON-RPC request to srv and returns the response body.
func hostedPost(srv hostedEntry, method string, params obj) ([]byte, error) {
	body, _ := json.Marshal(obj{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range srv.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	return bytes.TrimSpace(out), err
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
