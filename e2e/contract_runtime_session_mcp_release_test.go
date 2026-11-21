package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/config"
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
