package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const toolUseMeta = "claudecode/toolUseId"

// callHostedTool runs when FAKE_CLAUDE_CALL_TOOL names a tool and
// FAKE_CLAUDE_TOOL_LOG a file (the name "initialize" sends the initialize request; the call takes the JSON arguments of
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
	if name == "initialize" {
		method, params = "initialize", obj{"protocolVersion": "2025-06-18", "capabilities": obj{}, "clientInfo": obj{"name": "fakeclaude", "version": "1"}}
	}
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
	return hostedPostCtx(context.Background(), srv, 1, method, params)
}

// hostedPostContext is hostedPost on the harness server of --mcp-config; it
// drops the request when ctx ends.
func hostedPostContext(ctx context.Context, id any, method string, params obj) ([]byte, error) {
	srv, ok := hostedServer()
	if !ok {
		return nil, fmt.Errorf("no harness server in --mcp-config")
	}
	return hostedPostCtx(ctx, srv, id, method, params)
}

func hostedPostCtx(ctx context.Context, srv hostedEntry, id any, method string, params obj) ([]byte, error) {
	msg := obj{"jsonrpc": "2.0", "method": method, "params": params}
	if id != nil {
		msg["id"] = id
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
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

type hostedText struct {
	Result struct {
		Content []struct{ Text string }
		IsError bool
	}
}

// hostedOffers reports whether the tools/list of the harness MCP server holds
// the tool name.
func hostedOffers(name string) bool {
	var list struct {
		Result struct{ Tools []struct{ Name string } }
	}
	if json.Unmarshal([]byte(hostedRPC("tools/list", obj{})), &list) != nil {
		return false
	}
	return slices.ContainsFunc(list.Result.Tools, func(t struct{ Name string }) bool { return t.Name == name })
}

// hostedCall calls a tool as the CLI does. A result that breaks the MCP
// CallToolResult schema is an error result with the text of the CLI, which
// reports such a result in place of the tool output.
func hostedCall(name string, args obj, id string) hostedText {
	var out hostedText
	body := hostedRPC("tools/call", obj{"name": name, "arguments": args, "_meta": obj{toolUseMeta: id}})
	_ = json.Unmarshal([]byte(body), &out)
	var msg struct{ Result json.RawMessage }
	if json.Unmarshal([]byte(body), &msg) == nil && msg.Result != nil {
		if err := checkCallResult(msg.Result); err != nil {
			bad := hostedText{}
			bad.Result.Content = []struct{ Text string }{{fmt.Sprintf(
				"MCP server %q returned a malformed result that failed schema validation: Invalid result for tools/call: %v", "harness", err)}}
			bad.Result.IsError = true
			return bad
		}
	}
	return out
}

// checkCallResult checks a tools/call result against the MCP schema: each
// content item has the string fields that its type requires.
func checkCallResult(raw json.RawMessage) error {
	var res struct {
		Content []map[string]json.RawMessage `json:"content"`
		IsError json.RawMessage              `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if res.IsError != nil && string(res.IsError) != "true" && string(res.IsError) != "false" {
		return fmt.Errorf("path [\"isError\"]: expected boolean, received %s", res.IsError)
	}
	if res.Content == nil {
		return fmt.Errorf("path [\"content\"]: expected array, received undefined")
	}
	required := map[string][]string{"text": {"text"}, "image": {"data", "mimeType"}, "audio": {"data", "mimeType"},
		"resource_link": {"uri", "name"}}
	isString := func(m map[string]json.RawMessage, k string) bool {
		var s string
		return m[k] != nil && string(m[k]) != "null" && json.Unmarshal(m[k], &s) == nil
	}
	for i, item := range res.Content {
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		if typ == "resource" {
			var r map[string]json.RawMessage
			if json.Unmarshal(item["resource"], &r) != nil || !isString(r, "uri") || !isString(r, "text") && !isString(r, "blob") {
				return fmt.Errorf("path [\"content\", %d, \"resource\"]: expected a resource with uri and text or blob", i)
			}
			continue
		}
		fields, ok := required[typ]
		if !ok {
			return fmt.Errorf("path [\"content\", %d, \"type\"]: unknown content type %q", i, typ)
		}
		for _, k := range fields {
			if !isString(item, k) {
				return fmt.Errorf("invalid_union ... path [\"%s\"]: expected string, received undefined (content item %d of type %s)", k, i, typ)
			}
		}
	}
	return nil
}

func (h hostedText) text() string {
	if len(h.Result.Content) == 0 {
		return ""
	}
	return h.Result.Content[0].Text
}

type turnCall struct {
	Name string `json:"name"`
	Args obj    `json:"args"`
}

// turnCalls runs the batches of FAKE_CLAUDE_TURN_CALLS during the turn, as the
// CLI does: each batch prints one tool_use frame per call, calls the tools of
// the harness MCP server at once, and prints the tool_result frames. The text
// $HANDLE in the arguments of a call stands for the first trh_N handle in the
// result of the batch before it.
func turnCalls(f *fake) {
	var batches [][]turnCall
	_ = json.Unmarshal([]byte(os.Getenv("FAKE_CLAUDE_TURN_CALLS")), &batches)
	handle := regexp.MustCompile(`trh_\d+`)
	last := ""
	for b, batch := range batches {
		results := make([]hostedText, len(batch))
		ids := make([]string, len(batch))
		for i, c := range batch {
			ids[i] = fmt.Sprintf("toolu_t%d_%d", b, i)
			args, _ := json.Marshal(c.Args)
			args = []byte(strings.ReplaceAll(string(args), "$HANDLE", handle.FindString(last)))
			batch[i].Args = obj{}
			_ = json.Unmarshal(args, &batch[i].Args)
			f.emit(assistant(toolUse(ids[i], "mcp__harness__"+c.Name, batch[i].Args)).inMessage("id", fmt.Sprintf("msg_t%d", b)))
		}
		var wg sync.WaitGroup
		for i, c := range batch {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = hostedCall(c.Name, c.Args, ids[i])
			}()
		}
		wg.Wait()
		for i := range batch {
			f.emit(user(toolResult(ids[i], results[i].text(), results[i].Result.IsError)))
		}
		last = results[0].text()
	}
}

// abandonTool runs a slow harness tool call and gives it up, as the CLI does
// when its tool timeout ends. The call is a bash command that writes its pid
// to abandon.pid in its working directory and then sleeps. Once the file
// exists, the mode "abandon_tool" sends notifications/cancelled for the
// request and leaves the request open, as the MCP SDK of the CLI does, and
// "drop_tool" closes the connection of the request. The file abandon.reply
// gets "reply" when the host answers the request and "no reply" when the
// request ends with no answer. Both wait for the process
// to be gone, so the turn ends only if the host stopped the call. They then
// print the error result of the timeout and a success result. The mode
// "hold_tool" keeps the request and hangs instead, so that an interrupt of
// the turn ends the call.
func abandonTool(f *fake) {
	const pidfile, replyFile, requestID = "abandon.pid", "abandon.reply", 7
	_ = os.Remove(pidfile)
	cmd := "echo $$ > " + pidfile + ".tmp && mv " + pidfile + ".tmp " + pidfile + " && exec sleep 600"
	args := obj{"command": cmd}
	f.emit(say("Let me check that."), assistant(toolUse("toolu_slow", "mcp__harness__bash", args)))
	ctx, drop := context.WithCancel(context.Background())
	defer drop()
	_ = os.Remove(replyFile)
	posted, answered := make(chan struct{}), make(chan bool, 1)
	go func() {
		defer close(posted)
		out, err := hostedPostContext(ctx, requestID, "tools/call", obj{"name": "bash", "arguments": args, "_meta": obj{toolUseMeta: "toolu_slow"}})
		answered <- err == nil && len(out) > 0
	}()
	pid := awaitPid(pidfile)
	switch f.mode {
	case "hold_tool":
		hang(f)
	case "drop_tool":
		drop()
	default:
		srv, _ := hostedServer()
		_, _ = hostedPostCtx(context.Background(), srv, nil, "notifications/cancelled", obj{"requestId": requestID, "reason": "Request timed out"})
	}
	awaitGone(pid)
	verdict := "no reply"
	select {
	case <-posted:
		if <-answered {
			verdict = "reply"
		}
	case <-time.After(replyWait):
	}
	appendFile(replyFile, verdict+"\n")
	f.emit(user(toolResult("toolu_slow", "MCP error -32001: Request timed out", true)), say("Done."), success("Done.", 0, 0))
}

// replyWait is how long the mode waits for an answer to a request that it gave up.
const replyWait = time.Second

// awaitPid returns the pid that the file holds, once the file exists.
func awaitPid(path string) int {
	for deadline := time.Now().Add(gateWait); time.Now().Before(deadline); time.Sleep(gatePoll) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
	}
	fmt.Fprintln(os.Stderr, "fakeclaude: the tool call never started")
	os.Exit(1)
	return 0
}

// awaitGone waits until the process pid is gone.
func awaitGone(pid int) {
	for deadline := time.Now().Add(gateWait); time.Now().Before(deadline); time.Sleep(gatePoll) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	fmt.Fprintln(os.Stderr, "fakeclaude: the tool call is still running after its request was given up")
	os.Exit(1)
}
