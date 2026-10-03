package turn

import (
	"context"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// Restrict returns the tools that names lists, or every tool when names is nil.
func Restrict(tools []Tool, names []string) []Tool {
	if names == nil {
		return tools
	}
	return slices.DeleteFunc(slices.Clone(tools), func(t Tool) bool { return !slices.Contains(names, t.Spec().Name) })
}

func toolCalls(items []eventlog.Message) []protocol.ToolCall {
	var out []protocol.ToolCall
	for _, m := range items {
		for _, p := range m.Parts {
			if p.Type == eventlog.PartToolCall {
				out = append(out, protocol.ToolCall{ID: p.CallID, Name: p.Name, Arguments: p.Arguments})
			}
		}
	}
	return out
}

// runTool refuses a call to a tool that the model may not call, and turns an
// error into an error result that the model sees.
func runTool(ctx context.Context, tools []Tool, c protocol.ToolCall) protocol.ToolResult {
	i := slices.IndexFunc(tools, func(t Tool) bool { return t.Spec().Name == c.Name })
	if i < 0 {
		return protocol.ToolResult{Text: "no such tool available: " + c.Name, IsError: true}
	}
	res, err := tools[i].Run(ctx, c)
	if err != nil {
		return protocol.ToolResult{Text: err.Error(), IsError: true}
	}
	return res
}

func result(c protocol.ToolCall, r protocol.ToolResult) eventlog.Message {
	return eventlog.Message{Role: eventlog.RoleTool, Parts: []eventlog.Part{
		{Type: eventlog.PartToolResult, CallID: c.ID, Name: c.Name, Text: r.Text, IsError: r.IsError}}}
}
