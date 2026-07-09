package main

import (
	"context"
	"errors"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// TestRunClientAPISessionMessages covers `harness run` mode: a single
// engine.Session held directly in-process (no server, no session store to
// speak of) still answers client/session.messages for its own id and gets
// the same canonical history back.
func TestRunClientAPISessionMessages(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	sess := engine.NewSession(engine.Config{
		Providers:    provider.Registry{"test": prov},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:      t.TempDir(),
		Instructions: &engine.InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
	if _, err := sess.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	want := sess.History()
	if len(want) == 0 {
		t.Fatal("session has no history to assert against")
	}

	api := newRunClientAPI(sess)
	resp, err := api.SessionMessages(context.Background(), &plugin.SessionMessagesRequest{SessionID: sess.ID})
	if err != nil {
		t.Fatalf("SessionMessages: %v", err)
	}
	if len(resp.Messages) != len(want) {
		t.Fatalf("got %d messages, want %d", len(resp.Messages), len(want))
	}
	for i := range want {
		if resp.Messages[i].Parts.Text() != want[i].Parts.Text() {
			t.Errorf("message %d text = %q, want %q", i, resp.Messages[i].Parts.Text(), want[i].Parts.Text())
		}
	}
}

// TestRunClientAPISessionMessagesUnknownSession verifies an id other than
// the one live session errors cleanly: run mode has exactly one session, so
// anything else is unknown.
func TestRunClientAPISessionMessagesUnknownSession(t *testing.T) {
	sess := engine.NewSession(engine.Config{
		Providers:    provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:      t.TempDir(),
		Instructions: &engine.InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
	api := newRunClientAPI(sess)
	_, err := api.SessionMessages(context.Background(), &plugin.SessionMessagesRequest{SessionID: "does-not-exist"})
	if err == nil {
		t.Fatal("want an error for an unknown session, got nil")
	}
}

// TestRunClientAPIMCPCallNotImplemented verifies MCPCall returns a clear
// not-implemented error rather than panicking or silently returning nil.
func TestRunClientAPIMCPCallNotImplemented(t *testing.T) {
	sess := engine.NewSession(engine.Config{
		Providers:    provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:      t.TempDir(),
		Instructions: &engine.InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
	api := newRunClientAPI(sess)
	_, err := api.MCPCall(context.Background(), &plugin.MCPCallRequest{Server: "gateway", Tool: "noop"})
	if err == nil {
		t.Fatal("want a not-implemented error, got nil")
	}
	if !errors.Is(err, plugin.ErrMCPNotImplemented) {
		t.Errorf("err = %v, want plugin.ErrMCPNotImplemented", err)
	}
}
