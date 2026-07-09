package server

import (
	"context"
	"fmt"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
)

// clientAPI implements plugin.ClientAPI backed by this Server's own session
// store — the exact same lookup GET /session/{id}/message uses (see
// Server.lookup / handleMessages): a resident in-memory session if one is
// running or idle in this process, else a transparent load from the
// on-disk log. session.messages therefore returns the canonical message
// list for any session this server process owns, live or reloaded.
type clientAPI struct {
	srv *Server
}

// ClientAPI returns a plugin.ClientAPI backed by this server's session
// store, suitable for plugin.Options.Client when running `harness serve`.
func (s *Server) ClientAPI() plugin.ClientAPI {
	return &clientAPI{srv: s}
}

// SessionMessages implements plugin.ClientAPI.
func (c *clientAPI) SessionMessages(_ context.Context, req *plugin.SessionMessagesRequest) (*plugin.SessionMessagesResponse, error) {
	sess, _, ok := c.srv.lookup(req.SessionID)
	if !ok {
		return nil, fmt.Errorf("client API: no such session %q", req.SessionID)
	}
	msgs := sess.History()
	if msgs == nil {
		msgs = []message.Message{}
	}
	return &plugin.SessionMessagesResponse{Messages: msgs}, nil
}

// MCPCall implements plugin.ClientAPI. MCP engine integration (routing a
// plugin's call through this server's configured MCP client servers) is a
// separate PR; until then this returns a clear, typed error rather than
// panicking or silently returning an empty result.
func (c *clientAPI) MCPCall(_ context.Context, _ *plugin.MCPCallRequest) (*plugin.MCPCallResult, error) {
	return nil, plugin.ErrMCPNotImplemented
}

// Generate implements plugin.ClientAPI. Routing plugin-initiated LLM calls
// through the provider layer is not yet wired (a separate PR, like
// MCPCall); this returns a clear, typed error instead of panicking or
// silently returning an empty message.
func (c *clientAPI) Generate(_ context.Context, _ *plugin.GenerateRequest) (*plugin.GenerateResponse, error) {
	return nil, plugin.ErrGenerateNotImplemented
}
