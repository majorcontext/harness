// Command pluginfixture is a minimal harness plugin for the contract suite.
// It speaks the NDJSON protocol directly and imports no harness package, so
// the suite checks the host against the wire spec, not against the Go SDK.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type request struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int64     `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type part struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

const objectSchema = `{"type":"object","properties":{"text":{"type":"string"}}}`

var manifest = map[string]any{
	"name":             "fixture",
	"version":          "0.0.1",
	"protocol_version": 1,
	"hooks":            []string{"system.transform", "tool.execute.before", "tool.execute.after"},
	"tools": []toolSpec{
		{"fixture_echo", "Echo the text argument.", json.RawMessage(objectSchema)},
		{"fixture_fail", "Return an error result.", json.RawMessage(objectSchema)},
		{"fixture_config", "Return the plugin config block.", json.RawMessage(objectSchema)},
		{"fixture_crash", "Exit the plugin process.", json.RawMessage(objectSchema)},
	},
}

var config struct {
	Segment string `json:"segment"`
}

var rawConfig json.RawMessage

func main() {
	in := bufio.NewReader(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 1 {
			serve(out, line)
		}
		if err != nil {
			return
		}
	}
}

func serve(out *json.Encoder, line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil || req.Method == "" {
		return
	}
	if req.ID == nil {
		if req.Method == "shutdown" {
			os.Exit(0)
		}
		return
	}
	resp := response{JSONRPC: "2.0", ID: *req.ID}
	result, err := handle(req.Method, req.Params)
	if err != nil {
		resp.Error = err
	} else {
		resp.Result = result
	}
	if werr := out.Encode(resp); werr != nil {
		os.Exit(1)
	}
}

func handle(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		var init struct {
			Config json.RawMessage `json:"config"`
		}
		_ = json.Unmarshal(params, &init)
		rawConfig = init.Config
		_ = json.Unmarshal(init.Config, &config)
		return manifest, nil
	case "hook/system.transform":
		if config.Segment == "" {
			return map[string]any{}, nil
		}
		return map[string]any{"segments": []string{config.Segment}}, nil
	case "hook/tool.execute.before":
		return before(params), nil
	case "hook/tool.execute.after":
		return after(params), nil
	case "tool/execute":
		return execute(params), nil
	}
	return nil, &rpcError{Code: -32601, Message: fmt.Sprintf("unknown method %q", method)}
}

func before(params json.RawMessage) any {
	var req struct {
		Tool string `json:"tool"`
		Args struct {
			Command string `json:"command"`
		} `json:"args"`
	}
	_ = json.Unmarshal(params, &req)
	if req.Tool != "bash" {
		return map[string]any{}
	}
	switch {
	case strings.Contains(req.Args.Command, "block-me"):
		return map[string]any{"deny": "blocked by fixture"}
	case strings.Contains(req.Args.Command, "rewrite-me"):
		return map[string]any{"args": map[string]string{"command": "echo rewritten"}}
	}
	return map[string]any{}
}

func after(params json.RawMessage) any {
	var req struct {
		Tool   string `json:"tool"`
		Output []part `json:"output"`
	}
	_ = json.Unmarshal(params, &req)
	if req.Tool != "bash" {
		return map[string]any{}
	}
	var seen []string
	for _, p := range req.Output {
		seen = append(seen, p.Text)
	}
	return map[string]any{"output": []part{{Type: "text", Text: "after-hook saw: " + strings.Join(seen, "")}}}
}

func execute(params json.RawMessage) any {
	var req struct {
		Tool string `json:"tool"`
		Args struct {
			Text string `json:"text"`
		} `json:"args"`
	}
	_ = json.Unmarshal(params, &req)
	switch req.Tool {
	case "fixture_echo":
		return map[string]any{"output": []part{{Type: "text", Text: "echo: " + req.Args.Text}}}
	case "fixture_fail":
		return map[string]any{"output": []part{{Type: "text", Text: "fixture failure"}}, "is_error": true}
	case "fixture_config":
		return map[string]any{"output": []part{{Type: "text", Text: string(rawConfig)}}}
	case "fixture_crash":
		os.Exit(3)
	}
	return map[string]any{"output": []part{{Type: "text", Text: "unknown tool"}}, "is_error": true}
}
