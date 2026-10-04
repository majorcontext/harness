package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
)

// mcpTurn lists the tools of the "harness" server of --mcp-config into the
// init frame, then calls FAKE_CLAUDE_MCP_CALL as tool use toolu_m with the
// toolUseId meta that the real CLI sends. FAKE_CLAUDE_MCP_LOG receives the
// server URL and the listed tool names as one JSON line.
func mcpTurn(f *fake) bool {
	url, err := mcpURL()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var list struct{ Tools []struct{ Name string } }
	if err := rpc(url, "tools/list", obj{}, &list); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var tools, names []string
	if raw := os.Getenv("FAKE_CLAUDE_INIT_TOOLS"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &tools)
	}
	for _, t := range list.Tools {
		names = append(names, t.Name)
		tools = append(tools, "mcp__harness__"+t.Name)
	}
	if path := os.Getenv("FAKE_CLAUDE_MCP_LOG"); path != "" {
		b, _ := json.Marshal(obj{"url": url, "tools": names})
		appendFile(path, string(b)+"\n")
	}
	f.emit(system("init", obj{"session_id": f.sessionID, "tools": tools}))
	name, args := os.Getenv("FAKE_CLAUDE_MCP_CALL"), obj{"q": "hi"}
	f.emit(assistant(toolUse("toolu_m", "mcp__harness__"+name, args)))
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	call := obj{"name": name, "arguments": args, "_meta": obj{"claudecode/toolUseId": "toolu_m"}}
	if err := rpc(url, "tools/call", call, &res); err != nil || len(res.Content) != 1 {
		fmt.Fprintln(os.Stderr, "tools/call:", err, res)
		os.Exit(1)
	}
	f.emit(user(toolResult("toolu_m", res.Content[0].Text, res.IsError)), say("done"), success("done", 1, 1))
	return true
}

func mcpConfig() ([]byte, error) {
	i := slices.Index(os.Args, "--mcp-config")
	if i < 0 || i+1 >= len(os.Args) {
		return nil, fmt.Errorf("no --mcp-config")
	}
	return os.ReadFile(os.Args[i+1])
}

func logMCPConfig() {
	path := os.Getenv("FAKE_CLAUDE_MCP_CONFIG_LOG")
	if data, err := mcpConfig(); path != "" && err == nil {
		appendFile(path, string(bytes.TrimSpace(data))+"\n")
	}
}

func mcpURL() (string, error) {
	data, err := mcpConfig()
	if err != nil {
		return "", err
	}
	var cfg struct {
		MCPServers map[string]struct{ Type, URL string } `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}
	s, ok := cfg.MCPServers["harness"]
	if !ok || s.Type != "http" {
		return "", fmt.Errorf("no http server named harness in %s", data)
	}
	return s.URL, nil
}

func rpc(url, method string, params obj, out any) error {
	body, _ := json.Marshal(obj{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var msg struct {
		Result json.RawMessage
		Error  *struct{ Message string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return err
	}
	if msg.Error != nil {
		return fmt.Errorf("%s: %s", method, msg.Error.Message)
	}
	return json.Unmarshal(msg.Result, out)
}
