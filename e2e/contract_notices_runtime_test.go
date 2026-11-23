package e2e

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func TestContractRuntimeDurableNotices(t *testing.T) {
	skipShort(t)
	t.Run("a_server_that_still_loads_keeps_the_recorded_down_notice_in_place", rowAServerThatStillLoadsKeepsTheRecordedDownNotice)
}

func rowAServerThatStillLoadsKeepsTheRecordedDownNotice(t *testing.T) {
	t.Parallel()
	log := newSyncLog()
	fake := harnesstest.NewChat(t, replyText("ok"))
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Logger: slog.New(log), Getenv: func(string) string { return "k" }, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "SESSION_MCP_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	down := harness.WithSessionMCP(map[string]config.MCPServerSpec{"weather": {Command: []string{filepath.Join(t.TempDir(), "no-such-server")}}})
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"}, down)
	if err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s, "a", "one")
	<-log.parked
	sessionMCPTurn(t, s, "b", "two")
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	unblock := make(chan struct{})
	defer close(unblock)
	slow := harness.WithSessionMCP(map[string]config.MCPServerSpec{"remote": {URL: slowListStub(t, unblock)}})
	if s, err = r.Open(t.Context(), "s1", slow); err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s, "c", "three")
	reqs := fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("model requests = %d, want 3", len(reqs))
	}
	got := requestText(reqs[2])
	if !strings.Contains(requestText(reqs[1]), "[mcp: unavailable") || !strings.Contains(got, "[mcp: unavailable") || strings.Contains(got, "every server is connected again") {
		t.Errorf("request 3 must keep the recorded down notice and add no cleared notice while a server still loads: %s", got)
	}
}
