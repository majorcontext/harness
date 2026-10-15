package harness

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"

	iplugin "github.com/majorcontext/harness/internal/plugin"
	"github.com/majorcontext/harness/internal/selftool"
	"github.com/majorcontext/harness/internal/tool/builtin"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/tool/pluginsrc"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
)

// sessionTool is a runtime tool that belongs to one session. Bind returns
// the tool for session id at depth, or nil when that session does not get it.
type sessionTool interface {
	turn.Tool
	Bind(id string, child bool, depth int) turn.Tool
}

// known reports whether a session at model can have a tool named name: a
// tool of Options.Tools, a started plugin tool, an MCP tool, a built-in tool
// of the WorkDir that a harness loop runs, the history tool, or a built-in
// tool of the backend of model. An empty model names no backend.
func (r *Runtime) known(model, name string) bool { return r.knownIn(model, name, false) }

// knownIn is known for a session that has session MCP servers when session is true.
func (r *Runtime) knownIn(model, name string, session bool) bool {
	caps := r.models.Capabilities(model)
	return slices.Contains(caps.Tools, name) || name == turn.HistoryTool || r.builtin(name) || !caps.OwnsLoop && name == turn.AskTool ||
		(r.mcp != nil || session) && mcpsrc.Reserved(name) || r.owns(name)
}

// owns reports whether name is a tool of Options.Tools or of a started plugin.
func (r *Runtime) owns(name string) bool {
	return slices.ContainsFunc(r.tools, func(t turn.Tool) bool { return t.Spec().Name == name }) ||
		r.plugins != nil && r.plugins.Has(name)
}

// builtin reports whether name is a built-in tool of the WorkDir.
func (r *Runtime) builtin(name string) bool {
	return r.workDir != "" && (slices.Contains(builtin.Names, name) || name == toolresult.ToolName || name == selftool.SessionInfoName ||
		r.web != nil && name == turn.WebFetchTool)
}

// shadowable reports whether name is a file, search, or shell tool of the
// WorkDir, which a tool of Options.Tools replaces.
func (r *Runtime) shadowable(name string) bool {
	return r.workDir != "" && slices.Contains(builtin.Names, name)
}

// shadowed returns the shadowable built-in names that Options.Tools takes.
func (r *Runtime) shadowed() []string {
	var out []string
	for _, t := range r.tools {
		if n := t.Spec().Name; r.shadowable(n) {
			out = append(out, n)
		}
	}
	return out
}

// source returns the tools of session id for each model call: the runtime
// tools, then the goal, file, MCP, and plugin tools of the models that take
// them. A session tool binds to the session or drops out. Each session has
// its own file tools, because the write_file guard belongs to one session,
// and its own session_info tool, which reads what sp recorded of the session.
func (r *Runtime) source(id, root string, child bool, depth int, plug *pluginsrc.Session, sp *selftool.Prompt, sm *mcpsrc.Source) turn.Source {
	var static, loopOnly []turn.Tool
	var model *selftool.Model
	for _, t := range r.tools {
		if b, ok := t.(sessionTool); ok {
			if t = b.Bind(id, child, depth); t == nil {
				continue
			}
		}
		if m, ok := t.(selftool.Model); ok {
			model = &m
			continue
		}
		if _, ok := t.(selftool.Goal); ok {
			loopOnly = append(loopOnly, t)
			continue
		}
		static = append(static, t)
	}
	srcs := turn.Sources{boundTools{tools: static, session: id, root: root}}
	if len(loopOnly) > 0 {
		goal := turn.Fixed(loopOnly)
		srcs = append(srcs, perModel{r.models, func(c turn.Capabilities, _ []string) turn.Source {
			if c.OwnsLoop {
				return nil
			}
			return goal
		}})
	}
	if model != nil {
		full, listed := turn.Fixed{*model}, turn.Fixed{model.ListOnly()}
		srcs = append(srcs, perModel{r.models, func(c turn.Capabilities, _ []string) turn.Source {
			if c.OwnsLoop {
				return listed
			}
			return full
		}})
	}
	if r.workDir != "" {
		var env builtin.ShellEnv
		if plug != nil {
			env = plug.ShellEnv
		}
		taken := r.shadowed()
		files := turn.Fixed(append(slices.DeleteFunc(builtin.Tools(r.workDir, env), func(t turn.Tool) bool { return slices.Contains(taken, t.Spec().Name) }),
			selftool.NewSessionInfo(toolHost{r}, id, sp)))
		if r.web != nil {
			files = append(files, newWebTool(*r.web))
		}
		srcs = append(srcs, files)
	}
	if mcp := cmp.Or(sm, r.mcp); mcp != nil {
		srcs = append(srcs, perModel{r.models, func(c turn.Capabilities, allowed []string) turn.Source {
			if allowed == nil && c.OwnsMCP {
				return nil
			}
			return mcp
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
	return srcs
}

// perModel gives each model call the Source that pick chooses for the
// capabilities of its model and its allowed tools. pick returns nil for none.
type perModel struct {
	backend turn.Backend
	pick    func(caps turn.Capabilities, allowed []string) turn.Source
}

func (p perModel) Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, model string) turn.Toolset {
	src := p.pick(p.backend.Capabilities(model), allowed)
	if src == nil {
		return turn.Toolset{}
	}
	return src.Toolset(ctx, history, allowed, model)
}

// checkPlugins fails on a nil plugin, a plugin with no name, a
// ToolExecuteBefore with no Run, a tool with no Execute, a name that repeats
// one of cfg or of an earlier plugin, and a plugin tool that takes the name of
// a tool the runtime has or of an earlier plugin.
func (r *Runtime) checkPlugins(cfg []config.PluginSpec, plugins []*plugin.Plugin) error {
	names, tools := map[string]bool{}, map[string]bool{}
	for _, c := range cfg {
		names[c.Name] = true
	}
	for i, p := range plugins {
		switch {
		case p == nil:
			return fmt.Errorf("harness: plugin %d is nil", i)
		case p.Name == "":
			return fmt.Errorf("harness: plugin %d has no name", i)
		case names[p.Name]:
			return fmt.Errorf("harness: plugin name %q is repeated", p.Name)
		}
		names[p.Name] = true
		if b := p.ToolExecuteBefore; b != nil && b.Run == nil {
			return fmt.Errorf("harness: plugin %s: ToolExecuteBefore has no Run", p.Name)
		}
		if err := checkHookTimeouts(p); err != nil {
			return err
		}
		for _, t := range p.Tools {
			if t.Execute == nil {
				return fmt.Errorf("harness: plugin %s: tool %q has no Execute", p.Name, t.Def.Name)
			}
		}
		for _, d := range p.Manifest().Tools {
			if d.Name == "" || tools[d.Name] || r.known("", d.Name) {
				return fmt.Errorf("harness: plugin %s: tool name %q is empty or taken", p.Name, d.Name)
			}
			tools[d.Name] = true
		}
	}
	return nil
}

// checkHookTimeouts validates the HookTimeouts of p and names the Duration that
// the author wrote when one is invalid.
func checkHookTimeouts(p *plugin.Plugin) error {
	m := p.Manifest()
	for _, h := range slices.Sorted(maps.Keys(m.HookTimeouts)) {
		one := m
		one.HookTimeouts = map[plugin.Hook]int{h: m.HookTimeouts[h]}
		if err := iplugin.ValidateHookTimeouts(one); err != nil {
			return fmt.Errorf("harness: %w (HookTimeouts[%s] is %v)", err, h, p.HookTimeouts[h])
		}
	}
	return nil
}
