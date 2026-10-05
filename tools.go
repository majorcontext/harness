package harness

import (
	"context"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/tool/builtin"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/tool/pluginsrc"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
)

// sessionTool is a runtime tool that belongs to one session. Bind returns
// the tool for session id, or nil when that session does not get it.
type sessionTool interface {
	turn.Tool
	Bind(id string, child bool) turn.Tool
}

// known reports whether a session at model can have a tool named name: a
// tool of Options.Tools, a started plugin tool, an MCP tool, a built-in tool
// of the WorkDir that a harness loop runs, the history tool, or a built-in
// tool of the backend of model. An empty model names no backend.
func (r *Runtime) known(model, name string) bool {
	caps := r.models.Capabilities(model)
	return slices.Contains(caps.Tools, name) || name == turn.HistoryTool || !caps.OwnsLoop && r.builtin(name) ||
		r.mcp != nil && mcpsrc.Reserved(name) || r.owns(name)
}

// owns reports whether name is a tool of Options.Tools or of a started plugin.
func (r *Runtime) owns(name string) bool {
	return slices.ContainsFunc(r.tools, func(t turn.Tool) bool { return t.Spec().Name == name }) ||
		r.plugins != nil && r.plugins.Has(name)
}

// builtin reports whether name is a built-in tool of the WorkDir.
func (r *Runtime) builtin(name string) bool {
	return r.workDir != "" && (slices.Contains(builtin.Names, name) || name == toolresult.ToolName || name == sessionInfoName)
}

// source returns the tools of session id for each model call: the runtime
// tools, then the goal, file, MCP, and plugin tools of the models that take
// them. A session tool binds to the session or drops out. Each session has
// its own file tools, because the write_file guard belongs to one session,
// and its own session_info tool, which reads what sp recorded of the session.
func (r *Runtime) source(id string, child bool, plug *pluginsrc.Session, sp *sessionPrompt) turn.Source {
	var static, loopOnly []turn.Tool
	for _, t := range r.tools {
		if b, ok := t.(sessionTool); ok {
			if t = b.Bind(id, child); t == nil {
				continue
			}
		}
		if _, ok := t.(goalTool); ok {
			loopOnly = append(loopOnly, t)
			continue
		}
		static = append(static, t)
	}
	srcs := turn.Sources{turn.Fixed(static)}
	if len(loopOnly) > 0 {
		goal := turn.Fixed(loopOnly)
		srcs = append(srcs, perModel{r.models, func(c turn.Capabilities, _ []string) turn.Source {
			if c.OwnsLoop {
				return nil
			}
			return goal
		}})
	}
	if r.workDir != "" {
		files := turn.Fixed(append(builtin.Tools(r.workDir), &sessionInfoTool{r: r, session: id, prompt: sp}))
		srcs = append(srcs, perModel{r.models, func(c turn.Capabilities, _ []string) turn.Source {
			if c.OwnsLoop {
				return nil
			}
			return files
		}})
	}
	if r.mcp != nil {
		srcs = append(srcs, perModel{r.models, func(c turn.Capabilities, allowed []string) turn.Source {
			if allowed == nil && c.OwnsMCP {
				return nil
			}
			return r.mcp
		}})
	}
	if plug != nil {
		srcs = append(srcs, perModel{r.models, func(c turn.Capabilities, _ []string) turn.Source {
			if c.OwnsLoop {
				return plug.Untransformed()
			}
			return plug
		}})
	}
	return sp.recording(srcs)
}

// perModel gives each model call the Source that pick chooses for the
// capabilities of its model and its allowed tools. pick returns nil for none.
type perModel struct {
	backend turn.Backend
	pick    func(caps turn.Capabilities, allowed []string) turn.Source
}

func (p perModel) Toolset(ctx context.Context, history []eventlog.Message, allowed []string, model string) turn.Toolset {
	src := p.pick(p.backend.Capabilities(model), allowed)
	if src == nil {
		return turn.Toolset{}
	}
	return src.Toolset(ctx, history, allowed, model)
}
