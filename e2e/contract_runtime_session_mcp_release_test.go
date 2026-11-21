package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/store"
)

type deleteProbe struct {
	url       string
	seen      chan struct{}
	unblock   chan struct{}
	cancelled atomic.Bool
	finished  atomic.Bool
}

// sessionIDStub is an MCP server that gives a session id and holds the DELETE of that session until unblock closes.
func sessionIDStub(t *testing.T) *deleteProbe {
	t.Helper()
	p := &deleteProbe{seen: make(chan struct{}, 4), unblock: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			p.seen <- struct{}{}
			select {
			case <-p.unblock:
				p.finished.Store(true)
			case <-r.Context().Done():
				p.cancelled.Store(true)
			}
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "initialize" && req.Method != "tools/list" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		result := `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}`
		if req.Method == "tools/list" {
			result = `{"tools":[{"name":"forecast","description":"d","inputSchema":{"type":"object"}}]}`
		} else {
			w.Header().Set("Mcp-Session-Id", "remote-session")
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + result + `}`))
	}))
	t.Cleanup(func() {
		select {
		case <-p.unblock:
		default:
			close(p.unblock)
		}
		srv.Close()
	})
	p.url = srv.URL
	return p
}

func rowAReleaseDoesNotWaitForTheRemoteDeleteAndCloseJoinsIt(t *testing.T) {
	p := sessionIDStub(t)
	r, _ := sessionMCPRuntime(t, nil, nil, config.Config{}, replyText("ok"))
	servers := map[string]config.MCPServerSpec{"remote": {URL: p.url}}
	s := sessionMCPCreate(t, r, servers)
	sessionMCPTurn(t, s, "a", "go")
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-s.Done()
	if p.cancelled.Load() || p.finished.Load() {
		t.Fatalf("the session ended after the remote DELETE gave up (%v) or finished (%v), want it to end while the DELETE waits", p.cancelled.Load(), p.finished.Load())
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close(context.Background()) }()
	<-p.seen
	close(p.unblock)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if !p.finished.Load() {
		t.Error("Runtime.Close returned before the remote DELETE finished")
	}
}

func slowListStub(t *testing.T, unblock chan struct{}) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "tools/list" {
			select {
			case <-unblock:
			case <-r.Context().Done():
				return
			}
		}
		if req.Method != "initialize" && req.Method != "tools/list" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		result := `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}`
		if req.Method == "tools/list" {
			result = `{"tools":[{"name":"forecast","description":"d","inputSchema":{"type":"object"}}]}`
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + result + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func rowASlowSessionServerNeverHoldsATurnOrChangesTheTools(t *testing.T) {
	unblock := make(chan struct{})
	url := slowListStub(t, unblock)
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	fake := harnesstest.NewChat(t, replyText("ok"))
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Getenv: func(string) string { return "k" }, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "SESSION_MCP_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s := sessionMCPCreate(t, r, map[string]config.MCPServerSpec{"remote": {URL: url}})
	sessionMCPTurn(t, s, "a", "first")
	close(unblock)
	if err := s.AwaitMCP(t.Context()); err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s, "b", "second")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model requests = %d, want 2", len(reqs))
	}
	if requestHas(reqs[0], "mcp__remote__forecast") || !reflect.DeepEqual(reqs[0].Tools, reqs[1].Tools) {
		t.Errorf("tools of the requests = %v and %v, want the same tools without the late tool", reqs[0].Tools, reqs[1].Tools)
	}
	if got := requestText(reqs[0]); !strings.Contains(got, "remote tools loading") {
		t.Errorf("the first request lacks the loading notice: %s", got)
	}
	if got := requestText(reqs[1]); strings.Contains(got, "tools loading") {
		t.Errorf("the second request still holds the loading notice: %s", got)
	}
}
