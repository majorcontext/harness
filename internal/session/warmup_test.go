package session_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// closeRuntime closes r and fails the test when it cannot.
func closeRuntime(t *testing.T, r *harness.Runtime) {
	t.Helper()
	if err := r.Close(bg); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// pluginFixture builds the wire-level plugin of the contract suite.
func pluginFixture(t *testing.T, cfg string) []config.PluginSpec {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pluginfixture")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/majorcontext/harness/harnesstest/pluginfixture").CombinedOutput(); err != nil {
		t.Fatalf("go build pluginfixture: %v\n%s", err, out)
	}
	return []config.PluginSpec{{Name: "fixture", Command: []string{bin}, Config: []byte(cfg)}}
}

// warmWindow is how long a test waits to see a warm-up that must not happen.
const warmWindow = 500 * time.Millisecond

func TestWarmDiscoversToolsOnlyForTheCodexWebsocket(t *testing.T) {
	for _, tc := range []struct {
		name      string
		websocket bool
		discovers bool
	}{{"http", false, false}, {"websocket", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			seen := make(chan struct{}, 1)
			mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				select {
				case seen <- struct{}{}:
				default:
				}
				http.Error(w, "down", http.StatusServiceUnavailable)
			}))
			defer mcpSrv.Close()
			s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{})
			t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
			r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: config.Config{
				Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
					BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"},
					UseWebSocketTransport: tc.websocket}},
				MCPServers: map[string]config.MCPServerSpec{"weather": {URL: mcpSrv.URL}}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"}); err != nil {
				t.Fatal(err)
			}
			if tc.discovers {
				<-seen
			} else {
				window := time.NewTimer(warmWindow)
				select {
				case <-seen:
				case <-window.C:
				}
				window.Stop()
			}
			closeRuntime(t, r)
			if got := int(hits.Load()); (got > 0) != tc.discovers {
				t.Errorf("MCP server requests = %d, want some: %v", got, tc.discovers)
			}
		})
	}
}

func TestCloseDoesNotWaitForAWarmUpThatDiscoversTools(t *testing.T) {
	stuck := make(chan struct{})
	seen := make(chan struct{}, 1)
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case seen <- struct{}{}:
		default:
		}
		select {
		case <-stuck:
		case <-r.Context().Done():
		}
	}))
	defer mcpSrv.Close()
	defer close(stuck)
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{})
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: config.Config{
		Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
			BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"},
			UseWebSocketTransport: true}},
		MCPServers: map[string]config.MCPServerSpec{"weather": {URL: mcpSrv.URL}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"}); err != nil {
		t.Fatal(err)
	}
	<-seen
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := r.Close(ctx); err != nil || time.Since(start) > 2*time.Second {
		t.Errorf("Close = %v after %v, want nil at once: the session ended its warm-up", err, time.Since(start))
	}
}

func warmRuntime(t *testing.T, s *harnesstest.OpenAI, st harness.Store, plugins []config.PluginSpec) *harness.Runtime {
	t.Helper()
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	r, err := harness.New(harness.Options{Store: st, Config: config.Config{
		Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
			BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"},
			UseWebSocketTransport: true}},
		Plugins: plugins}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// awaitPrewarms waits until s has seen n websocket prewarms.
func awaitPrewarms(t *testing.T, s *harnesstest.OpenAI, n int) []string {
	t.Helper()
	deadline, tick := time.NewTimer(10*time.Second), time.NewTicker(5*time.Millisecond)
	defer deadline.Stop()
	defer tick.Stop()
	for {
		if got := s.PrewarmInstructions(); len(got) >= n {
			return got
		}
		select {
		case <-deadline.C:
			t.Fatalf("websocket prewarms = %d, want %d", len(s.PrewarmInstructions()), n)
		case <-tick.C:
		}
	}
}

func TestWakeWarmsTheWebsocketAgain(t *testing.T) {
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{},
		harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "hi"}})
	st := harness.NewMemStore()
	r1 := warmRuntime(t, s, st, nil)
	sess, err := r1.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	awaitPrewarms(t, s, 1)
	ask(t, sess, "hello")
	closeRuntime(t, r1)

	r2 := warmRuntime(t, s, st, nil)
	t.Cleanup(func() { closeRuntime(t, r2) })
	if _, err := r2.Open(bg, "s1"); err != nil {
		t.Fatal(err)
	}
	awaitPrewarms(t, s, 2)
	var prewarms, dials int
	for _, e := range s.WireEvents() {
		switch e.Event {
		case "prewarm":
			prewarms++
		case "dial":
			dials++
		}
	}
	if prewarms != 2 || dials < 2 {
		t.Errorf("prewarms = %d, dials = %d, want one prewarm on a new connection for create and for wake", prewarms, dials)
	}
}

func TestWarmCarriesTheSystemTransformOfAPlugin(t *testing.T) {
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{})
	r := warmRuntime(t, s, harness.NewMemStore(), pluginFixture(t, `{"segment":"SEGMENT"}`))
	t.Cleanup(func() { closeRuntime(t, r) })
	if _, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"}); err != nil {
		t.Fatal(err)
	}
	got := awaitPrewarms(t, s, 1)
	if got[0] != "SEGMENT" {
		t.Errorf("prewarm instructions = %q, want the system.transform segment of the plugin", got[0])
	}
}
