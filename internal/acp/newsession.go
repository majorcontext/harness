package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// origin is the Origin of the sessions that session/new creates.
const origin = "acp"

func (a *agent) newSession(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		CWD        string            `json:"cwd"`
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.MCPServers == nil {
		return nil, invalidParams("cwd and mcpServers are required")
	}
	if !filepath.IsAbs(p.CWD) {
		return nil, invalidParams("cwd must be an absolute path")
	}
	servers, err := sessionServers(p.MCPServers)
	if err != nil {
		return nil, err
	}
	if len(servers) > 0 && !a.cfg.MCP {
		return nil, invalidParams("mcp unavailable on this host")
	}
	if a.isClosed() {
		return nil, rpcError(harness.ErrSessionReleased)
	}
	if a.rts == nil {
		return nil, rpcError(errors.New("acp: no runtimes"))
	}
	rt, err := a.rts.For(ctx, filepath.Clean(p.CWD))
	if err != nil {
		return nil, rpcError(err)
	}
	var opts []harness.SessionOption
	if len(servers) > 0 {
		opts = append(opts, harness.WithSessionMCP(servers))
	}
	sess, err := rt.Create(ctx, protocol.CreateSession{Origin: origin, AllowedTools: a.cfg.AllowedTools}, opts...)
	if err != nil {
		return nil, rpcError(err)
	}
	id := sess.ID()
	snap := sess.Snapshot()
	models := rt.Models()
	h := newHeld(a.base, sess, heldConfig{cwd: filepath.Clean(p.CWD), snap: snap, models: models, notices: a.notices.Load(), log: a.cfg.Logger, send: a.conn.Notify,
		toolImage: func(ctx context.Context, key string) (io.ReadCloser, error) { return a.rts.ToolImage(ctx, id, key) }})
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		h.close()
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBudget)
		defer cancel()
		if err := sess.Release(bg); err != nil {
			a.cfg.Logger.Warn("acp: release after shutdown", "session", h.id, "error", err.Error())
		}
		return nil, rpcError(harness.ErrSessionReleased)
	}
	a.sessions[h.id] = h
	a.mu.Unlock()
	return sessionResult{SessionID: h.id, ConfigOptions: configOptions(models, snap), after: func() { a.sendCommands(h, rt) }}, nil
}

// sendCommands sends the available commands of the work directory of rt.
func (a *agent) sendCommands(h *held, rt Runtime) {
	cmds, err := rt.Commands(a.base)
	if err != nil {
		a.cfg.Logger.Warn("acp: commands", "session", h.id, "error", err.Error())
		return
	}
	_ = h.update(map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": availableCommands(cmds)})
}

func (a *agent) isClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}
