package session

import (
	"context"
	"slices"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

func holdsCode(tools []protocol.ToolSpec) bool {
	return slices.ContainsFunc(tools, func(s protocol.ToolSpec) bool { return s.Name == turn.CodeTool })
}

// DeclaresWithout reports whether the newest declaration of the session exists and lacks the tool of that name. A
// declaration that cannot be read counts as absent, because the next model call declares again from live.
func (a *Actor) DeclaresWithout(ctx context.Context, tool string) bool {
	a.declMu.Lock()
	defer a.declMu.Unlock()
	v, err := call(ctx, a, func(reply func(declView, error)) { reply(a.declView(), nil) })
	if err != nil || !v.has {
		return false
	}
	d := v.cached
	if d == nil {
		got, err := a.readDeclaration(ctx, v.rec.BlobKey)
		if err != nil {
			return false
		}
		a.setDecl(ctx, v.rec.BlobKey, got)
		d = &got
	}
	return !slices.ContainsFunc(d.Tools, func(s protocol.ToolSpec) bool { return s.Name == tool })
}

// codeMode returns the code mode of the session now.
func (a *Actor) codeMode(ctx context.Context) bool {
	on, _ := call(ctx, a, func(reply func(bool, error)) { reply(a.state.Settings().CodeMode, nil) })
	return on
}

// baseCodeMode returns the base mode of the newest declaration.
func (a *Actor) baseCodeMode(ctx context.Context) bool {
	on, _ := call(ctx, a, func(reply func(bool, error)) {
		rec, _ := a.state.Declared()
		reply(rec.CodeMode, nil)
	})
	return on
}

// withCodeMode sets the code mode of a model call on ts: the refusal of a direct call, and the notice that states the
// mode after the history. The mode holds only while the declaration and the live tools both hold the code tool. A
// backend that changes tools inside the conversation reads the base mode from the tools array, so its notice speaks only
// when the mode differs from it.
func (a *Actor) withCodeMode(ctx context.Context, ts turn.Toolset, decl declaration, live []protocol.ToolSpec, at int, model string) turn.Toolset {
	if !holdsCode(decl.Tools) {
		ts.Notices = append(slices.Clone(ts.Notices), turn.NoCodeNotice(at))
		return ts
	}
	ts.CodeMode = holdsCode(live) && a.codeMode(ctx)
	notice := turn.CodeModeNotice(ts.CodeMode, at)
	if a.cfg.Backend.Capabilities(model).ToolChange {
		ts.BaseCode = a.baseCodeMode(ctx)
		notice = turn.ChangeNotice(ts.CodeMode, ts.BaseCode, at)
	}
	ts.Notices = append(slices.Clone(ts.Notices), notice)
	return ts
}
