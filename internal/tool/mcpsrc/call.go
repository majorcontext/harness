package mcpsrc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

func (s *Source) sessionHeader(ctx context.Context, server string) context.Context {
	if m := s.view().mem[server]; m != nil && m.sessionIDHeader {
		if id := turn.SessionID(ctx); id != "" {
			return mcp.WithRequestHeader(ctx, mcp.HarnessSessionIDHeader, id)
		}
	}
	return ctx
}

// Call runs tool of server with args, outside any session. It waits for the
// first connect of every server. A server that is retrying in the background
// or parked returns its reason at once and gets no attempt; only the connect
// action of the mcp tool brings a parked server back. A server that connected
// and then dropped is dialed again. A tool that reports an error returns a result
// with IsError; a failed connect or transport returns an error without the
// endpoint.
func (s *Source) Call(ctx context.Context, server, tool string, args json.RawMessage) (protocol.ToolResult, error) {
	owner := s.owner(server)
	if owner == nil {
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
	if sv, _ := owner.serverNow(server); !sv.up() {
		if err := owner.redial(ctx, server); err != nil {
			return protocol.ToolResult{}, err
		}
	}
	sv, life := owner.serverNow(server)
	if !sv.up() {
		return protocol.ToolResult{}, fmt.Errorf("mcp: server %q is not connected", server)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(life, cancel)()
	return toolresult.Limit(ctx, prefix+server+"__"+tool, sv.limit, func(ctx context.Context) (protocol.ToolResult, error) {
		ctx = s.sessionHeader(ctx, server)
		res, err := sv.client.CallTool(ctx, tool, in)
		if err != nil {
			return protocol.ToolResult{}, hide(server, err)
		}
		text, blobs := render(res.Content)
		return protocol.ToolResult{Text: text, IsError: res.IsError, Blobs: blobs}, nil
	})
}

// redial dials a server that nothing has tried. A server that is retrying or parked returns the reason of its
// last failed connect instead.
func (s *Source) redial(ctx context.Context, server string) error {
	h := s.snap.Load().health[server]
	if h.reason == "" {
		if _, err := s.connectNow(ctx, server); err != nil {
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
