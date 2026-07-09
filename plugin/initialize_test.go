package plugin

import (
	"context"
	"testing"
)

// TestInitializeParamsServeURLAndRunToken covers audit gap #6's last piece:
// InitializeParams.ServeURL/RunToken must actually reach the plugin (via
// Client.ServeURL/Client.RunToken) when a Host is configured for serve mode.
func TestInitializeParamsServeURLAndRunToken(t *testing.T) {
	var gotServeURL, gotRunToken string
	seen := false
	p := testPlugin(t, "p1", &Hooks{
		ChatParams: func(_ context.Context, c *Client, req *ChatParamsRequest) (*ChatParamsResponse, error) {
			gotServeURL = c.ServeURL()
			gotRunToken = c.RunToken()
			seen = true
			return nil, nil
		},
	})

	h := newTestHost(t, Options{ServeURL: "http://localhost:4096", RunToken: "secret-run-token"}, p)
	h.ChatParams(context.Background(), &ChatParamsRequest{SessionID: "s1"})

	if !seen {
		t.Fatal("hook never invoked")
	}
	if gotServeURL != "http://localhost:4096" {
		t.Errorf("ServeURL = %q, want http://localhost:4096", gotServeURL)
	}
	if gotRunToken != "secret-run-token" {
		t.Errorf("RunToken = %q, want secret-run-token", gotRunToken)
	}
}

// TestInitializeParamsOmittedInRunMode covers the flip side: a Host built
// without ServeURL/RunToken (as `harness run` does — there is no HTTP API to
// reach) must not leak anything into InitializeParams.
func TestInitializeParamsOmittedInRunMode(t *testing.T) {
	var gotServeURL, gotRunToken string
	seen := false
	p := testPlugin(t, "p1", &Hooks{
		ChatParams: func(_ context.Context, c *Client, req *ChatParamsRequest) (*ChatParamsResponse, error) {
			gotServeURL = c.ServeURL()
			gotRunToken = c.RunToken()
			seen = true
			return nil, nil
		},
	})

	h := newTestHost(t, Options{}, p) // run mode: no ServeURL/RunToken configured
	h.ChatParams(context.Background(), &ChatParamsRequest{SessionID: "s1"})

	if !seen {
		t.Fatal("hook never invoked")
	}
	if gotServeURL != "" {
		t.Errorf("ServeURL = %q, want empty in run mode", gotServeURL)
	}
	if gotRunToken != "" {
		t.Errorf("RunToken = %q, want empty in run mode", gotRunToken)
	}
}
