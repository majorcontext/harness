package plugin_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/internal/plugin/wire"
	"github.com/majorcontext/harness/plugin"
)

func TestServeConnAnswersAPanicWithAnError(t *testing.T) {
	p := &plugin.Plugin{Name: "boom", ShellEnv: func(context.Context, *plugin.Client, *plugin.ShellEnvRequest) (*plugin.ShellEnvResponse, error) {
		panic("kaboom")
	}}
	host, side := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- plugin.ServeConn(side, p) }()
	c := wire.NewConn(host, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
	go c.Run() //nolint:errcheck
	err := c.Call(context.Background(), wire.HookPrefix+string(plugin.HookShellEnv), &plugin.ShellEnvRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "panic: kaboom") {
		t.Fatalf("error = %v, want panic: kaboom", err)
	}
	if err := c.Call(context.Background(), wire.MethodInitialize, &plugin.InitializeParams{ProtocolVersion: plugin.ProtocolVersion}, nil); err != nil {
		t.Fatalf("initialize after a panic: %v", err)
	}
	_ = c.Notify(wire.MethodShutdown, struct{}{})
	if err := <-done; err != nil {
		t.Errorf("ServeConn = %v, want nil after shutdown", err)
	}
}

func TestClientHTTPClientAddsHeadersAndKeepsTheRequestOnes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get("X-A") + "," + r.Header.Get("X-B")))
	}))
	defer srv.Close()
	p := &plugin.Plugin{Name: "http", SystemTransform: func(ctx context.Context, c *plugin.Client, _ *plugin.SystemTransformRequest) (*plugin.SystemTransformResponse, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		req.Header.Set("X-B", "mine")
		resp, err := c.HTTPClient().Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return &plugin.SystemTransformResponse{Segments: []string{string(body), req.Header.Get("X-A")}}, nil
	}}
	host, side := net.Pipe()
	go plugin.ServeConn(side, p) //nolint:errcheck
	c := wire.NewConn(host, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
	go c.Run() //nolint:errcheck
	defer func() { _ = c.Close() }()
	init := &plugin.InitializeParams{ProtocolVersion: plugin.ProtocolVersion, HTTPHeaders: map[string]string{"X-A": "a", "X-B": "harness"}}
	if err := c.Call(context.Background(), wire.MethodInitialize, init, nil); err != nil {
		t.Fatal(err)
	}
	var resp plugin.SystemTransformResponse
	if err := c.Call(context.Background(), wire.HookPrefix+string(plugin.HookSystemTransform), &plugin.SystemTransformRequest{}, &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Segments; len(got) != 2 || got[0] != "a,mine" || got[1] != "" {
		t.Errorf("segments = %q, want [a,mine and an unchanged caller request]", got)
	}
}

func TestHTTPClientStampsHeadersOnlyOnTheFirstHost(t *testing.T) {
	var self *httptest.Server
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/back" {
			http.Redirect(w, r, self.URL+"/end", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("other:" + r.Header.Get("X-A")))
	}))
	defer other.Close()
	self = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cross":
			http.Redirect(w, r, other.URL, http.StatusFound)
		case "/bounce":
			http.Redirect(w, r, other.URL+"/back", http.StatusFound)
		case "/same":
			http.Redirect(w, r, "/end", http.StatusFound)
		default:
			_, _ = w.Write([]byte("self:" + r.Header.Get("X-A")))
		}
	}))
	defer self.Close()
	p := &plugin.Plugin{Name: "redir", SystemTransform: func(_ context.Context, c *plugin.Client, req *plugin.SystemTransformRequest) (*plugin.SystemTransformResponse, error) {
		var resp *http.Response
		var err error
		if u, ok := strings.CutPrefix(req.SessionID, "scheme:"); ok {
			firstHop, _ := http.NewRequest(http.MethodGet, strings.Replace(u, "http://", "https://", 1), nil)
			hop, _ := http.NewRequest(http.MethodGet, u, nil)
			hop.Response = &http.Response{Request: firstHop}
			resp, err = c.HTTPClient().Transport.RoundTrip(hop)
		} else {
			resp, err = c.HTTPClient().Get(req.SessionID)
		}
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return &plugin.SystemTransformResponse{Segments: []string{string(body)}}, nil
	}}
	host, side := net.Pipe()
	go plugin.ServeConn(side, p) //nolint:errcheck
	c := wire.NewConn(host, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
	go c.Run() //nolint:errcheck
	defer func() { _ = c.Close() }()
	init := &plugin.InitializeParams{ProtocolVersion: plugin.ProtocolVersion, HTTPHeaders: map[string]string{"X-A": "a"}}
	if err := c.Call(context.Background(), wire.MethodInitialize, init, nil); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/cross": "other:", "/same": "self:a", "/bounce": "self:", "scheme:": "self:"} {
		var resp plugin.SystemTransformResponse
		target := self.URL + path
		if path == "scheme:" {
			target = "scheme:" + self.URL
		}
		if err := c.Call(context.Background(), wire.HookPrefix+string(plugin.HookSystemTransform), &plugin.SystemTransformRequest{SessionID: target}, &resp); err != nil {
			t.Fatal(err)
		}
		if got := resp.Segments; len(got) != 1 || got[0] != want {
			t.Errorf("%s: segments = %q, want %q", path, got, want)
		}
	}
}

func TestServeConnEndsTheContextOfAHookAtItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wait := func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}
		p := &plugin.Plugin{
			Name: "deadline",
			ShellEnv: func(ctx context.Context, _ *plugin.Client, _ *plugin.ShellEnvRequest) (*plugin.ShellEnvResponse, error) {
				return nil, wait(ctx)
			},
			SystemTransform: func(ctx context.Context, _ *plugin.Client, _ *plugin.SystemTransformRequest) (*plugin.SystemTransformResponse, error) {
				return nil, wait(ctx)
			},
			HookTimeouts: map[plugin.Hook]time.Duration{plugin.HookShellEnv: 100 * time.Millisecond},
		}
		host, side := net.Pipe()
		go plugin.ServeConn(side, p) //nolint:errcheck
		c := wire.NewConn(host, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
		go c.Run() //nolint:errcheck
		defer func() { _ = c.Close() }()
		call := func(h plugin.Hook, req any) (time.Duration, error) {
			begin := time.Now()
			err := c.Call(context.Background(), wire.HookPrefix+string(h), req, nil)
			return time.Since(begin), err
		}
		if d, err := call(plugin.HookShellEnv, &plugin.ShellEnvRequest{}); d != 100*time.Millisecond || err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Errorf("shell.env: ended after %v with %v, want its 100 ms deadline", d, err)
		}
		if d, err := call(plugin.HookSystemTransform, &plugin.SystemTransformRequest{}); d != 5*time.Second || err == nil {
			t.Errorf("system.transform: ended after %v with %v, want the 5 s default", d, err)
		}
	})
}
