package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
)

// mcpTurn lists the tools of each server of --mcp-config into the init frame
// as mcp__<server>__<tool>, then calls FAKE_CLAUDE_MCP_CALL as tool use
// toolu_m with the toolUseId meta that the real CLI sends. A call name with no
// "mcp__" prefix is a tool of the "harness" server. FAKE_CLAUDE_MCP_LOG
// receives the URL and the tool names of the "harness" server as one JSON line.
func mcpTurn(f *fake) bool {
	servers, err := mcpServers()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var tools []string
	if raw := os.Getenv("FAKE_CLAUDE_INIT_TOOLS"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &tools)
	}
	for _, server := range slices.Sorted(maps.Keys(servers)) {
		var list struct{ Tools []struct{ Name string } }
		if err := rpc(servers[server], "tools/list", obj{}, &list); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var names []string
		for _, t := range list.Tools {
			names = append(names, t.Name)
			tools = append(tools, "mcp__"+server+"__"+t.Name)
		}
		if path := os.Getenv("FAKE_CLAUDE_MCP_LOG"); path != "" && server == "harness" {
			b, _ := json.Marshal(obj{"url": servers[server], "tools": names})
			appendFile(path, string(b)+"\n")
		}
	}
	f.emit(system("init", obj{"session_id": f.sessionID, "tools": tools}))
	cliName, args := os.Getenv("FAKE_CLAUDE_MCP_CALL"), obj{"q": "hi"}
	if !strings.HasPrefix(cliName, "mcp__") {
		cliName = "mcp__harness__" + cliName
	}
	server, name, _ := strings.Cut(strings.TrimPrefix(cliName, "mcp__"), "__")
	f.emit(assistant(toolUse("toolu_m", cliName, args)))
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	call := obj{"name": name, "arguments": args, "_meta": obj{"claudecode/toolUseId": "toolu_m"}}
	if err := rpc(servers[server], "tools/call", call, &res); err != nil || len(res.Content) != 1 {
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

func mcpServers() (map[string]string, error) {
	data, err := mcpConfig()
	if err != nil {
		return nil, err
	}
	var cfg struct {
		MCPServers map[string]struct{ Type, URL string } `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	urls := map[string]string{}
	for name, s := range cfg.MCPServers {
		if s.Type != "http" {
			return nil, fmt.Errorf("server %s is not http in %s", name, data)
		}
		urls[name] = s.URL
	}
	if urls["harness"] == "" {
		return nil, fmt.Errorf("no http server named harness in %s", data)
	}
	return urls, nil
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
