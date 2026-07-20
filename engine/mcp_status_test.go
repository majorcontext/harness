package engine

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestAmbientMCPStatusPresentWhenDegraded is invariant 6's headline test
// (docs/plans/2026-07-20-mcp-init-resilience.md), red-verified against
// pre-Task-2 engine.go: a degraded MCP server produced no in-band signal at
// all — the model had nothing to go on but a missing tool. This test asserts
// the request the model actually sees carries a status block naming the
// degraded server.
func TestAmbientMCPStatusPresentWhenDegraded(t *testing.T) {
	mgr := NewMCPManager(map[string]MCPServerConfig{
		"linear": {URL: "http://127.0.0.1:1"}, // nothing listens: connection refused
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		MCP:       mgr,
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	last := lastUserText(t, prov.requests[0])
	if !strings.Contains(last, "[mcp:") {
		t.Fatalf("last user message = %q, want a degraded-MCP status block naming linear", last)
	}
	if !strings.Contains(last, "linear") {
		t.Errorf("ambient block = %q, want it to name the degraded server", last)
	}
	// Pin the classified form and, since a real endpoint URL was dialed
	// here, assert it never appears verbatim in model-visible context (see
	// classifyMCPConnectError's doc comment).
	if !strings.Contains(last, "connection refused") {
		t.Errorf("ambient block = %q, want the classified reason %q", last, "connection refused")
	}
	if strings.Contains(last, "127.0.0.1") {
		t.Errorf("ambient block = %q, leaked the raw endpoint URL", last)
	}
}

// TestMCPStatusSegmentAbsentBeforeConnectTriggered covers the
// never-triggered state mcpStatusSegment/MCPManager.Status's doc comments
// call out explicitly: a manager that has never had Tools/CallTool/
// CallServerTool called on it has m.state == nil, not "every server
// failed" — that must render as silence, not degradation.
func TestMCPStatusSegmentAbsentBeforeConnectTriggered(t *testing.T) {
	mgr := NewMCPManager(map[string]MCPServerConfig{
		"linear": {URL: "http://127.0.0.1:1"},
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	if got := mcpStatusSegment(mgr); got != "" {
		t.Errorf("mcpStatusSegment before any connect trigger = %q, want \"\"", got)
	}
}

// TestMCPStatusSegmentAbsentForNilRegistry covers the "MCP not configured
// at all" case: Config.MCP left nil (see MCPRegistry's doc comment).
func TestMCPStatusSegmentAbsentForNilRegistry(t *testing.T) {
	if got := mcpStatusSegment(nil); got != "" {
		t.Errorf("mcpStatusSegment(nil) = %q, want \"\"", got)
	}
}

// bareMCPRegistry implements MCPRegistry but not mcpStatusReader — the
// shape any pre-existing MCPRegistry fake (cmd/harness, server) has today.
type bareMCPRegistry struct{}

func (bareMCPRegistry) Tools(context.Context) []provider.ToolDef { return nil }
func (bareMCPRegistry) CallTool(context.Context, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}
func (bareMCPRegistry) CallServerTool(context.Context, string, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}

// TestMCPStatusSegmentAbsentForRegistryWithoutStatus covers an MCPRegistry
// implementation with no status surface at all: mcpStatusSegment must not
// panic or misbehave on it, just render nothing (see mcpStatusReader's doc
// comment on why this is a separate interface).
func TestMCPStatusSegmentAbsentForRegistryWithoutStatus(t *testing.T) {
	if got := mcpStatusSegment(bareMCPRegistry{}); got != "" {
		t.Errorf("mcpStatusSegment(bareMCPRegistry) = %q, want \"\"", got)
	}
}

// TestMCPStatusSegmentAbsentWhenHealthy covers the happy path: every
// configured server connected, no degraded clause to render.
func TestMCPStatusSegmentAbsentWhenHealthy(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "ok", content: []map[string]any{textContent("fine")}},
	}}
	url := srv.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	mgr.Tools(context.Background())

	if got := mcpStatusSegment(mgr); got != "" {
		t.Errorf("mcpStatusSegment with every server healthy = %q, want \"\"", got)
	}
}

// TestMCPStatusSegmentDeterministicOrdering covers the sorted-by-name
// requirement: two simultaneously degraded servers must always render in
// the same (alphabetical) order, matching MCPManager.Status's own sort.
func TestMCPStatusSegmentDeterministicOrdering(t *testing.T) {
	mgr := NewMCPManager(map[string]MCPServerConfig{
		"zeta":  {URL: "http://127.0.0.1:1"},
		"alpha": {URL: "http://127.0.0.1:1"},
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	mgr.Tools(context.Background())

	got := mcpStatusSegment(mgr)
	ai, zi := strings.Index(got, "alpha"), strings.Index(got, "zeta")
	if ai < 0 || zi < 0 {
		t.Fatalf("mcpStatusSegment = %q, want both alpha and zeta named", got)
	}
	if ai > zi {
		t.Errorf("mcpStatusSegment = %q, want alpha before zeta", got)
	}
}

// TestAmbientMCPStatusAbsentWhenHealthy is the Session-level happy-path
// counterpart to TestAmbientMCPStatusPresentWhenDegraded: a fully healthy
// MCP server must add no ambient text to the request at all.
func TestAmbientMCPStatusAbsentWhenHealthy(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "ok", content: []map[string]any{textContent("fine")}},
	}}
	url := srv.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		MCP:       mgr,
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if last := lastUserText(t, prov.requests[0]); strings.Contains(last, "[mcp:") {
		t.Fatalf("last user message = %q, want no ambient MCP status block", last)
	}
}

// fakeMCPStatusReader is a minimal mcpStatusReader (plus the rest of
// MCPRegistry, unused by these tests) that returns a fixed Status() slice
// under caller control — used to pin formatMCPServerStatus's reason text
// against a specific LastErr without driving a real connect attempt.
type fakeMCPStatusReader struct {
	bareMCPRegistry
	status []MCPServerStatus
}

func (f fakeMCPStatusReader) Status() []MCPServerStatus { return f.status }

// TestMCPStatusSegmentClassifiesReasonNeverLeaksURL is the should-fix
// finding's headline test, red-verified against pre-classifier code (see
// formatMCPServerStatus's old `reason = st.LastErr.Error()`): a raw
// *url.Error (the shape net/http returns on a failed dial, and the shape
// LastErr takes for an HTTP MCP server) stringifies as
// `Post "<full-URL>": <cause>`, so a real endpoint URL carrying a secret in
// its path or query would land verbatim in model-visible context. The
// ambient status block must carry only the classified, URL-free reason.
func TestMCPStatusSegmentClassifiesReasonNeverLeaksURL(t *testing.T) {
	const secret = "SUPERSECRET123"
	leaky := &url.Error{
		Op:  "Post",
		URL: "https://mcp.example.com/v1?token=" + secret,
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
		},
	}
	reg := fakeMCPStatusReader{status: []MCPServerStatus{
		{Name: "linear", Connected: false, LastErr: leaky},
	}}

	got := mcpStatusSegment(reg)
	if strings.Contains(got, secret) {
		t.Fatalf("mcpStatusSegment = %q, leaked the fake secret from LastErr's URL", got)
	}
	if strings.Contains(got, "mcp.example.com") {
		t.Fatalf("mcpStatusSegment = %q, leaked the endpoint host from LastErr's URL", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("mcpStatusSegment = %q, want the classified reason %q", got, "connection refused")
	}
	if !strings.Contains(got, "linear") {
		t.Errorf("mcpStatusSegment = %q, want it to still name the server", got)
	}
}

// TestAmbientMCPStatusOnlyOnNewestUserMessage mirrors
// TestAmbientProcessStatusPresentAfterStart's cache-prefix assertion: over
// two turns, a permanently degraded server's block must land only on the
// newest user message, never an earlier one.
func TestAmbientMCPStatusOnlyOnNewestUserMessage(t *testing.T) {
	mgr := NewMCPManager(map[string]MCPServerConfig{
		"linear": {URL: "http://127.0.0.1:1"},
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "first"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "second"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		MCP:       mgr,
	})
	if _, err := s.Prompt(context.Background(), "hello one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), "hello two"); err != nil {
		t.Fatal(err)
	}

	last := prov.requests[1]
	var sawUser int
	for i, m := range last.Messages {
		if m.Role != message.RoleUser {
			continue
		}
		sawUser++
		isNewest := i == len(last.Messages)-1
		has := strings.Contains(renderMsgText(m), "[mcp:")
		if isNewest && !has {
			t.Errorf("newest user message = %+v, want the ambient MCP status block", m)
		}
		if !isNewest && has {
			t.Errorf("ambient status block leaked onto a non-newest message: %+v", m)
		}
	}
	if sawUser < 2 {
		t.Fatalf("second request carried %d user messages, want at least 2 (hello one, hello two)", sawUser)
	}
}

// TestAmbientMCPStatusNeverPersisted mirrors
// TestAmbientProcessStatusNeverPersisted: the block must never survive a
// LoadSession round trip.
func TestAmbientMCPStatusNeverPersisted(t *testing.T) {
	sesDir := t.TempDir()
	mgr := NewMCPManager(map[string]MCPServerConfig{
		"linear": {URL: "http://127.0.0.1:1"},
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: sesDir,
		MCP:        mgr,
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(Config{
		Providers:  provider.Registry{"test": prov},
		SessionDir: sesDir,
		MCP:        mgr,
	}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	for _, m := range loaded.History() {
		if strings.Contains(renderMsgText(m), "[mcp:") {
			t.Fatalf("ambient MCP status block leaked into persisted history: %+v", m)
		}
	}
}

// TestAmbientMCPStatusDisappearsAfterRecovery is invariant 6's
// self-correcting assertion: a server degraded on turn 1's request is
// healthy — no block at all — by turn 2's, once its background retry
// commits a success in between. Uses a real HTTP handler (like
// TestMCPManagerCallServerToolRetryingThenRecovers) that fails the very
// first request and succeeds every one after, with mcpTestRetryCommitted
// as the synchronization point instead of a sleep or poll loop.
func TestAmbientMCPStatusDisappearsAfterRecovery(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		first := requestCount == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		(&fakeMCPHTTPServer{tools: []fakeMCPTool{
			{name: "echo", content: []map[string]any{textContent("hi")}},
		}}).serveHTTP(w, r)
	})
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)

	committed := make(chan bool, 8)
	orig := mcpTestRetryCommitted
	t.Cleanup(func() { mcpTestRetryCommitted = orig })
	mcpTestRetryCommitted = func(server string, connected bool) {
		if server == "svc" {
			committed <- connected
		}
	}

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: hs.URL, ConnectTimeout: 500 * time.Millisecond}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "first"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "second"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		MCP:       mgr,
	})

	if _, err := s.Prompt(context.Background(), "hello one"); err != nil {
		t.Fatal(err)
	}
	first := lastUserText(t, prov.requests[0])
	if !strings.Contains(first, "[mcp:") || !strings.Contains(first, "svc") {
		t.Fatalf("first request's ambient text = %q, want a degraded block naming svc", first)
	}

	// Block until the background retry actually commits a success — real
	// backoff wall time (mcpRetryBackoffBase, ~0.5-1s here), not a sleep.
	for !<-committed {
	}

	if _, err := s.Prompt(context.Background(), "hello two"); err != nil {
		t.Fatal(err)
	}
	second := lastUserText(t, prov.requests[1])
	if strings.Contains(second, "[mcp:") {
		t.Fatalf("second request's ambient text = %q, want no block after recovery", second)
	}
}
