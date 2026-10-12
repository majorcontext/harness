package harness

import (
	"context"
	"encoding/json"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// Tool is an embedder tool that the model can call. The runtime reads Spec
// once, at New. Run allows concurrent calls and must return when ctx ends. An
// error from Run becomes an error result.
type Tool interface {
	Spec() protocol.ToolSpec
	Run(ctx context.Context, call Call) (protocol.ToolResult, error)
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

// embedded is an embedder Tool as the runtime holds it, before a session binds it.
type embedded struct{ Tool }

func (e embedded) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	return e.Tool.Run(ctx, Call{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
}

// bind returns the tool for one turn of session, with the Alone and Key of the embedder tool.
func (e embedded) bind(session, root, turnID string) turn.Tool {
	b := bound{t: e.Tool, session: session, root: root, turn: turnID}
	_, alone := e.Tool.(AloneTool)
	_, keyed := e.Tool.(KeyedTool)
	switch {
	case alone && keyed:
		return boundAloneKeyed{boundKeyed{b}}
	case alone:
		return boundAlone{b}
	case keyed:
		return boundKeyed{b}
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
	return b.t.Run(ctx, b.call(c))
}

type boundAlone struct{ bound }

func (boundAlone) Alone() {}

type boundKeyed struct{ bound }

func (b boundKeyed) Key(c protocol.ToolCall) string { return b.t.(KeyedTool).Key(b.call(c)) }

type boundAloneKeyed struct{ boundKeyed }

func (boundAloneKeyed) Alone() {}

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
