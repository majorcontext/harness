package session_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

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
