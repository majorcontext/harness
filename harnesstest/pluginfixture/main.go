// Command pluginfixture is a minimal harness plugin for the contract suite.
// It speaks the NDJSON protocol directly and imports no harness package, so
// the suite checks the host against the wire spec, not against the Go SDK.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

const (
	objectSchema = `{"type":"object","properties":{"text":{"type":"string"}}}`
	reportSchema = `{"type":"object","properties":{"edits":{"type":"integer"},"process":{"type":"boolean"},"events":{"type":"integer"}}}`
)

var manifest = map[string]any{
	"name":             "fixture",
	"version":          "0.0.1",
	"protocol_version": 1,
	"hooks":            []string{"system.transform", "tool.execute.before", "tool.execute.after", "event"},
	"tools": []toolSpec{
		{"fixture_echo", "Echo the text argument.", json.RawMessage(objectSchema)},
		{"fixture_fail", "Return an error result.", json.RawMessage(objectSchema)},
		{"fixture_config", "Return the plugin config block.", json.RawMessage(objectSchema)},
		{"fixture_crash", "Exit the plugin process.", json.RawMessage(objectSchema)},
		{"fixture_report", "Report what the plugin observed.", json.RawMessage(reportSchema)},
	},
}

var config struct {
	Segment   string `json:"segment"`
	Recall    bool   `json:"recall"`
	Model     bool   `json:"model"`
	ExtraTool string `json:"extra_tool"`
}

var rawConfig json.RawMessage

var (
	in     = bufio.NewReader(os.Stdin)
	out    = json.NewEncoder(os.Stdout)
	nextID = int64(1000)
)

// seen is touched only from the serving goroutine, so it needs no lock.
var seen struct {
	edited        []string
	eventSession  string
	systemSession string
	afterSession  string
	afterArgs     map[string]json.RawMessage
	events        []string
}

func main() {
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 1 {
			serve(line)
		}
		if err != nil {
			return
		}
	}
}

func serve(line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil || req.Method == "" {
		return
	}
	if req.ID == nil {
		switch req.Method {
		case "shutdown":
			os.Exit(0)
		case "hook/event":
			recordEvents(req.Params)
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
		if config.ExtraTool != "" {
			m := maps.Clone(manifest)
			m["tools"] = append(slices.Clone(manifest["tools"].([]toolSpec)), toolSpec{config.ExtraTool, "An extra tool.", json.RawMessage(objectSchema)})
			return m, nil
		}
		return manifest, nil
	case "hook/system.transform":
		return systemTransform(params), nil
	case "hook/tool.execute.before":
		return before(params), nil
	case "hook/tool.execute.after":
		return after(params), nil
	case "tool/execute":
		return execute(params), nil
	}
	return nil, &rpcError{Code: -32601, Message: fmt.Sprintf("unknown method %q", method)}
}

func systemTransform(params json.RawMessage) any {
	var req struct {
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
	}
	_ = json.Unmarshal(params, &req)
	seen.systemSession = req.SessionID
	var segments []string
	if config.Model {
		segments = append(segments, "MODEL: "+req.Model)
	}
	if config.Segment != "" {
		segments = append(segments, config.Segment)
	}
	if config.Recall {
		segments = append(segments, "LAST-USER: "+lastUserText(req.SessionID))
	}
	if len(segments) == 0 {
		return map[string]any{}
	}
	return map[string]any{"segments": segments}
}

// lastUserText asks the host for the session history while the
// system.transform dispatch is still in flight.
func lastUserText(sessionID string) string {
	var resp struct {
		Messages []struct {
			Role   string `json:"role"`
			Origin string `json:"origin"`
			Parts  []part `json:"parts"`
		} `json:"messages"`
	}
	if err := callHost("client/session.messages", map[string]string{"session_id": sessionID}, &resp); err != nil {
		return "error: " + err.Error()
	}
	for i := len(resp.Messages) - 1; i >= 0; i-- {
		m := resp.Messages[i]
		if m.Role == "user" && m.Origin == "" && len(m.Parts) > 0 {
			return m.Parts[0].Text
		}
	}
	return "none"
}

// callHost sends one numbered request and serves every other line until the
// matching response arrives.
func callHost(method string, params, result any) error {
	id := nextID
	nextID++
	if err := out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		os.Exit(1)
	}
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			os.Exit(1)
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(line, &msg) == nil && msg.Method == "" && msg.ID != nil && *msg.ID == id {
			if msg.Error != nil {
				return fmt.Errorf("%s", msg.Error.Message)
			}
			return json.Unmarshal(msg.Result, result)
		}
		if len(line) > 1 {
			serve(line)
		}
	}
}

func recordEvents(params json.RawMessage) {
	var batch struct {
		Events []struct {
			Type       string `json:"type"`
			SessionID  string `json:"session_id"`
			Properties struct {
				Path   string `json:"path"`
				Status string `json:"status"`
				Tool   string `json:"tool"`
			} `json:"properties"`
		} `json:"events"`
	}
	_ = json.Unmarshal(params, &batch)
	for _, ev := range batch.Events {
		detail := ev.Properties.Status + ev.Properties.Tool
		if ev.Type == "file.edited" {
			detail = map[bool]string{true: "absolute", false: "relative"}[filepath.IsAbs(ev.Properties.Path)]
		}
		seen.events = append(seen.events, strings.TrimSpace(ev.Type+" "+detail))
		if ev.Type == "file.edited" {
			seen.edited = append(seen.edited, filepath.Base(ev.Properties.Path))
			seen.eventSession = ev.SessionID
		}
	}
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
		SessionID string          `json:"session_id"`
		Tool      string          `json:"tool"`
		Args      json.RawMessage `json:"args"`
		Output    []part          `json:"output"`
	}
	_ = json.Unmarshal(params, &req)
	if seen.afterArgs == nil {
		seen.afterArgs = map[string]json.RawMessage{}
	}
	seen.afterSession = req.SessionID
	seen.afterArgs[req.Tool] = req.Args
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
		SessionID string `json:"session_id"`
		Tool      string `json:"tool"`
		Args      struct {
			Text    string `json:"text"`
			Edits   int    `json:"edits"`
			Process bool   `json:"process"`
			Events  int    `json:"events"`
		} `json:"args"`
	}
	_ = json.Unmarshal(params, &req)
	switch req.Tool {
	case "fixture_report":
		if req.Args.Events > 0 {
			return reportEvents(req.Args.Events)
		}
		return report(req.SessionID, req.Args.Edits, req.Args.Process)
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

// report waits for the wanted number of file.edited events, which reach the
// plugin asynchronously, then returns what the plugin observed. Paths are
// reduced to base names and session ids to a match against the call's own.
// With process set it reports only the working directory and script argument.
func report(sessionID string, edits int, process bool) any {
	serveUntil(func() bool { return len(seen.edited) >= edits })
	view := map[string]any{
		"edited":     seen.edited,
		"after_args": seen.afterArgs,
		"session_ids_match": map[string]bool{
			"system_transform": seen.systemSession == sessionID,
			"after":            seen.afterSession == sessionID,
			"event":            seen.eventSession == sessionID,
		},
	}
	if process {
		cwd, _ := os.Getwd()
		view = map[string]any{"cwd": filepath.Base(cwd), "argv": scriptArgs()}
	}
	body, _ := json.Marshal(view)
	return map[string]any{"output": []part{{Type: "text", Text: string(body)}}}
}

// reportEvents waits for n events and returns each as its type, then its
// status or tool.
func reportEvents(n int) any {
	serveUntil(func() bool { return len(seen.events) >= n })
	body, _ := json.Marshal(seen.events)
	return map[string]any{"output": []part{{Type: "text", Text: string(body)}}}
}

func serveUntil(done func() bool) {
	for !done() {
		line, err := in.ReadBytes('\n')
		if err != nil {
			os.Exit(1)
		}
		if len(line) > 1 {
			serve(line)
		}
	}
}

func scriptArgs() []string {
	var args []string
	for _, a := range os.Args[1:] {
		args = append(args, filepath.Base(a))
	}
	return args
}
