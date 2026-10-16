package mcpsrc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// gate holds one request at the fake until the test releases it.
type gate struct{ held, release chan struct{} }

// memServer is an MCP server over an in-memory http.RoundTripper, so a synctest bubble sees every wait of it.
type memServer struct {
	mu     sync.Mutex
	tools  []string
	gates  map[string][]*gate
	counts map[string]int
	fail   map[string]int
	// notify, when set, makes the next tools/list answer start with a list_changed notification and wait for notify.release
	// before the list itself. After that answer is built, the server lists notifyTools.
	notify      *gate
	notifyTools []string
}

func newMemServer(tools ...string) *memServer {
	return &memServer{tools: tools, gates: map[string][]*gate{}, counts: map[string]int{}, fail: map[string]int{}}
}

// holdNext holds the next request of method until the test releases the gate.
func (m *memServer) holdNext(method string) *gate {
	g := &gate{held: make(chan struct{}), release: make(chan struct{})}
	m.mu.Lock()
	m.gates[method] = append(m.gates[method], g)
	m.mu.Unlock()
	return g
}

// failNext makes the next request of method answer with HTTP 500.
func (m *memServer) failNext(method string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail[method]++
}

// notifyDuringNextList makes the next tools/list answer carry a list_changed notification, then wait for the gate,
// and makes the server list after tools from then on.
func (m *memServer) notifyDuringNextList(after ...string) *gate {
	g := &gate{held: make(chan struct{}), release: make(chan struct{})}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify, m.notifyTools = g, after
	return g
}

// notifyBody is an event stream that sends head at once and rest after the gate opens.
type notifyBody struct {
	head, rest []byte
	g          *gate
	waited     bool
}

func (b *notifyBody) Read(p []byte) (int, error) {
	if len(b.head) == 0 && !b.waited {
		b.waited = true
		close(b.g.held)
		<-b.g.release
		b.head, b.rest = b.rest, nil
	}
	if len(b.head) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.head)
	b.head = b.head[n:]
	return n, nil
}

func (b *notifyBody) Close() error { return nil }

func (m *memServer) setTools(tools ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = tools
}

func (m *memServer) count(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[method]
}

func (m *memServer) RoundTrip(r *http.Request) (*http.Response, error) {
	method := r.Method
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		method = req.Method
	}
	m.mu.Lock()
	m.counts[method]++
	if m.fail[method] > 0 {
		m.fail[method]--
		m.mu.Unlock()
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Request: r, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}
	var result any = map[string]any{}
	var special *gate
	switch method {
	case "initialize":
		result = map[string]any{"protocolVersion": mcp.LatestProtocolVersion, "serverInfo": map[string]any{"name": "mem", "version": "1"},
			"capabilities": map[string]any{"tools": map[string]any{"listChanged": true}, "resources": map[string]any{}}}
	case "tools/list":
		var tools []map[string]any
		for _, n := range m.tools {
			tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object"}})
		}
		result = map[string]any{"tools": tools}
		if special, m.notify = m.notify, nil; special != nil {
			m.tools = m.notifyTools
		}
	case "resources/read":
		result = map[string]any{"contents": []map[string]any{{"uri": "doc://x", "text": "body"}}}
	}
	var g *gate
	if q := m.gates[method]; len(q) > 0 {
		g, m.gates[method] = q[0], q[1:]
	}
	m.mu.Unlock()
	if g != nil {
		close(g.held)
		select {
		case <-g.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: r, Body: io.NopCloser(bytes.NewReader(nil))}
	switch {
	case method == http.MethodDelete:
	case len(req.ID) == 0:
		resp.StatusCode = http.StatusAccepted
	default:
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		resp.Body = io.NopCloser(bytes.NewReader(b))
		if special != nil {
			resp.Header.Set("Content-Type", "text/event-stream")
			resp.Body = &notifyBody{head: []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n"),
				rest: append(append([]byte("event: message\ndata: "), b...), "\n\n"...), g: special}
		}
		if method == "initialize" {
			resp.Header.Set("Mcp-Session-Id", "mem")
		}
	}
	return resp, nil
}

func memSource(m *memServer, specs map[string]config.MCPServerSpec) *Source {
	s := New(config.Config{MCPServers: specs}, nil)
	s.httpClient = &http.Client{Transport: m}
	return s
}

func memSpec() config.MCPServerSpec { return config.MCPServerSpec{URL: "http://mem/mcp"} }

func toolNamesOf(s *Source, server string) []string {
	var out []string
	for _, t := range s.view().live[server].tools {
		out = append(out, t.Name)
	}
	return out
}

func TestReplacementConnectsOnlyAfterTheOldClientClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		del := m.holdNext("DELETE")
		release := sync.OnceFunc(func() { close(del.release) })
		defer release()
		changed := memSpec()
		changed.ConnectTimeoutS = 20
		fin := s.Replace(map[string]config.MCPServerSpec{"w": changed})
		go fin()
		<-del.held
		synctest.Wait()
		if got := m.count("initialize"); got != 1 {
			t.Fatalf("initialize requests while the old client closes = %d, want 1", got)
		}
		release()
		synctest.Wait()
		if got := m.count("initialize"); got != 2 {
			t.Errorf("initialize requests after the old client closed = %d, want 2", got)
		}
	})
}

func TestEmptySetStartsNoFirstConnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, nil)
		defer s.Close()
		s.Replace(map[string]config.MCPServerSpec{})()
		synctest.Wait()
		s.Replace(map[string]config.MCPServerSpec{"w": memSpec()})()
		synctest.Wait()
		if ts := s.Toolset(context.Background(), nil, nil, ""); len(ts.Deferred) != 0 || !slices.ContainsFunc(ts.Tools, func(t turn.Tool) bool { return t.Spec().Name == "mcp__w__a" }) {
			t.Errorf("tools %d, deferred %d, want the tool eager as a tool of the first connect", len(ts.Tools), len(ts.Deferred))
		}
	})
}

func TestOlderToolListAnswerNeverReplacesANewerOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		_, e := s.find("w")
		c := s.view().live["w"].client
		older := m.holdNext("tools/list")
		s.listChanged(e, c)
		<-older.held
		m.setTools("a", "b")
		s.listChanged(e, c)
		synctest.Wait()
		if got := toolNamesOf(s, "w"); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("tools after the newer read = %v, want [a b]", got)
		}
		close(older.release)
		synctest.Wait()
		if got := toolNamesOf(s, "w"); !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("tools after the older answer = %v, want [a b]", got)
		}
	})
}

func TestResourceReadEndsWhenItsServerGoes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		read := m.holdNext("resources/read")
		defer func() {
			select {
			case <-read.release:
			default:
				close(read.release)
			}
		}()
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err = reader{s}.Run(context.Background(), protocol.ToolCall{Arguments: json.RawMessage(`{"server":"w","uri":"doc://x"}`)})
		}()
		<-read.held
		s.Replace(map[string]config.MCPServerSpec{})()
		synctest.Wait()
		select {
		case <-done:
			if err == nil {
				t.Error("the read of a removed server returned no error")
			}
		default:
			t.Error("the read of a removed server is still in flight")
		}
	})
}

func TestReplacementRemovedWhileItWaitsNeverDials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		del := m.holdNext("DELETE")
		release := sync.OnceFunc(func() { close(del.release) })
		defer release()
		changed := memSpec()
		changed.ConnectTimeoutS = 20
		fin := s.Replace(map[string]config.MCPServerSpec{"w": changed})
		go fin()
		<-del.held
		_, waiting := s.find("w")
		s.Replace(map[string]config.MCPServerSpec{})()
		release()
		synctest.Wait()
		if got := waiting.health.attempts; got != 0 {
			t.Errorf("attempts of the replacement that a later call removed = %d, want 0", got)
		}
	})
}

func TestOlderReadIsDroppedWhenTheNewerReadFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		_, e := s.find("w")
		c := s.view().live["w"].client
		older := m.holdNext("tools/list")
		m.setTools("a", "b")
		s.listChanged(e, c)
		<-older.held
		m.failNext("tools/list")
		s.listChanged(e, c)
		synctest.Wait()
		close(older.release)
		synctest.Wait()
		if got := toolNamesOf(s, "w"); !slices.Equal(got, []string{"a"}) {
			t.Errorf("tools = %v, want [a]: the answer of a read that is not the newest started is dropped", got)
		}
	})
}

func TestRefreshDuringTheFirstListWinsAndIsAToolOfTheFirstConnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		g := m.notifyDuringNextList("a", "b")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		go s.Toolset(context.Background(), nil, nil, "")
		<-g.held
		synctest.Wait()
		close(g.release)
		synctest.Wait()
		ts := s.Toolset(context.Background(), nil, nil, "")
		if got := toolNamesOf(s, "w"); !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("tools = %v, want [a b]: the refresh that finished first must not be lost", got)
		}
		if !slices.ContainsFunc(ts.Tools, func(t turn.Tool) bool { return t.Spec().Name == "mcp__w__b" }) || len(ts.Deferred) != 0 {
			t.Errorf("tool b must be a tool of the first connect, not a deferred one")
		}
		for _, n := range ts.Notices {
			if n.Kind == addedKind && n.Text != "" {
				t.Errorf("added notice %q names a tool that the first connect froze", n.Text)
			}
		}
	})
}
