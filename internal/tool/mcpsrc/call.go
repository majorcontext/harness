package mcpsrc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/majorcontext/harness/protocol"
)

// Call runs tool of server with args, outside any session. It waits for the
// first connect of every server. A server that is retrying in the background
// or parked returns its reason at once and gets no attempt; only the connect
// action of the mcp tool brings a parked server back. A server that connected
// and then dropped is dialed again. A tool that reports an error returns a result
// with IsError; a failed connect or transport returns an error without the
// endpoint.
func (s *Source) Call(ctx context.Context, server, tool string, args json.RawMessage) (protocol.ToolResult, error) {
	if !s.owns(server) {
		return protocol.ToolResult{}, fmt.Errorf("mcp: server %q is not configured", server)
	}
	var in any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return protocol.ToolResult{}, fmt.Errorf("mcp: invalid arguments for %s: %w", tool, err)
		}
	}
	if !s.wait(ctx) {
		return protocol.ToolResult{}, fmt.Errorf("mcp: server %q: %w", server, context.Cause(ctx))
	}
	if !s.server(server).up() {
		if err := s.redial(ctx, server); err != nil {
			return protocol.ToolResult{}, err
		}
	}
	sv := s.server(server)
	if !sv.up() {
		return protocol.ToolResult{}, fmt.Errorf("mcp: server %q is not connected", server)
	}
	res, err := sv.client.CallTool(ctx, tool, in)
	if err != nil {
		return protocol.ToolResult{}, hide(server, err)
	}
	text, blobs := render(res.Content)
	return protocol.ToolResult{Text: text, IsError: res.IsError, Blobs: blobs}, nil
}

// redial dials a server that dropped after it connected. A server that is
// retrying or parked returns the reason of its last failed connect instead.
func (s *Source) redial(ctx context.Context, server string) error {
	s.mu.Lock()
	h := *s.health[server]
	s.mu.Unlock()
	if h.reason == "" {
		dctx, cancel := context.WithCancel(s.ctx)
		defer cancel()
		defer context.AfterFunc(ctx, cancel)()
		if err := s.connect(dctx, server); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("mcp: server %q: %w", server, context.Cause(ctx))
			}
			return fmt.Errorf("mcp: server %q: connect failed: %s", server, connectReason(err))
		}
		return nil
	}
	if h.parked {
		return fmt.Errorf("mcp: server %q is down: %s; parked until the mcp tool connects it", server, h.reason)
	}
	return fmt.Errorf("mcp: server %q is down: %s; retrying in the background", server, h.reason)
}
