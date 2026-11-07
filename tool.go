package harness

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// StopToolCall is a tool call and its result in a stop context.
type StopToolCall = turn.StopToolCall

// StopContext is the output of a turn through the current tool result.
type StopContext = turn.StopContext

// Tool is an embedder tool that the model can call. The runtime reads Spec
// once, at New. Run allows concurrent calls and should return when ctx ends.
// Cancellation stops waiting for Run and discards a late result, but cannot
// stop its goroutine or side effects. An error from Run becomes an error result.
type Tool interface {
	Spec() protocol.ToolSpec
	Run(ctx context.Context, call Call) (protocol.ToolResult, error)
}

// InputNormalizer is a tool that normalizes its input before execution.
// NormalizeInput must be fast and pure, with no I/O.
type InputNormalizer interface {
	NormalizeInput(context.Context, json.RawMessage) (json.RawMessage, error)
}

// Call is one tool call and the session that made it.
type Call struct {
	// ID is the call ID of the model, stable across a resumed turn.
	ID        string
	Name      string
	Arguments json.RawMessage
	// Session is the calling session, and Root the root of its session tree.
	Session, Root string
	// TurnID is the turn that made the call.
	TurnID string
}

// AloneTool is a Tool whose call runs with no other call of its model call in flight.
type AloneTool interface {
	Tool
	Alone()
}

// KeyedTool is a Tool whose calls with the same non-empty key run one at a time, in call order.
// The Call that Key receives names the session and the turn.
type KeyedTool interface {
	Tool
	Key(c Call) string
}

// ParallelTool is a Tool that changes state but whose calls may run beside the other calls of
// their model call, the way a read-only call does. Calls with the same non-empty key (KeyedTool)
// still run one at a time, in call order. Use it for a tool that writes only what its key names.
// A tool that is also an AloneTool runs alone.
type ParallelTool interface {
	Tool
	Parallel()
}

// embedded is an embedder Tool as the runtime holds it, before a session binds it.
type embedded struct{ Tool }

func (e embedded) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	return runEmbedded(ctx, e.Tool, Call{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
}

// bind returns the tool for one turn of session, with the scheduling of the embedder tool.
// An AloneTool is Alone, which wins over Parallel; a ParallelTool is Parallel.
func (e embedded) bind(session, root, turnID string) turn.Tool {
	b := bound{t: e.Tool, session: session, root: root, turn: turnID}
	_, normalizes := e.Tool.(InputNormalizer)
	if _, ok := e.Tool.(AloneTool); ok {
		if normalizes {
			return normalizingAlone{boundAlone{b}}
		}
		return boundAlone{b}
	}
	if _, ok := e.Tool.(ParallelTool); ok {
		if normalizes {
			return normalizingParallel{boundParallel{b}}
		}
		return boundParallel{b}
	}
	if normalizes {
		return normalizingBound{b}
	}
	return b
}

type bound struct {
	t                   Tool
	session, root, turn string
}

func (b bound) Spec() protocol.ToolSpec { return b.t.Spec() }

func (b bound) call(c protocol.ToolCall) Call {
	return Call{ID: c.ID, Name: c.Name, Arguments: c.Arguments, Session: b.session, Root: b.root, TurnID: b.turn}
}

func (b bound) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	return runEmbedded(ctx, b.t, b.call(c))
}

func runEmbedded(ctx context.Context, tool Tool, call Call) (protocol.ToolResult, error) {
	if ctx.Err() != nil {
		return protocol.ToolResult{}, ctx.Err()
	}
	type outcome struct {
		result protocol.ToolResult
		err    error
	}
	call.Arguments = append(json.RawMessage(nil), call.Arguments...)
	done := make(chan outcome, 1)
	go func() {
		var out outcome
		defer func() {
			if recover() != nil {
				out = outcome{err: errors.New("tool call failed: the tool panicked")}
			}
			done <- out
		}()
		out.result, out.err = tool.Run(ctx, call)
	}()
	select {
	case out := <-done:
		return out.result, out.err
	case <-ctx.Done():
		select {
		case out := <-done:
			return out.result, out.err
		default:
			return protocol.ToolResult{}, ctx.Err()
		}
	}
}

// Key is the key of the embedder tool, or empty when it is not a KeyedTool.
func (b bound) Key(c protocol.ToolCall) string {
	if k, ok := b.t.(KeyedTool); ok {
		return k.Key(b.call(c))
	}
	return ""
}

type boundAlone struct{ bound }

func (boundAlone) Alone() {}

type boundParallel struct{ bound }

func (boundParallel) Parallel() {}

type normalizingBound struct{ bound }
type normalizingAlone struct{ boundAlone }
type normalizingParallel struct{ boundParallel }

func (b normalizingBound) NormalizeInput(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return normalizeEmbedded(ctx, b.t.(InputNormalizer), input)
}
func (b normalizingAlone) NormalizeInput(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return normalizeEmbedded(ctx, b.t.(InputNormalizer), input)
}
func (b normalizingParallel) NormalizeInput(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return normalizeEmbedded(ctx, b.t.(InputNormalizer), input)
}

func normalizeEmbedded(ctx context.Context, tool InputNormalizer, input json.RawMessage) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	type outcome struct {
		input json.RawMessage
		err   error
	}
	input = append(json.RawMessage(nil), input...)
	done := make(chan outcome, 1)
	go func() {
		var out outcome
		defer func() {
			if recover() != nil {
				out = outcome{err: errors.New("tool call failed: the tool panicked")}
			}
			done <- out
		}()
		out.input, out.err = tool.NormalizeInput(ctx, input)
	}()
	select {
	case out := <-done:
		return out.input, out.err
	case <-ctx.Done():
		select {
		case out := <-done:
			return out.input, out.err
		default:
			return nil, ctx.Err()
		}
	}
}

// boundTools is a Source of the runtime tools that binds each embedder tool to the session and the turn of the model call.
type boundTools struct {
	tools         []turn.Tool
	session, root string
}

func (s boundTools) Toolset(ctx context.Context, _ []protocol.LogMessage, allowed []string, _ string) turn.Toolset {
	id := turn.ID(ctx)
	out := make([]turn.Tool, len(s.tools))
	for i, t := range s.tools {
		if e, ok := t.(embedded); ok {
			t = e.bind(s.session, s.root, id)
		}
		out[i] = t
	}
	return turn.Toolset{Tools: turn.Restrict(out, allowed)}
}
