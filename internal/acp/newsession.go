package acp

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
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
	cwd, servers, err := a.sessionTarget(p.CWD, p.MCPServers)
	if err != nil {
		return nil, err
	}
	if err := a.requireLogin(ctx, cwd, "", false); err != nil {
		return nil, err
	}
	rt, err := a.runtime(ctx, cwd)
	if err != nil {
		return nil, err
	}
	var opts []harness.SessionOption
	if len(servers) > 0 {
		opts = append(opts, harness.WithSessionMCP(servers))
	}
	sess, err := rt.Create(ctx, protocol.CreateSession{Origin: origin, AllowedTools: a.cfg.AllowedTools}, opts...)
	if err != nil {
		return nil, rpcError(err)
	}
	snap := sess.Snapshot()
	h := a.newHeld(ctx, sess.ID(), cwd, rt)
	h.start(a.base, sess, newPumpState(snap), snap.HeadSeq)
	if _, err := a.adopt(ctx, h); err != nil {
		return nil, err
	}
	return sessionResult{SessionID: h.id, ConfigOptions: configOptions(h.models, snap), after: func() { a.sendCommands(h, rt) }}, nil
}

// sessionTarget checks the cwd and the mcpServers of a request that opens a
// session, and returns the cleaned cwd and the servers.
func (a *agent) sessionTarget(cwd string, mcpServers []json.RawMessage) (string, map[string]config.MCPServerSpec, error) {
	if !filepath.IsAbs(cwd) {
		return "", nil, invalidParams("cwd must be an absolute path")
	}
	servers, err := sessionServers(mcpServers)
	if err != nil {
		return "", nil, err
	}
	if len(servers) > 0 && !a.cfg.MCP {
		return "", nil, invalidParams("mcp unavailable on this host")
	}
	return filepath.Clean(cwd), servers, nil
}

// runtime returns the Runtime of cwd.
func (a *agent) runtime(ctx context.Context, cwd string) (Runtime, error) {
	if a.isClosed() {
		return nil, rpcError(harness.ErrSessionReleased)
	}
	if a.rts == nil {
		return nil, rpcError(errNoRuntimes)
	}
	rt, err := a.rts.For(ctx, cwd)
	if err != nil {
		return nil, rpcError(err)
	}
	return rt, nil
}

// newHeld returns a held session of the work directory cwd, served by rt.
func (a *agent) newHeld(ctx context.Context, id, cwd string, rt Runtime) *held {
	return newHeld(id, heldConfig{cwd: cwd, models: rt.Models(ctx), notices: a.notices.Load(), log: a.cfg.Logger, send: a.conn.Notify,
		toolImage: func(ctx context.Context, key string) (io.ReadCloser, error) { return a.rts.ToolImage(ctx, id, key) }})
}

// adopt registers h, whose pump runs. It returns the held session that the
// connection already had for the ID, when there is one, and then stops h. When
// the connection has ended it stops h, releases the session, and fails.
func (a *agent) adopt(ctx context.Context, h *held) (*held, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		h.close()
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBudget)
		defer cancel()
		if err := h.sess.Release(bg); err != nil {
			a.cfg.Logger.Warn("acp: release after shutdown", "session", h.id, "error", err.Error())
		}
		return nil, rpcError(harness.ErrSessionReleased)
	}
	if existing, ok := a.sessions[h.id]; ok {
		a.mu.Unlock()
		h.close()
		return existing, nil
	}
	a.sessions[h.id] = h
	a.mu.Unlock()
	return h, nil
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
