// Package codetool is the code tool: it runs a Starlark script that calls the other tools of its model call as
// functions. Each call of the tool gets a new interpreter, and the script has no access except through tools.
package codetool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// Name is the name of the code tool.
const Name = "code"

// Limits of one script.
const (
	// DefaultTimeoutS is the wall clock of a script, and the most that timeout_s may ask for.
	DefaultTimeoutS = 600
	// MaxSteps is the number of Starlark steps that one thread of a script may run.
	MaxSteps = 20_000_000
	// MaxPrintBytes is the most printed output that a script keeps.
	MaxPrintBytes = 256 << 10
	// MaxHeldBytes is the most result text that the nested calls of one script hand to it.
	MaxHeldBytes = 32 << 20
	// MaxBlobs is the most blobs of nested results that join the result of the script.
	MaxBlobs = 8
	// MaxParallel is the most functions of one parallel call that run at once.
	MaxParallel = 8
)

// notCallable are the tools that a script cannot call.
var notCallable = map[string]bool{Name: true, turn.AskTool: true, "goal": true, "model": true, "task": true, "mcp": true}

const description = `Run a Starlark script (a Python dialect) that calls tools as functions. Use it to chain tool calls, loop, filter, and add up results in one step. The result holds only what the script prints and the value that it ends with.

Each tool of this session is a global function under its own name. Call it with keyword arguments that match its input, such as read_file(path="a.go"). It returns the result text as a string. A tool that returns an error stops the script with that text. A tool that this session lacks is undefined.

Built-in tools:
  read_file(path, offset, limit)
  write_file(path, content)
  edit_file(path, old_string, new_string, replace_all)
  glob(pattern, path)
  grep(pattern, path, glob, case_insensitive)
  ls(path)
  bash(command)
  web_fetch(url, format, max_bytes)
  read_tool_result(handle, offset, limit, search, max_bytes)
  process(action, name, tail)
  session_info()
  load_skill(name)

Functions of the script:
  attempt(tool, **kwargs) calls a tool, never stops the script, and returns a struct with ok (bool) and text (string). tool is a function or a tool name: attempt(bash, command="false").
  parallel([f1, f2]) runs functions that take no argument at the same time, at most 8 at once, and returns their results in list order. Calls that touch one file still run in order. Do not change a value that two of the functions share.
  search_tools(query) returns the tools whose name or description matches, as a list of dicts with name and description.
  describe_tool(name) returns a dict with name, description, and input_schema. Use it for a tool that is not listed here, such as an MCP tool.
  json.encode(value), json.decode(text), and print(...).

The result is the printed output, then the value of the last expression as JSON when the script ends with one. Images that nested calls return follow the text, at most 8.
A script cannot call code, ask_user_question, goal, model, task, or mcp. It has no file, network, clock, or process access except through tools. A call keeps no state of an earlier call.
Limits: timeout_s (default and most 600), a step limit, and bounds on printed output and on result text. A failed script returns its printed output, the error, and the position.`

const schema = `{"type":"object","properties":{` +
	`"script":{"type":"string","description":"The Starlark source."},` +
	`"timeout_s":{"type":"integer","minimum":1,"maximum":600,"description":"Wall clock of the script in seconds (default 600)."}},` +
	`"required":["script"],"additionalProperties":false}`

// Tool is the code tool. The zero value is the tool that the runtime holds; Nest binds it to one model call.
type Tool struct{ n turn.Nested }

// New returns the code tool.
func New() Tool { return Tool{} }

// Spec returns the fixed spec of the tool.
func (Tool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: Name, Description: description, InputSchema: json.RawMessage(schema)}
}

// Nest binds the tool to the nested caller of a model call.
func (Tool) Nest(n turn.Nested) turn.Tool { return Tool{n: n} }

// Alone makes the tool run with no other call of its model call in flight.
func (Tool) Alone() {}

type input struct {
	Script   string `json:"script"`
	TimeoutS *int   `json:"timeout_s"`
}

// Run runs the script of call.
func (t Tool) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	if t.n == nil {
		return protocol.ToolResult{}, errors.New("code: this call has no way to run other tools")
	}
	var in input
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("code: invalid arguments: %w", err)
	}
	if strings.TrimSpace(in.Script) == "" {
		return protocol.ToolResult{}, errors.New("code: script is empty")
	}
	secs := DefaultTimeoutS
	if in.TimeoutS != nil {
		if *in.TimeoutS < 1 || *in.TimeoutS > DefaultTimeoutS {
			return protocol.ToolResult{}, fmt.Errorf("code: timeout_s must be between 1 and %d", DefaultTimeoutS)
		}
		secs = *in.TimeoutS
	}
	timeout := fmt.Errorf("script timed out after %d s", secs)
	ctx, cancel := context.WithTimeoutCause(ctx, time.Duration(secs)*time.Second, timeout)
	defer cancel()
	return run(ctx, t.n, call.ID, in.Script), nil
}
