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
type runClientAPI struct {
	sess *engine.Session
}

// newRunClientAPI builds a plugin.ClientAPI scoped to a single, already
// in-flight run-mode session.
func newRunClientAPI(sess *engine.Session) plugin.ClientAPI {
	return &runClientAPI{sess: sess}
}

// SessionMessages implements plugin.ClientAPI. Run mode holds exactly one
// session, so anything but that session's own id is unknown.
func (r *runClientAPI) SessionMessages(_ context.Context, req *plugin.SessionMessagesRequest) (*plugin.SessionMessagesResponse, error) {
	if req.SessionID != r.sess.ID {
		return nil, fmt.Errorf("client API: no such session %q", req.SessionID)
	}
	msgs := r.sess.History()
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
