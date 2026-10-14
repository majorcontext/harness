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
	after         func()
}

// AfterSent sends the updates that the pump queued, so that no live update
// comes before the answer.
func (r setupResult) AfterSent() {
	if r.after != nil {
		r.after()
	}
}

func (a *agent) loadSession(ctx context.Context, params json.RawMessage) (any, error) {
	return a.ordered(ctx, params, func() (any, error) { return a.openSession(ctx, params, true) })
}

func (a *agent) resumeSession(ctx context.Context, params json.RawMessage) (any, error) {
	return a.ordered(ctx, params, func() (any, error) { return a.openSession(ctx, params, false) })
}

// ordered runs f in the order of the requests for the session of params. Params
// with no session ID run at once, and f answers them with invalid params.
func (a *agent) ordered(ctx context.Context, params json.RawMessage, f func() (any, error)) (any, error) {
	id := sessionIDOf(params)
	if id == "" {
		return f()
	}
	return a.ticketed(ctx, id, f)
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
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" || p.CWD == "" {
		return nil, invalidParams(setupRequired(replay))
	}
	if replay && p.MCPServers == nil {
		return nil, invalidParams(setupRequired(replay))
	}
	cwd, servers, err := a.sessionTarget(p.CWD, p.MCPServers)
	if err != nil {
		return nil, err
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
	if snap.ParentID != "" {
		return nil, invalidParams("the session is a task child")
	}
	if snap.WorkDir != "" && filepath.Clean(snap.WorkDir) != cwd {
		return nil, invalidParams("the session belongs to another cwd")
	}
	if h, err := a.held(p.SessionID); err == nil {
		return a.heldSetup(ctx, h, view, cwd, len(servers) > 0, replay)
	}
	h := a.newHeld(ctx, p.SessionID, cwd, rt)
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
	var release func()
	h.send, release = holdSends(h.send)
	h.start(a.base, sess, st, snap.HeadSeq)
	started := h
	if h, err = a.adopt(ctx, h); err != nil {
		return nil, err
	}
	return setupResult{ConfigOptions: configOptions(h.models, snap), after: queueRelease(started, h, release)}, nil
}

// heldSetup answers a load or resume of a session that the connection holds.
// A load replays the conversation of view after the pump has sent every update
// through the head of view, with a pump state of its own, so the replay does
// not touch what the live pump keeps. A resume sends nothing.
// queueRelease returns release when adopted is the session whose pump queues
// the updates. When adopt returned another session, the queue of the stopped
// pump is dropped.
func queueRelease(started, adopted *held, release func()) func() {
	if adopted != started {
		return nil
	}
	return release
}

func (a *agent) heldSetup(ctx context.Context, h *held, view harness.Reader, cwd string, servers, replay bool) (any, error) {
	if h.cwd != cwd {
		return nil, invalidParams("the session belongs to another cwd")
	}
	if servers {
		return nil, invalidParams("a session that the connection holds takes no new mcpServers")
	}
	if replay {
		h.flush(ctx, view.Snapshot().HeadSeq)
		if err := h.replay(ctx, view, newPumpState(view.Snapshot())); err != nil {
			return nil, rpcError(err)
		}
	}
	return setupResult{ConfigOptions: configOptions(h.models, h.sess.Snapshot())}, nil
}

// setupRequired names the required fields of session/load, or of session/resume.
func setupRequired(load bool) string {
	if load {
		return "sessionId, cwd, and mcpServers are required"
	}
	return "sessionId and cwd are required"
}
