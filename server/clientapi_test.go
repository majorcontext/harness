package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// fakeMCPRegistry implements engine.MCPRegistry for tests that need to
// observe/control MCP routing without a real MCP server.
type fakeMCPRegistry struct {
	tools          []provider.ToolDef
	callTool       func(name string) (message.Parts, bool, error)
	callServerTool func(server, tool string) (message.Parts, bool, error)
}

func (f *fakeMCPRegistry) Tools(context.Context) []provider.ToolDef { return f.tools }

func (f *fakeMCPRegistry) CallTool(_ context.Context, name string, _ json.RawMessage) (message.Parts, bool, error) {
	if f.callTool == nil {
		return nil, false, nil
	}
	return f.callTool(name)
}

func (f *fakeMCPRegistry) CallServerTool(_ context.Context, server, tool string, _ json.RawMessage) (message.Parts, bool, error) {
	if f.callServerTool == nil {
		return nil, false, nil
	}
	return f.callServerTool(server, tool)
}

var _ engine.MCPRegistry = (*fakeMCPRegistry)(nil)

// TestClientAPISessionMessagesEndToEnd is the audit-gap-#6 regression test: a
// fake plugin calls client/session.messages through a real plugin.Host,
// dispatched to a real, server-backed plugin.ClientAPI (the same session
// store GET /session/{id}/message reads), and gets that session's canonical
// messages back.
func TestClientAPISessionMessagesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("hi there")}}
	srv := newServer(t, dir, prov, 0)

	sess, err := srv.opts.NewSession(message.ModelRef{Provider: "test", Model: "m1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	srv.sessions[sess.ID] = &sessionState{sess: sess}
	srv.mu.Unlock()

	if _, err := sess.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	want := sess.History()
	if len(want) == 0 {
		t.Fatal("session has no history to assert against")
	}

	var got []message.Message
	p := plugin.NewTestSpec("fetcher", &plugin.Hooks{
		ChatParams: func(ctx context.Context, c *plugin.Client, req *plugin.ChatParamsRequest) (*plugin.ChatParamsResponse, error) {
			msgs, err := c.SessionMessages(ctx, sess.ID)
			if err != nil {
				t.Errorf("SessionMessages: %v", err)
				return nil, nil
			}
			got = msgs
			return nil, nil
		},
	})
	host, err := plugin.NewHost(plugin.Options{Client: srv.ClientAPI()}, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)

	host.ChatParams(context.Background(), &plugin.ChatParamsRequest{SessionID: sess.ID})

	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Role != want[i].Role {
			t.Errorf("message %d role = %q, want %q", i, got[i].Role, want[i].Role)
		}
		if got[i].Parts.Text() != want[i].Parts.Text() {
			t.Errorf("message %d text = %q, want %q", i, got[i].Parts.Text(), want[i].Parts.Text())
		}
	}
}

// TestClientAPISessionMessagesUnknownSession verifies an unknown session id
// errors cleanly (no panic, no empty-success) rather than silently returning
// nothing.
func TestClientAPISessionMessagesUnknownSession(t *testing.T) {
	dir := t.TempDir()
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0)

	api := srv.ClientAPI()
	_, err := api.SessionMessages(context.Background(), &plugin.SessionMessagesRequest{SessionID: "does-not-exist"})
	if err == nil {
		t.Fatal("want an error for an unknown session, got nil")
	}
}

// TestClientAPIMCPCallNoServersConfigured verifies MCPCall returns a clear
// error (not a panic, not a silent empty result) when the server has no MCP
// registry configured at all.
func TestClientAPIMCPCallNoServersConfigured(t *testing.T) {
	dir := t.TempDir()
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0)

	api := srv.ClientAPI()
	_, err := api.MCPCall(context.Background(), &plugin.MCPCallRequest{Server: "gateway", Tool: "noop"})
	if err == nil {
		t.Fatal("want an error when no MCP servers are configured, got nil")
	}
}

// TestClientAPIMCPCallRoutesToRegistry verifies MCPCall reaches the
// server's shared MCP registry (Options.MCP) by explicit server+tool name.
func TestClientAPIMCPCallRoutesToRegistry(t *testing.T) {
	dir := t.TempDir()
	mgr := &fakeMCPRegistry{
		callServerTool: func(server, tool string) (message.Parts, bool, error) {
			if server != "gateway" || tool != "noop" {
				t.Errorf("callServerTool(%q, %q), want (gateway, noop)", server, tool)
			}
			return message.Parts{&message.Text{Text: "ok"}}, false, nil
		},
	}
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.MCP = mgr
	})

	api := srv.ClientAPI()
	resp, err := api.MCPCall(context.Background(), &plugin.MCPCallRequest{Server: "gateway", Tool: "noop"})
	if err != nil {
		t.Fatalf("MCPCall: %v", err)
	}
	if resp.IsError {
		t.Error("IsError = true, want false")
	}
	if resp.Content.Text() != "ok" {
		t.Errorf("Content = %q, want %q", resp.Content.Text(), "ok")
	}
}

// TestClientAPIMCPCallError verifies a registry-level error (e.g. an
// unconfigured server) propagates as a plain error, not a panic or a
// silently empty result.
func TestClientAPIMCPCallError(t *testing.T) {
	dir := t.TempDir()
	mgr := &fakeMCPRegistry{
		callServerTool: func(string, string) (message.Parts, bool, error) {
			return nil, false, fmt.Errorf("server %q is not configured", "gateway")
		},
	}
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.MCP = mgr
	})

	api := srv.ClientAPI()
	_, err := api.MCPCall(context.Background(), &plugin.MCPCallRequest{Server: "gateway", Tool: "noop"})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
}
