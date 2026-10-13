package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"slices"
	"strconv"
)

const hostCallSchema = `{"type":"object","properties":{"server":{"type":"string"},"tool":{"type":"string"},"mcp_args":{"type":"object"},"model":{"type":"string"},"system":{"type":"string"},"text":{"type":"string"},"max_tokens":{"type":"integer"}}}`

var pixelPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")

var hostCallTools = []toolSpec{
	{"fixture_mcp", "Call an MCP tool through the host.", json.RawMessage(hostCallSchema)},
	{"fixture_generate", "Make a model call through the host.", json.RawMessage(hostCallSchema)},
}

func withHostCallTools(tools []toolSpec) []toolSpec {
	return append(slices.Clone(tools), hostCallTools...)
}

// hostCall runs fixture_mcp, fixture_generate, and fixture_blob, which an extra tool names. ok is false for another tool.
func hostCall(tool string, params json.RawMessage) (result any, ok bool) {
	var req struct {
		Args struct {
			Server  string          `json:"server"`
			Tool    string          `json:"tool"`
			MCPArgs json.RawMessage `json:"mcp_args"`
			Model   string          `json:"model"`
			System  string          `json:"system"`
			Text    string          `json:"text"`
			Max     *int            `json:"max_tokens"`
		} `json:"args"`
	}
	_ = json.Unmarshal(params, &req)
	a := req.Args
	var method string
	var call, reply any
	switch tool {
	case "fixture_blob":
		return map[string]any{"output": []any{part{Type: "text", Text: "chart"}, map[string]any{"type": "blob", "media_type": "image/png", "data": pixelPNG}}}, true
	case "fixture_mcp":
		method, call = "client/mcp.call", map[string]any{"server": a.Server, "tool": a.Tool, "args": a.MCPArgs}
		reply = &struct {
			Content []part `json:"content"`
			IsError bool   `json:"is_error"`
		}{}
	case "fixture_generate":
		method = "client/generate"
		call = map[string]any{"model": a.Model, "system": a.System, "max_tokens": maxTokens(a.Max),
			"messages": []map[string]any{{"role": "user", "parts": []part{{Type: "text", Text: a.Text}}}}}
		reply = &struct {
			Message struct {
				Role  string `json:"role"`
				Model string `json:"model"`
				Parts []part `json:"parts"`
			} `json:"message"`
			Usage json.RawMessage `json:"usage"`
		}{}
	default:
		return nil, false
	}
	if config.PidFile != "" {
		_ = os.WriteFile(config.PidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	if err := callHost(method, call, reply); err != nil {
		return map[string]any{"output": []part{{Type: "text", Text: "host error: " + err.Error()}}, "is_error": true}, true
	}
	body, _ := json.Marshal(reply)
	return map[string]any{"output": []part{{Type: "text", Text: string(body)}}}, true
}

func maxTokens(v *int) int {
	if v == nil {
		return 64
	}
	return *v
}
