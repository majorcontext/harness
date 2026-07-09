package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
)

// runClientAPI implements plugin.ClientAPI for `harness run`: a single
// engine.Session held directly in-process, with no server session store
// behind it. It is the direct engine-backed adapter counterpart to the
// server package's ClientAPI (which is backed by a multi-session store) —
// see server/clientapi.go.
//
// It resolves its session through a getter rather than holding a
// *engine.Session field directly, so it can be constructed (via
// newLazyRunClientAPI) before the session exists: runCmd must set
// engine.Config.Hooks — which needs a plugin.Host, which needs this
// ClientAPI — before resolveSession returns the *engine.Session it wraps.
// newRunClientAPI (used directly by tests, where the session already
// exists) is the trivial case of the same getter.
type runClientAPI struct {
	getSess func() *engine.Session
}

// newRunClientAPI builds a plugin.ClientAPI scoped to a single, already
// in-flight run-mode session.
func newRunClientAPI(sess *engine.Session) plugin.ClientAPI {
	return &runClientAPI{getSess: func() *engine.Session { return sess }}
}

// newLazyRunClientAPI builds a plugin.ClientAPI whose session is resolved
// on every call via getSess rather than fixed at construction time. This is
// what runCmd wires into plugin.NewHost: the Host (and its Client option)
// must exist before resolveSession has produced the *engine.Session it
// ultimately serves, so getSess reads a variable that runCmd assigns
// immediately after resolveSession returns — strictly before the first
// Prompt/PursueGoal call, which is the earliest point any hook (and so any
// plugin client API call) can fire. A nil session (called out of that
// order, which should never happen in practice) is reported as a clean
// error rather than a nil-pointer panic.
func newLazyRunClientAPI(getSess func() *engine.Session) plugin.ClientAPI {
	return &runClientAPI{getSess: getSess}
}

// SessionMessages implements plugin.ClientAPI. Run mode holds exactly one
// session, so anything but that session's own id is unknown.
func (r *runClientAPI) SessionMessages(_ context.Context, req *plugin.SessionMessagesRequest) (*plugin.SessionMessagesResponse, error) {
	sess := r.getSess()
	if sess == nil {
		return nil, fmt.Errorf("client API: no active session yet")
	}
	if req.SessionID != sess.ID {
		return nil, fmt.Errorf("client API: no such session %q", req.SessionID)
	}
	msgs := sess.History()
	if msgs == nil {
		msgs = []message.Message{}
	}
	return &plugin.SessionMessagesResponse{Messages: msgs}, nil
}

// MCPCall implements plugin.ClientAPI. See plugin.ErrMCPNotImplemented.
func (r *runClientAPI) MCPCall(_ context.Context, _ *plugin.MCPCallRequest) (*plugin.MCPCallResult, error) {
	return nil, plugin.ErrMCPNotImplemented
}

// Generate implements plugin.ClientAPI. See plugin.ErrGenerateNotImplemented.
func (r *runClientAPI) Generate(_ context.Context, _ *plugin.GenerateRequest) (*plugin.GenerateResponse, error) {
	return nil, plugin.ErrGenerateNotImplemented
}

// lateClientAPI is a plugin.ClientAPI that delegates to an implementation
// bound after host construction. The plugin host must be built before the
// engine session (run) or server (serve) that backs the real ClientAPI
// exists, so the host gets this indirection and Bind supplies the real
// implementation once it is available. Calls before Bind fail cleanly.
type lateClientAPI struct {
	impl atomic.Pointer[plugin.ClientAPI]
}

func newLateClientAPI() *lateClientAPI { return &lateClientAPI{} }

// Bind installs the real implementation. Safe to call once, before any
// plugin traffic that depends on it.
func (l *lateClientAPI) Bind(api plugin.ClientAPI) { l.impl.Store(&api) }

func (l *lateClientAPI) get() (plugin.ClientAPI, error) {
	if p := l.impl.Load(); p != nil {
		return *p, nil
	}
	return nil, fmt.Errorf("client API: not ready (host still starting)")
}

func (l *lateClientAPI) SessionMessages(ctx context.Context, req *plugin.SessionMessagesRequest) (*plugin.SessionMessagesResponse, error) {
	api, err := l.get()
	if err != nil {
		return nil, err
	}
	return api.SessionMessages(ctx, req)
}

func (l *lateClientAPI) MCPCall(ctx context.Context, req *plugin.MCPCallRequest) (*plugin.MCPCallResult, error) {
	api, err := l.get()
	if err != nil {
		return nil, err
	}
	return api.MCPCall(ctx, req)
}

func (l *lateClientAPI) Generate(ctx context.Context, req *plugin.GenerateRequest) (*plugin.GenerateResponse, error) {
	api, err := l.get()
	if err != nil {
		return nil, err
	}
	return api.Generate(ctx, req)
}
