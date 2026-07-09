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

// TestLazyRunClientAPIResolvesSessionAfterAssignment covers runCmd's actual
// wiring shape: the ClientAPI is constructed (via newLazyRunClientAPI)
// before the session it serves exists, and only resolves it lazily on each
// call through the getter — mirroring how runCmd must build the Host ahead
// of resolveSession returning the *engine.Session that fills its Options.
func TestLazyRunClientAPIResolvesSessionAfterAssignment(t *testing.T) {
	var sess *engine.Session
	api := newLazyRunClientAPI(func() *engine.Session { return sess })

	// Called before the session variable is assigned: a clean error, not a
	// nil-pointer panic.
	if _, err := api.SessionMessages(context.Background(), &plugin.SessionMessagesRequest{SessionID: "whatever"}); err == nil {
		t.Fatal("want an error before the session is assigned, got nil")
	}

	sess = engine.NewSession(engine.Config{
		Providers:    provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:      t.TempDir(),
		Instructions: &engine.InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
	if _, err := sess.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	want := sess.History()

	resp, err := api.SessionMessages(context.Background(), &plugin.SessionMessagesRequest{SessionID: sess.ID})
	if err != nil {
		t.Fatalf("SessionMessages after assignment: %v", err)
	}
	if len(resp.Messages) != len(want) {
		t.Fatalf("got %d messages, want %d", len(resp.Messages), len(want))
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
