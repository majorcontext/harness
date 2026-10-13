package acp

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/majorcontext/harness"
)

// setupResult is the answer of session/load and session/resume.
type setupResult struct {
	ConfigOptions []configOption `json:"configOptions"`
}

func (a *agent) loadSession(ctx context.Context, params json.RawMessage) (any, error) {
	return a.openSession(ctx, params, true)
}

func (a *agent) resumeSession(ctx context.Context, params json.RawMessage) (any, error) {
	return a.openSession(ctx, params, false)
}

// openSession serves session/load, which replays the conversation, and
// session/resume, which does not. Both read one view of the session, whose head
// is the boundary where the pump starts, then open the session.
func (a *agent) openSession(ctx context.Context, params json.RawMessage, replay bool) (any, error) {
	var p struct {
		SessionID  string            `json:"sessionId"`
		CWD        string            `json:"cwd"`
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" || p.CWD == "" || (replay && p.MCPServers == nil) {
		return nil, invalidParams("sessionId, cwd, and mcpServers are required")
	}
	cwd, servers, err := a.sessionTarget(p.CWD, p.MCPServers)
	if err != nil {
		return nil, err
	}
	if h, err := a.held(p.SessionID); err == nil {
		if h.cwd != cwd {
			return nil, invalidParams("the session belongs to another cwd")
		}
		return setupResult{ConfigOptions: configOptions(h.models, h.sess.Snapshot())}, nil
	}
	rt, err := a.runtime(ctx, cwd)
	if err != nil {
		return nil, err
	}
	view, err := rt.Read(ctx, p.SessionID)
	if err != nil {
		return nil, rpcError(err)
	}
	snap := view.Snapshot()
	if snap.WorkDir != "" && filepath.Clean(snap.WorkDir) != cwd {
		return nil, invalidParams("the session belongs to another cwd")
	}
	h := a.newHeld(p.SessionID, cwd, rt)
	st := newPumpState(snap)
	if replay {
		err = h.replay(ctx, view, st)
	} else {
		err = h.seedDiffs(ctx, view, st)
	}
	if err != nil {
		return nil, rpcError(err)
	}
	a.sendCommands(h, rt)
	var opts []harness.SessionOption
	if len(servers) > 0 {
		opts = append(opts, harness.WithSessionMCP(servers))
	}
	sess, err := rt.Open(ctx, p.SessionID, opts...)
	if err != nil {
		return nil, rpcError(err)
	}
	h.start(a.base, sess, st, snap.HeadSeq)
	if h, err = a.adopt(ctx, h); err != nil {
		return nil, err
	}
	return setupResult{ConfigOptions: configOptions(h.models, snap)}, nil
}
