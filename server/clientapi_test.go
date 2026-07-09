package server

import (
	"context"
	"errors"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

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

// TestClientAPIMCPCallNotImplemented verifies MCPCall returns a clear
// not-implemented error instead of panicking or returning a silent nil
// result.
func TestClientAPIMCPCallNotImplemented(t *testing.T) {
	dir := t.TempDir()
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0)

	api := srv.ClientAPI()
	_, err := api.MCPCall(context.Background(), &plugin.MCPCallRequest{Server: "gateway", Tool: "noop"})
	if err == nil {
		t.Fatal("want a not-implemented error, got nil")
	}
	if !errors.Is(err, plugin.ErrMCPNotImplemented) {
		t.Errorf("err = %v, want plugin.ErrMCPNotImplemented", err)
	}
}
