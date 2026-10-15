package turn

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/majorcontext/harness/protocol"
)

// bannerKind is the kind of the pin of Request.Banner.
const bannerKind = "engine"

// HistoryTool is the name of the tool that gives a backend that owns its loop
// the conversation that its own session lacks.
const HistoryTool = "get_conversation_history"

// WebFetchTool is the name of the web_fetch tool.
const WebFetchTool = "web_fetch"

// Toolset is the tools of one model call.
type Toolset struct {
	// Tools are described to the model.
	Tools []Tool
	// Deferred are not described, but the model may call them.
	Deferred []Tool
	// Prompt follows the system prompt.
	Prompt string
	// Notices are statuses that the model reads inside the conversation, and
	// not in the system prompt, so that the system prompt stays stable.
	Notices []Notice
	// Hooks run around each tool call. nil: none.
	Hooks Hooks
}

// Notice is a status that the model reads inside the conversation. Each model
// call pins a notice when its Text differs from the newest notice of its Kind.
type Notice struct {
	Kind string
	// Text is the status now. Empty: nothing to report.
	Text string
	// Cleared is pinned once when Text turns empty after a status with text.
	Cleared string
	// At is the number of messages of the history that the pin follows. Zero:
	// the whole history of the call.
	At int
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
	Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, model string) Toolset
}

// Fixed is a Source of tools that never change.
type Fixed []Tool

// Toolset implements Source: the tools that allowed keeps.
func (f Fixed) Toolset(_ context.Context, _ []protocol.LogMessage, allowed []string, _ string) Toolset {
	return Toolset{Tools: Restrict(f, allowed)}
}

// Sources gives the tools of each Source in order, their prompts joined by a
// blank line, their notices, and the hooks of every Source that has any, chained in order.
type Sources []Source

// Toolset implements Source.
func (s Sources) Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, model string) Toolset {
	var out Toolset
	var prompts []string
	var hooks chain
	for _, src := range s {
		ts := src.Toolset(ctx, history, allowed, model)
		out.Tools, out.Deferred = append(out.Tools, ts.Tools...), append(out.Deferred, ts.Deferred...)
		if ts.Prompt != "" {
			prompts = append(prompts, ts.Prompt)
		}
		out.Notices = append(out.Notices, ts.Notices...)
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

// toolsetOf returns the tools of the model call call from src. A nil src gives none.
func toolsetOf(ctx context.Context, call *Request, src Source) Toolset {
	if src == nil {
		return Toolset{}
	}
	return src.Toolset(ctx, call.History, call.AllowedTools, call.Model)
}

// describe sets the tools, prompt, and Call of call from ts, pins its notices and
// banner through to, and returns the runner of its calls. all describes every
// tool and pins nothing, for a backend that owns the loop; a nil to pins nothing.
func describe(call *Request, ts Toolset, to Turn, all bool) runner {
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
	if !all && to != nil {
		call.Pins = to.Pin(append([]Notice{{Kind: bannerKind, Text: call.Banner, At: call.BannerAt}}, ts.Notices...), len(call.History))
	}
	t := runner{append(slices.Clip(ts.Tools), ts.Deferred...), ts.Hooks}
	call.Call = t.run
	if all {
		call.Call, call.Ask = t.bridged(), t.asker(to)
	}
	return t
}

// bridged returns the Call of a backend that owns the loop: the calls of one
// key run one at a time, and each result goes through the Join of the hooks, one
// result at a time, so the retention of two results never shares a handle.
func (t runner) bridged() func(context.Context, protocol.ToolCall) protocol.ToolResult {
	run := t.keyed()
	var mu sync.Mutex
	return func(ctx context.Context, c protocol.ToolCall) protocol.ToolResult {
		r := run(ctx, c)
		mu.Lock()
		defer mu.Unlock()
		return t.join(ctx, c, r)
	}
}

// keyed returns a run func that runs the calls of one key one at a time, in
// the order they get the key, as a batch does. A backend that owns the loop
// may run its calls at once, and calls to one file must not lose an edit.
func (t runner) keyed() func(context.Context, protocol.ToolCall) protocol.ToolResult {
	var mu sync.Mutex
	locks := map[string]chan struct{}{}
	return func(ctx context.Context, c protocol.ToolCall) protocol.ToolResult {
		key := t.key(c)
		if key == "" {
			return t.run(ctx, c)
		}
		mu.Lock()
		lock, ok := locks[key]
		if !ok {
			lock = make(chan struct{}, 1)
			locks[key] = lock
		}
		mu.Unlock()
		select {
		case lock <- struct{}{}:
			defer func() { <-lock }()
		case <-ctx.Done():
			return protocol.ToolResult{IsError: true, Text: context.Cause(ctx).Error()}
		}
		return t.run(ctx, c)
	}
}

// Restrict returns the tools that names lists, or every tool when names is nil.
func Restrict(tools []Tool, names []string) []Tool {
	if names == nil {
		return tools
	}
	return slices.DeleteFunc(slices.Clone(tools), func(t Tool) bool { return !slices.Contains(names, t.Spec().Name) })
}

func toolCalls(items []protocol.LogMessage) []protocol.ToolCall {
	var out []protocol.ToolCall
	for _, m := range items {
		for _, p := range m.Parts {
			if p.Type == protocol.LogPartToolCall {
				out = append(out, protocol.ToolCall{ID: p.CallID, Name: p.Name, Arguments: p.Arguments})
			}
		}
	}
	return out
}

// runTool runs c between the hooks h. It refuses a call to a tool that the
// model may not call, and turns an error into an error result that the
// model sees. A canceled ctx after Before skips the run, and After still sees
// the error result so that a hook which announced the call can announce its end.
func runTool(ctx context.Context, tools []Tool, h Hooks, c protocol.ToolCall) protocol.ToolResult {
	if h == nil {
		return invoke(ctx, tools, c)
	}
	c, deny := h.Before(ctx, c)
	if deny != "" {
		return protocol.ToolResult{Text: deny, IsError: true}
	}
	if err := ctx.Err(); err != nil {
		return h.After(ctx, c, protocol.ToolResult{Text: err.Error(), IsError: true})
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

// result builds the tool message of c. A blob with no bytes is dropped.
func result(to Turn, c protocol.ToolCall, r protocol.ToolResult) (protocol.LogMessage, error) {
	p := protocol.LogPart{Type: protocol.LogPartToolResult, CallID: c.ID, Name: c.Name, Text: r.Text, IsError: r.IsError}
	for _, b := range r.Blobs {
		if len(b.Data) == 0 {
			continue
		}
		bp, err := to.Attach(b.MediaType, b.Data)
		if err != nil {
			return protocol.LogMessage{}, err
		}
		p.Blobs = append(p.Blobs, bp)
	}
	return protocol.LogMessage{Role: protocol.RoleTool, Parts: []protocol.LogPart{p}}, nil
}
