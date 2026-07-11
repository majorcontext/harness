package main

import (
	"context"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/process"
)

// processCloseTimeout bounds shutting down every managed process on exit,
// mirroring mcpCloseTimeout's role for MCP server connections.
const processCloseTimeout = 10 * time.Second

// buildProcessManager converts a config's processes section into a
// *process.Manager, built once per harness process and shared across
// every session (run mode's one session, or every session `harness serve`
// hosts) via engine.Config.Processes / server.Options.Processes.
//
// alwaysOn selects whether an EMPTY (or nil) processes map should still
// produce a non-nil Manager: `harness run` keeps the zero-cost-when-unused
// rule (nil when unconfigured, like buildMCPManager), but `harness serve`
// always passes alwaysOn=true so the `process` tool (and the /process HTTP
// endpoints) are present even on a box with no processes declared yet — a
// runtime `declare` call, or a future config reload, has something to
// register against. See docs/design/managed-processes.md for the
// tradeoff.
func buildProcessManager(workDir string, processes map[string]config.ProcessSpec, alwaysOn bool) *process.Manager {
	if len(processes) == 0 && !alwaysOn {
		return nil
	}
	defs := make(map[string]process.Def, len(processes))
	for name, spec := range processes {
		defs[name] = process.Def{
			Command:      spec.Command,
			Dir:          spec.Dir,
			Env:          spec.Env,
			ReadyRegex:   spec.ReadyRegex,
			ReadyTimeout: time.Duration(spec.ReadyTimeoutS) * time.Second,
		}
	}
	return process.NewManager(workDir, defs)
}

// processRegistry adapts a possibly-nil *process.Manager to
// engine.ProcessRegistry, the same typed-nil guard mcpRegistry applies to
// *engine.MCPManager: assigning a typed-nil *process.Manager directly to
// an engine.ProcessRegistry-typed field would produce a non-nil interface.
func processRegistry(mgr *process.Manager) engine.ProcessRegistry {
	if mgr == nil {
		return nil
	}
	return mgr
}

// closeProcessManager stops every currently-active managed process (a
// no-op if mgr is nil), bounded by processCloseTimeout.
func closeProcessManager(mgr *process.Manager) {
	if mgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), processCloseTimeout)
	defer cancel()
	mgr.Close(ctx)
}
