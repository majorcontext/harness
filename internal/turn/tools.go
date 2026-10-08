package turn

import (
	"context"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// HistoryTool is the name of the tool that gives a backend that owns its loop
// the conversation that its own session lacks.
const HistoryTool = "get_conversation_history"

// Toolset is the tools of one model call.
type Toolset struct {
	// Tools are described to the model.
	Tools []Tool
	// Deferred are not described, but the model may call them.
	Deferred []Tool
	// Prompt follows the system prompt.
	Prompt string
	// Hooks run around each tool call. nil: none.
	Hooks Hooks
}

// Hooks run around each tool call of a model call.
type Hooks interface {
	// Before returns the call to run, or a deny that becomes the error
	// result of the call. A denied call does not run.
	Before(ctx context.Context, c protocol.ToolCall) (run protocol.ToolCall, deny string)
	// After returns the result that the model sees.
	After(ctx context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult
}

// Joiner is a Hooks that also changes the result of each call once, in call
// order, after every After hook ran. A batch calls Join as it records the
// result, so a Join keeps state without a lock against the other calls.
type Joiner interface {
	Join(ctx context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult
}

// Source gives tools that can change between the model calls of a turn.
type Source interface {
	// Toolset returns the tools of the next model call. history is the
	// conversation so far; allowed restricts the tools as AllowedTools does.
	Toolset(ctx context.Context, history []eventlog.Message, allowed []string, model string) Toolset
}

// Fixed is a Source of tools that never change.
type Fixed []Tool

// Toolset implements Source: the tools that allowed keeps.
func (f Fixed) Toolset(_ context.Context, _ []eventlog.Message, allowed []string, _ string) Toolset {
	return Toolset{Tools: Restrict(f, allowed)}
}

// Sources gives the tools of each Source in order, their prompts joined by a
// blank line, and the hooks of every Source that has any, chained in order.
type Sources []Source

// Toolset implements Source.
func (s Sources) Toolset(ctx context.Context, history []eventlog.Message, allowed []string, model string) Toolset {
	var out Toolset
	var prompts []string
	var hooks chain
	for _, src := range s {
		ts := src.Toolset(ctx, history, allowed, model)
		out.Tools, out.Deferred = append(out.Tools, ts.Tools...), append(out.Deferred, ts.Deferred...)
		if ts.Prompt != "" {
			prompts = append(prompts, ts.Prompt)
		}
		if ts.Hooks != nil {
			hooks = append(hooks, ts.Hooks)
		}
	}
	out.Prompt = strings.Join(prompts, "\n\n")
	switch len(hooks) {
	case 0:
	case 1:
		out.Hooks = hooks[0]
	default:
		out.Hooks = hooks
	}
	return out
}

// chain runs each Hooks in order. A deny ends the chain.
type chain []Hooks

func (c chain) Before(ctx context.Context, call protocol.ToolCall) (protocol.ToolCall, string) {
	for _, h := range c {
		var deny string
		if call, deny = h.Before(ctx, call); deny != "" {
			return call, deny
		}
	}
	return call, ""
}

func (c chain) After(ctx context.Context, call protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	for _, h := range c {
		r = h.After(ctx, call, r)
	}
	return r
}

// Join runs the Join of each Hooks that has one, in order.
func (c chain) Join(ctx context.Context, call protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	for _, h := range c {
		if j, ok := h.(Joiner); ok {
			r = j.Join(ctx, call, r)
		}
	}
	return r
}

// runner runs the calls of one model call between the hooks of its Toolset.
type runner struct {
	runnable []Tool
	hooks    Hooks
}

func (t runner) run(ctx context.Context, c protocol.ToolCall) protocol.ToolResult {
	return runTool(ctx, t.runnable, t.hooks, c)
}

func (t runner) join(ctx context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	if j, ok := t.hooks.(Joiner); ok {
		return j.Join(ctx, c, r)
	}
	return r
}

func (t runner) find(name string) Tool {
	if i := slices.IndexFunc(t.runnable, func(x Tool) bool { return x.Spec().Name == name }); i >= 0 {
		return t.runnable[i]
	}
	return nil
}

// alone reports whether the tool name runs Alone. A tool whose Spec panics is
// Alone, so that its panic reaches the result of its own call.
func (t runner) alone(name string) (alone bool) {
	defer func() {
		if recover() != nil {
			alone = true
		}
	}()
	_, ok := t.find(name).(Alone)
	return ok
}

// key returns the key of c. A tool whose Key or Spec panics gets one key per
// tool name, so that its calls run one at a time and the panic reaches the
// result of its own call.
func (t runner) key(c protocol.ToolCall) (key string) {
	defer func() {
		if recover() != nil {
			key = "panicking-key:" + c.Name
		}
	}()
	if k, ok := t.find(c.Name).(Keyed); ok {
		return k.Key(c)
	}
	return ""
}

// describe sets the tools, prompt, and Call of call and returns the runner of
// its calls. all describes every tool, for a backend that owns the
// loop.
func describe(ctx context.Context, call *Request, src Source, all bool) runner {
	var ts Toolset
	if src != nil {
		ts = src.Toolset(ctx, call.History, call.AllowedTools, call.Model)
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
	t := runner{append(slices.Clip(ts.Tools), ts.Deferred...), ts.Hooks}
	call.Call = t.run
	return t
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

// runTool runs c between the hooks h. It refuses a call to a tool that the
// model may not call, and turns an error into an error result that the
// model sees.
func runTool(ctx context.Context, tools []Tool, h Hooks, c protocol.ToolCall) protocol.ToolResult {
	if h == nil {
		return invoke(ctx, tools, c)
	}
	c, deny := h.Before(ctx, c)
	if deny != "" {
		return protocol.ToolResult{Text: deny, IsError: true}
	}
	return h.After(ctx, c, invoke(ctx, tools, c))
}

func invoke(ctx context.Context, tools []Tool, c protocol.ToolCall) protocol.ToolResult {
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
