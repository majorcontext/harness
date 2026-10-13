package plugin

import (
	"context"
	"testing"

	pub "github.com/majorcontext/harness/plugin"
)

// A Host built with ServeURL and RunToken passes them to the plugin through
// InitializeParams. A Host built without them (`harness run` has no HTTP API)
// leaks nothing.
func TestInitializeParamsServeURLAndRunToken(t *testing.T) {
	for name, tc := range map[string]Options{
		"serve mode": {ServeURL: "http://localhost:4096", RunToken: "secret-run-token"},
		"run mode":   {},
	} {
		t.Run(name, func(t *testing.T) {
			var gotServeURL, gotRunToken string
			var seen bool
			p := testPlugin(t, "p1", &pub.Plugin{
				ChatParams: func(_ context.Context, c *pub.Client, _ *pub.ChatParamsRequest) (*pub.ChatParamsResponse, error) {
					gotServeURL, gotRunToken, seen = c.ServeURL(), c.RunToken(), true
					return nil, nil
				},
			})
			h := newTestHost(t, tc, p)
			h.ChatParams(context.Background(), &pub.ChatParamsRequest{SessionID: "s1"})
			if !seen || gotServeURL != tc.ServeURL || gotRunToken != tc.RunToken {
				t.Errorf("hook ran = %v, ServeURL, RunToken = %q, %q, want %q, %q", seen, gotServeURL, gotRunToken, tc.ServeURL, tc.RunToken)
			}
		})
	}
}
