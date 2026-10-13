package acp

import (
	"context"
	"encoding/json"
	"errors"
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
	h := newHeld(a.base, sess, sess.Snapshot().HeadSeq, a.cfg.Logger, a.conn.Notify)
	a.mu.Lock()
	a.sessions[h.id] = h
	a.mu.Unlock()
	return map[string]string{"sessionId": h.id}, nil
}
