package turn

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/majorcontext/harness/internal/tracing"
	"github.com/majorcontext/harness/protocol"
)

// BannerKind is the kind of the pin of Request.Banner.
const BannerKind = "engine"

// TurnContextKind is the kind of a pin of the turn context of an input. The log records no such pin.
const TurnContextKind = "turn-context"

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
	// Specs, when not nil, are the tools described to the model in place of the Spec of each of Tools.
	Specs []protocol.ToolSpec
	// SearchAllowed, when not nil, is the search_allowed of the declaration that Specs come from. The call sends
	// the native web search only when the session allows it now too.
	SearchAllowed *bool
	// System, when not nil, is the system prompt of the call in place of the system prompt of the request. Prompt follows it.
	System []string
	// SkillList is the skill segment of System, for a backend that builds its own prompt.
	SkillList string
	// Prompt follows the system prompt.
	Prompt string
	// MCPInstructions are the instructions of the MCP servers, which Prompt also holds.
	MCPInstructions MCPInstructions
	// Gone names the MCP servers that connected once and left the live set. A declaration records them.
	Gone []Gone
	// Notices are statuses that the model reads inside the conversation, and
	// not in the system prompt, so that the system prompt stays stable.
	Notices []Notice
	// Resolver finds a tool that a source can expose on demand.
	Resolver ToolResolver
	// Hooks run around each tool call. nil: none.
	Hooks Hooks
	// CodeMode refuses a direct model call of a tool that only a script calls.
	CodeMode bool
	// BaseCode is the base mode of the declaration that Specs come from: the mode that the tools array states.
	BaseCode bool
}

// MCPInstructions are the usage guidance of the MCP servers, for a backend
// that serves the tools to an external harness.
type MCPInstructions struct {
	// Common is the guidance on the tools that are not the tools of one server.
	Common string
	// Servers holds the instructions of each server by its name.
	Servers map[string]string
	// SessionIDHeaderServers names servers that receive Harness-Session-Id.
	SessionIDHeaderServers map[string]bool
}

// Notice is a status that the model reads inside the conversation. Each model
// call pins a notice when its Text differs from the newest notice of its Kind.
type Notice struct {
	Kind string
	// Text is the status now. Empty: nothing to report.
	Text string
	// Cleared is pinned once when Text turns empty after a status with text.
	Cleared string
	// Initial is the status that the model reads before the first pin of this Kind, so a Text equal to it pins nothing.
	Initial string
	// At is the number of messages of the history that the pin follows. Zero:
	// the whole history of the call.
	At int
	// Declared is true when a declaration states what the text says. A pin of this kind that is older than the
	// newest declaration, and whose text is not the text now, leaves the history that the model reads.
	Declared bool
	// Transient makes the model read Text in the turn of the first call that has text, at the place of that call,
	// like a turn context. The log records no pin and no later turn reads it, and Cleared is unused.
	Transient bool
}

// Gone is the removal of an MCP server. Gen tells a server that was added again, or in another process, from the one that left.
type Gone struct {
	Name string `json:"name"`
	Gen  uint64 `json:"gen"`
}

// Declarable is a Tool that tells deferred tools from loaded ones. Declared returns the tool for a model call whose
// tools array holds only the tools that holds accepts, whose declaration accounts for the tools that accounts accepts
// (the tools array and the deferred tools that were live when the declaration was built), whose prompt segment is
// prompt, and which recorded the removals in gone, and the notices that replace the notices of the same Kind.
type Declarable interface {
	Declared(holds, accounts func(name string) bool, prompt string, gone []Gone) (Tool, []Notice)
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

// Forwarder is a Tool that has actions which run a deferred tool. Forward returns the call of
// that tool, and false when c is another action. The call runs between the hooks, and only
// a tool of Toolset.Deferred can answer it.
type Forwarder interface {
	Forward(c protocol.ToolCall) (protocol.ToolCall, bool)
}

// ToolResolver finds a tool that a source can expose on demand.
type ToolResolver interface {
	ResolveTool(context.Context, string) Tool
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
	var prompts, common []string
	var hooks chain
	var resolvers toolResolvers
	for _, src := range s {
		ts := src.Toolset(ctx, history, allowed, model)
		out.Tools, out.Deferred = append(out.Tools, ts.Tools...), append(out.Deferred, ts.Deferred...)
		if ts.Prompt != "" {
			prompts = append(prompts, ts.Prompt)
		}
		if ts.MCPInstructions.Common != "" {
			common = append(common, ts.MCPInstructions.Common)
		}
		if len(ts.MCPInstructions.Servers) > 0 && out.MCPInstructions.Servers == nil {
			out.MCPInstructions.Servers = map[string]string{}
		}
		maps.Copy(out.MCPInstructions.Servers, ts.MCPInstructions.Servers)
		if len(ts.MCPInstructions.SessionIDHeaderServers) > 0 && out.MCPInstructions.SessionIDHeaderServers == nil {
			out.MCPInstructions.SessionIDHeaderServers = map[string]bool{}
		}
		maps.Copy(out.MCPInstructions.SessionIDHeaderServers, ts.MCPInstructions.SessionIDHeaderServers)
		out.Notices = append(out.Notices, ts.Notices...)
		out.Gone = append(out.Gone, ts.Gone...)
		if ts.Resolver != nil {
			resolvers = append(resolvers, ts.Resolver)
		}
		if ts.Hooks != nil {
			hooks = append(hooks, ts.Hooks)
		}
	}
	if len(resolvers) > 0 {
		out.Resolver = resolvers
	}
	out.Prompt, out.MCPInstructions.Common = strings.Join(prompts, "\n\n"), strings.Join(common, "\n\n")
	switch len(hooks) {
	case 0:
	case 1:
		out.Hooks = hooks[0]
	default:
		out.Hooks = hooks
	}
	return out
}

type toolResolvers []ToolResolver

func (rs toolResolvers) ResolveTool(ctx context.Context, name string) Tool {
	for _, r := range rs {
		if t := r.ResolveTool(ctx, name); t != nil {
			return t
		}
	}
	return nil
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

type inputNormalizer interface {
	NormalizeInput(context.Context, json.RawMessage) (json.RawMessage, error)
}

type inputForwarder interface {
	ForwardInput(protocol.ToolCall, json.RawMessage) (protocol.ToolCall, error)
}

func (t runner) normalize(ctx context.Context, c protocol.ToolCall) (out protocol.ToolCall, err error) {
	out = c
	defer func() {
		if recover() != nil {
			out = c
			err = errors.New(toolPanicked)
		}
	}()
	tools, target := t.target(c)
	tool := findIn(tools, target.Name)
	if tool == nil && t.resolver != nil {
		tool = t.resolver.ResolveTool(ctx, target.Name)
	}
	n, ok := tool.(inputNormalizer)
	if !ok {
		return c, nil
	}
	input, err := n.NormalizeInput(ctx, slices.Clone(target.Arguments))
	if err != nil {
		return c, err
	}
	if !json.Valid(input) {
		return c, errors.New("tool input normalization returned invalid JSON")
	}
	if target.Name != c.Name {
		if f, ok := findIn(t.runnable, c.Name).(inputForwarder); ok {
			return f.ForwardInput(c, input)
		}
		return c, errors.New("tool input cannot be forwarded")
	}
	c.Arguments = slices.Clone(input)
	return c, nil
}

// runner runs the calls of one model call between the hooks of its Toolset.
type runner struct {
	runnable []Tool
	// deferred are the tools that a Forwarder may run.
	deferred []Tool
	resolver ToolResolver
	hooks    Hooks
	// locks, when set, order the calls of Request.Call and the nested calls of a tool.
	locks *keyLocks
	// nests names the tools that run nested calls.
	nests map[string]bool
	// codeMode refuses a direct call of a tool that only a script calls. A nested call runs on a runner with none.
	codeMode bool
	// joinMu, when set, makes join run one result at a time across every
	// callback of the runner.
	joinMu *sync.Mutex
	// scope starts the span of each call; session is the conversation it belongs to.
	scope   tracing.Scope
	session string
	// mcpCalls is the MCP call counter of the turn, put in the context of each call: a backend that owns the loop
	// runs its calls under the context of its own transport, which holds none.
	mcpCalls *atomic.Int64
}

func (t runner) run(ctx context.Context, c protocol.ToolCall) protocol.ToolResult {
	tools, c := t.target(c)
	if msg := refusedInCodeMode(c.Name); t.codeMode && msg != "" {
		return protocol.ToolResult{Text: msg, IsError: true}
	}
	ctx, span := t.scope.Start(ctx, "execute_tool "+c.Name, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"), attribute.String("gen_ai.tool.name", c.Name),
		attribute.String("gen_ai.tool.call.id", c.ID), attribute.String("gen_ai.tool.type", "function")))
	defer span.End()
	if t.session != "" {
		span.SetAttributes(attribute.String("gen_ai.conversation.id", t.session))
	}
	content := t.scope.Content() && span.IsRecording()
	if content {
		span.SetAttributes(attribute.String("gen_ai.tool.call.arguments", tracing.Arguments(c.Arguments)))
	}
	ctx = WithSessionID(ctx, t.session)
	if t.mcpCalls != nil {
		ctx = context.WithValue(ctx, mcpCallsKey{}, t.mcpCalls)
	}
	res := runTool(ctx, tools, t.hooks, t.resolver, c)
	if res.IsError {
		tracing.Fail(span, "tool_error")
	} else if content {
		span.SetAttributes(attribute.String("gen_ai.tool.call.result", tracing.Result(res.Text)))
	}
	return res
}

// target returns the tools and the call that c runs. A tool whose Spec panics runs c itself.
func (t runner) target(c protocol.ToolCall) (tools []Tool, call protocol.ToolCall) {
	defer func() {
		if recover() != nil {
			tools, call = t.runnable, c
		}
	}()
	if f, ok := findIn(t.runnable, c.Name).(Forwarder); ok {
		if fc, ok := f.Forward(c); ok {
			return t.deferred, fc
		}
	}
	return t.runnable, c
}

func (t runner) join(ctx context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	if t.joinMu != nil {
		t.joinMu.Lock()
		defer t.joinMu.Unlock()
	}
	if j, ok := t.hooks.(Joiner); ok {
		_, c = t.target(c)
		return j.Join(ctx, c, r)
	}
	return r
}

func (t runner) find(name string) Tool { return findIn(t.runnable, name) }

func findIn(tools []Tool, name string) Tool {
	if i := slices.IndexFunc(tools, func(x Tool) bool { return x.Spec().Name == name }); i >= 0 {
		return tools[i]
	}
	return nil
}

// alone reports whether the tool that c runs is Alone. A tool whose Spec panics is
// Alone, so that its panic reaches the result of its own call.
func (t runner) alone(c protocol.ToolCall) (alone bool) {
	defer func() {
		if recover() != nil {
			alone = true
		}
	}()
	tools, c := t.target(c)
	_, ok := findIn(tools, c.Name).(Alone)
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
	tools, c := t.target(c)
	if k, ok := findIn(tools, c.Name).(Keyed); ok {
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
func describe(ctx context.Context, call *Request, ts Toolset, to Turn, all bool, sc tracing.Scope) runner {
	deferred := ts.Deferred
	if all {
		ts.Tools, ts.Deferred = append(slices.Clip(ts.Tools), ts.Deferred...), nil
	}
	call.Tools = nil
	if ts.Specs != nil {
		call.Tools = slices.Clone(ts.Specs)
	} else {
		for _, t := range ts.Tools {
			call.Tools = append(call.Tools, t.Spec())
		}
	}
	if ts.System != nil {
		call.Instructions, call.Skills = strings.Join(ts.System, "\n\n"), ts.SkillList
		if call.SystemBlocks != nil {
			call.SystemBlocks = slices.Clone(ts.System)
		}
	}
	switch {
	case call.Instructions == "":
		call.Instructions = ts.Prompt
	case ts.Prompt != "":
		call.Instructions += "\n\n" + ts.Prompt
	}
	if !all && call.SystemBlocks != nil && ts.Prompt != "" {
		call.SystemBlocks = append(slices.Clone(call.SystemBlocks), ts.Prompt)
	}
	if i := call.SystemCacheBreakpoint; i != nil && call.SystemBlocks != nil && *i >= len(call.SystemBlocks) {
		last := len(call.SystemBlocks) - 1
		call.SystemCacheBreakpoint = nil
		if last >= 0 {
			call.SystemCacheBreakpoint = &last
		}
	}
	call.MCPInstructions = ts.MCPInstructions
	call.BaseCodeMode = ts.BaseCode
	if !all && to != nil {
		call.Pins = to.Pin(append([]Notice{{Kind: BannerKind, Text: call.Banner, At: call.BannerAt}}, ts.Notices...), len(call.History))
	}
	if all && to != nil && call.Purpose == PurposeTurn {
		call.ModeText = sentMode(to.Pin(modeNotices(ts.Notices), len(call.History)), call.TurnID)
	}
	calls, _ := ctx.Value(mcpCallsKey{}).(*atomic.Int64)
	t := runner{runnable: append(slices.Clip(ts.Tools), ts.Deferred...), deferred: deferred, resolver: ts.Resolver, hooks: ts.Hooks, scope: sc, session: call.SessionID, mcpCalls: calls}
	if all {
		t.locks = newKeyLocks()
	}
	bindNested(&t, to)
	t.codeMode = ts.CodeMode
	call.Call = t.run
	if all {
		t.joinMu = new(sync.Mutex)
		call.Call, call.Ask = t.bridged(), t.asker(to)
	}
	return t
}

// bridged returns the Call of a backend that owns the loop: the calls of one
// key run one at a time, and each result goes through the Join of the hooks,
// one result at a time across Call and Ask, so the retention of two results
// never shares a handle.
func (t runner) bridged() func(context.Context, protocol.ToolCall) protocol.ToolResult {
	run := t.keyed()
	return func(ctx context.Context, c protocol.ToolCall) protocol.ToolResult {
		return t.join(ctx, c, run(ctx, c))
	}
}

// keyLocks are the locks of one run of a backend that owns the loop. The calls of Request.Call take them, and
// so do the nested calls of a tool, so a key or an Alone tool is ordered across both paths.
type keyLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
	alone chan struct{}
}

func newKeyLocks() *keyLocks {
	return &keyLocks{locks: map[string]chan struct{}{}, alone: make(chan struct{}, 1)}
}

func (k *keyLocks) lock(ctx context.Context, key string) (release func(), err error) {
	k.mu.Lock()
	lock, ok := k.locks[key]
	if !ok {
		lock = make(chan struct{}, 1)
		k.locks[key] = lock
	}
	k.mu.Unlock()
	return take(ctx, lock)
}

func take(ctx context.Context, lock chan struct{}) (func(), error) {
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// keyed returns a run func that runs the calls of one key one at a time, in
// the order they get the key, as a batch does, and the calls of a tool that
// runs nested calls one at a time. A backend that owns the loop may run its calls at once, and
// calls to one file must not lose an edit.
func (t runner) keyed() func(context.Context, protocol.ToolCall) protocol.ToolResult {
	return func(ctx context.Context, c protocol.ToolCall) protocol.ToolResult {
		if _, target := t.target(c); t.nests[target.Name] {
			release, err := take(ctx, t.locks.alone)
			if err != nil {
				return protocol.ToolResult{IsError: true, Text: err.Error()}
			}
			defer release()
		}
		if key := t.key(c); key != "" {
			release, err := t.locks.lock(ctx, key)
			if err != nil {
				return protocol.ToolResult{IsError: true, Text: err.Error()}
			}
			defer release()
		}
		return t.run(ctx, c)
	}
}

// Restrict returns the tools that names lists, or every tool when names is nil.
func Restrict(tools []Tool, names []string) []Tool {
	if names == nil {
		return tools
	}
	return slices.DeleteFunc(slices.Clone(tools), func(t Tool) bool { return !protocol.ToolAllowed(names, t.Spec().Name) })
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
func runTool(ctx context.Context, tools []Tool, h Hooks, resolver ToolResolver, c protocol.ToolCall) protocol.ToolResult {
	if h == nil {
		return invoke(ctx, tools, resolver, c)
	}
	c, deny := h.Before(ctx, c)
	if deny != "" {
		return protocol.ToolResult{Text: deny, IsError: true}
	}
	if err := ctx.Err(); err != nil {
		return h.After(ctx, c, protocol.ToolResult{Text: err.Error(), IsError: true})
	}
	return h.After(ctx, c, invoke(ctx, tools, resolver, c))
}

func invoke(ctx context.Context, tools []Tool, resolver ToolResolver, c protocol.ToolCall) protocol.ToolResult {
	i := slices.IndexFunc(tools, func(t Tool) bool { return t.Spec().Name == c.Name })
	if i < 0 && resolver != nil {
		if t := resolver.ResolveTool(ctx, c.Name); t != nil {
			tools, i = []Tool{t}, 0
		}
	}
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
