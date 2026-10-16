package harness

import (
	"context"
	"errors"
	"fmt"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// SetPlugins makes plugins the live set of config plugins. Config.Plugins is the set at the start of the runtime,
// and the set does not persist. The plugins of Options.Plugins are no part of the set and do not change. A plugin
// whose spec is the same keeps its process; a plugin that is new or whose spec differs gets a new process that
// starts on its next use, and the old process stops; a plugin that plugins leaves out stops. It returns the plugins
// of the new set in chain order. A spec that breaks a rule of Config.Plugins, a probe or a manifest that fails, a
// plugin name that an in-process plugin has, and a tool name that a built-in tool, an embedder tool, another plugin,
// or the mcp tool has fail with ErrInvalidRequest, and change nothing. Hooks and events reach the new set from the
// next dispatch. A session keeps the plugin tools of its first tool read; the tools that a call adds are deferred.
func (r *Runtime) SetPlugins(ctx context.Context, plugins []config.PluginSpec) ([]protocol.Plugin, error) {
	if err := r.hold(); err != nil {
		return nil, err
	}
	defer r.group.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.base, cancel)()
	if err := (&config.Config{Plugins: plugins}).Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	info, err := r.plugins.Set(ctx, plugins, func(name string) bool { return r.known("", name) })
	switch {
	case err == nil:
		return info, nil
	case r.base.Err() != nil:
		return nil, ErrDraining
	case errors.Is(err, context.Canceled):
		return nil, err
	}
	return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
}
