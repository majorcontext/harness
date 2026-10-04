package turn

import (
	"context"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// Toolset is the tools of one model call.
type Toolset struct {
	// Tools are described to the model.
	Tools []Tool
	// Deferred are not described, but the model may call them.
	Deferred []Tool
	// Prompt follows the system prompt.
	Prompt string
}

// Source gives tools that can change between the model calls of a turn.
type Source interface {
	// Toolset returns the tools of the next model call. history is the
	// conversation so far; allowed restricts the tools as AllowedTools does.
	Toolset(ctx context.Context, history []eventlog.Message, allowed []string) Toolset
}

// describe sets the tools and prompt of call and returns the tools it may
// run. all describes every tool, for a backend that owns the loop.
func describe(ctx context.Context, call *Request, tools []Tool, src Source, all bool) []Tool {
	ts := Toolset{Tools: tools}
	if src != nil {
		more := src.Toolset(ctx, call.History, call.AllowedTools)
		ts.Tools, ts.Deferred, ts.Prompt = append(slices.Clip(tools), more.Tools...), more.Deferred, more.Prompt
	}
	if all {
		ts.Tools, ts.Deferred = append(slices.Clip(ts.Tools), ts.Deferred...), nil
	}
	call.Tools = nil
	for _, t := range ts.Tools {
		call.Tools = append(call.Tools, t.Spec())
	}
	switch {
	case call.Instructions == "":
		call.Instructions = ts.Prompt
	case ts.Prompt != "":
		call.Instructions += "\n\n" + ts.Prompt
	}
	runnable := append(slices.Clip(ts.Tools), ts.Deferred...)
	call.Call = func(ctx context.Context, c protocol.ToolCall) protocol.ToolResult { return runTool(ctx, runnable, c) }
	return runnable
}

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
