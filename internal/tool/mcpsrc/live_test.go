package mcpsrc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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
	// deny holds the status that every request of a method answers.
	deny map[string]int
	// notify, when set, makes the next tools/list answer start with a list_changed notification and wait for notify.release
	// before the list itself. After that answer is built, the server lists notifyTools.
	notify      *gate
	notifyTools []string
	// notifyCall makes the next tools/call answer start with a list_changed notification.
	notifyCall bool
}

func newMemServer(tools ...string) *memServer {
	return &memServer{tools: tools, gates: map[string][]*gate{}, counts: map[string]int{}, fail: map[string]int{}, deny: map[string]int{}}
}

// holdNext holds the next request of method until the test releases the gate.
func (m *memServer) holdNext(method string) *gate {
	g := &gate{held: make(chan struct{}), release: make(chan struct{})}
	m.mu.Lock()
	m.gates[method] = append(m.gates[method], g)
	m.mu.Unlock()
	return g
}

// notifyOnNextCall makes the answer to the next tools/call carry a list_changed notification.
func (m *memServer) notifyOnNextCall() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifyCall = true
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

// takeFailure returns the HTTP status that the next request of method answers with, or 0. The caller holds m.mu.
func (m *memServer) takeFailure(method string) int {
	if m.fail[method] > 0 {
		m.fail[method]--
		return http.StatusInternalServerError
	}
	return m.deny[method]
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
	if status := m.takeFailure(method); status != 0 {
		m.mu.Unlock()
		return &http.Response{StatusCode: status, Header: http.Header{}, Request: r, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}
	var result any = map[string]any{}
	var special *gate
	var notifyNow bool
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
	case "tools/call":
		result = map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}
		if m.notifyCall {
			m.notifyCall = false
			notifyNow = true
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
		if notifyNow {
			resp.Header.Set("Content-Type", "text/event-stream")
			resp.Body = io.NopCloser(bytes.NewReader(append([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\nevent: message\ndata: "), append(b, "\n\n"...)...)))
		}
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

// denyAll makes every request of method answer with status.
func (m *memServer) denyAll(method string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deny[method] = status
}

func memSource(m *memServer, specs map[string]config.MCPServerSpec) *Source {
	s := New(config.Config{MCPServers: specs}, "", nil)
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

// trigger makes the server send a list_changed notification in the answer to a call, and waits for the call.
func trigger(t *testing.T, s *Source, m *memServer) {
	t.Helper()
	m.notifyOnNextCall()
	if _, err := s.Call(context.Background(), "w", "a", nil); err != nil {
		t.Fatal(err)
	}
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
		s.Set(map[string]config.MCPServerSpec{"w": changed})
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

func TestNextClientOfANameWaitsForEveryEarlierClientToClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		del := m.holdNext("DELETE")
		release := sync.OnceFunc(func() { close(del.release) })
		defer release()
		second, third := memSpec(), memSpec()
		second.ConnectTimeoutS, third.ConnectTimeoutS = 20, 30
		s.Set(map[string]config.MCPServerSpec{"w": second})
		<-del.held
		s.Set(map[string]config.MCPServerSpec{"w": third})
		synctest.Wait()
		if got := m.count("initialize"); got != 1 {
			t.Fatalf("initialize requests while the first client closes = %d, want 1", got)
		}
		release()
		synctest.Wait()
		if got := m.count("initialize"); got != 2 {
			t.Errorf("initialize requests after the first client closed = %d, want 2", got)
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
		s.Set(map[string]config.MCPServerSpec{"w": changed})
		<-del.held
		s.Set(map[string]config.MCPServerSpec{})
		release()
		synctest.Wait()
		if got := m.count("initialize"); got != 1 {
			t.Errorf("initialize requests = %d, want 1: the replacement that a later call removed must not dial", got)
		}
	})
}

func TestEmptySetStartsNoFirstConnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, nil)
		defer s.Close()
		s.Set(map[string]config.MCPServerSpec{})
		synctest.Wait()
		s.Set(map[string]config.MCPServerSpec{"w": memSpec()})
		synctest.Wait()
		if ts := s.Toolset(context.Background(), nil, nil, ""); len(ts.Deferred) != 0 || !slices.ContainsFunc(ts.Tools, func(t turn.Tool) bool { return t.Spec().Name == "mcp__w__a" }) {
			t.Errorf("tools %d, deferred %d, want the tool eager as a tool of the first connect", len(ts.Tools), len(ts.Deferred))
		}
	})
}

func TestOverlappingNotificationsEndWithTheNewestList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		older := m.holdNext("tools/list")
		trigger(t, s, m)
		<-older.held
		m.setTools("a", "b")
		trigger(t, s, m)
		synctest.Wait()
		if got := toolNamesOf(s, "w"); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("tools while the older read runs = %v, want [a]", got)
		}
		close(older.release)
		synctest.Wait()
		if got := toolNamesOf(s, "w"); !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("tools after both reads = %v, want [a b]", got)
		}
	})
}

func TestFailedRereadKeepsTheLastGoodList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		older := m.holdNext("tools/list")
		m.setTools("a", "b")
		trigger(t, s, m)
		<-older.held
		m.failNext("tools/list")
		trigger(t, s, m)
		close(older.release)
		synctest.Wait()
		if got := toolNamesOf(s, "w"); !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("tools = %v, want [a b]: the failed second read must keep the list of the first", got)
		}
	})
}

func TestModelCallWaitsAcrossOverlappingRefreshes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		first, second := m.holdNext("tools/list"), m.holdNext("tools/list")
		trigger(t, s, m)
		<-first.held
		done := make(chan turn.Toolset)
		go func() { done <- s.Toolset(context.Background(), nil, nil, "") }()
		synctest.Wait()
		m.setTools("a", "b")
		trigger(t, s, m)
		close(first.release)
		<-second.held
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("the model call returned while a refresh is in flight")
		default:
		}
		close(second.release)
		synctest.Wait()
		ts := <-done
		if !slices.ContainsFunc(ts.Deferred, func(t turn.Tool) bool { return t.Spec().Name == "mcp__w__b" }) {
			t.Errorf("deferred has %d tools, want the tool of the second refresh", len(ts.Deferred))
		}
	})
}

func TestCallToAToolThatARefreshWithdrewReadsNoSuchTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a", "b")
		s := memSource(m, map[string]config.MCPServerSpec{"w": memSpec()})
		defer s.Close()
		ts := s.Toolset(context.Background(), nil, nil, "")
		m.setTools("a")
		trigger(t, s, m)
		synctest.Wait()
		for _, tool := range ts.Tools {
			if tool.Spec().Name != "mcp__w__b" {
				continue
			}
			before := m.count("tools/call")
			res, err := tool.Run(context.Background(), protocol.ToolCall{Name: "mcp__w__b"})
			if err != nil || !res.IsError || res.Text != "no such tool available: mcp__w__b" {
				t.Errorf("Run = %+v, %v, want the no such tool answer", res, err)
			}
			if m.count("tools/call") != before {
				t.Errorf("the server received a call to a tool that it withdrew")
			}
			return
		}
		t.Fatal("tool b is not in the tools")
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
		s.Set(map[string]config.MCPServerSpec{})
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

func TestCloseWaitsForTheAuthCallbackInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		m.denyAll("initialize", http.StatusUnauthorized)
		started := make(chan struct{}, 8)
		release := make(chan struct{})
		var ended atomic.Bool
		s := NewSession(config.Config{}, nil, map[string]config.MCPServerSpec{"w": memSpec()}, "", nil,
			func(string, int) {
				started <- struct{}{}
				<-release
				ended.Store(true)
			}, HoldRetries(true))
		s.httpClient = &http.Client{Transport: m}
		s.Toolset(context.Background(), nil, nil, "")
		<-started
		var closed atomic.Bool
		go func() {
			s.Close()
			closed.Store(true)
		}()
		synctest.Wait()
		if closed.Load() {
			t.Fatal("Close returned while an auth callback was in flight")
		}
		close(release)
		synctest.Wait()
		if !closed.Load() || !ended.Load() {
			t.Errorf("closed = %v, callback ended = %v, want both true", closed.Load(), ended.Load())
		}
	})
}

func TestFailedConnectDetachesTheCloseOfItsClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		m.holdNext("tools/list")
		var mu sync.Mutex
		detached := 0
		spec := memSpec()
		spec.ConnectTimeoutS = 1
		s := NewSession(config.Config{}, nil, map[string]config.MCPServerSpec{"w": spec}, "", nil, nil, HoldRetries(true),
			Detach(func(f func()) {
				mu.Lock()
				detached++
				mu.Unlock()
			}))
		s.httpClient = &http.Client{Transport: m}
		s.Toolset(context.Background(), nil, nil, "")
		mu.Lock()
		defer mu.Unlock()
		if detached != 1 || m.count(http.MethodDelete) != 0 {
			t.Errorf("detached closes = %d, DELETE requests = %d, want 1 and 0 when the connect times out", detached, m.count(http.MethodDelete))
		}
		s.Close()
	})
}

func TestRemoteErrorTextNeverReachesTheToolError(t *testing.T) {
	err := (&Source{}).hide("w", &mcp.RPCError{Code: -32001, Message: "denied Bearer sk-live-0123456789"})
	if want := `mcp: server "w": call failed: rpc error -32001`; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	var rpc *mcp.RPCError
	if !errors.As(err, &rpc) {
		t.Error("the error does not keep its cause for errors.As")
	}
}

func TestResponseOverTheConfiguredBoundFailsTheConnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer(strings.Repeat("t", 2000))
		s := New(config.Config{MCPResponseBytes: 512, MCPServers: map[string]config.MCPServerSpec{"w": memSpec()}}, "", nil, HoldRetries(true))
		s.httpClient = &http.Client{Transport: m}
		defer s.Close()
		s.Toolset(context.Background(), nil, nil, "")
		if st := s.status(); len(st) != 1 || st[0].Connected {
			t.Errorf("status = %+v, want the server down: its tool list is over the response bound", st)
		}
	})
}

func TestResultOverTheBoundIsCutWithAMarker(t *testing.T) {
	s := &Source{resultBytes: 10}
	text, blobs := s.bound("aaaaaa", []protocol.Blob{{Data: []byte("1234")}, {Data: []byte("12")}, {Data: []byte("1234")}})
	if want := "aaaaaa\n[mcp: result cut at 10 bytes; 6 bytes left out]"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if len(blobs) != 1 || len(blobs[0].Data) != 4 {
		t.Errorf("blobs = %v, want the first blob only", blobs)
	}
	if text, _ := s.bound("short", nil); text != "short" {
		t.Errorf("a result inside the bound changed to %q", text)
	}
}

func TestSlowReadOfTheToolListAfterANotificationEndsAtTheConnectTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		first, reread := m.holdNext("tools/list"), m.holdNext("tools/list")
		notify := m.notifyDuringNextList("a")
		spec := memSpec()
		spec.ConnectTimeoutS = 2
		s := memSource(m, map[string]config.MCPServerSpec{"w": spec})
		defer s.Close()
		start := time.Now()
		done := make(chan time.Duration)
		go func() {
			s.Toolset(context.Background(), nil, nil, "")
			done <- time.Since(start)
		}()
		<-first.held
		close(first.release)
		<-notify.held
		close(notify.release)
		<-reread.held
		if waited := <-done; waited != 2*time.Second {
			t.Errorf("the first Toolset waited %v for a held read of the tool list, want the connect timeout of 2s", waited)
		}
	})
}

func toolDefinitions(t *testing.T, ts turn.Toolset) string {
	t.Helper()
	var specs []protocol.ToolSpec
	for _, tool := range ts.Tools {
		specs = append(specs, tool.Spec())
	}
	b, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSessionServerThatLoadsLateNeverHoldsATurnAndNeverChangesTheTools(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemServer("a")
		list := m.holdNext("tools/list")
		s := NewSession(config.Config{}, nil, map[string]config.MCPServerSpec{"w": memSpec()}, "", nil, nil, HoldRetries(true))
		s.httpClient = &http.Client{Transport: m}
		defer s.Close()
		s.Start()
		<-list.held
		first := turn.WithID(context.Background(), "turn1")
		start := time.Now()
		ts := s.Toolset(first, nil, nil, "")
		if waited := time.Since(start); waited != 0 {
			t.Errorf("the first Toolset of a turn waited %v for a server that is loading, want it not to wait", waited)
		}
		before := toolDefinitions(t, ts)
		if notice := ts.Notices[1]; notice.Kind != loadingKind || !notice.Transient || !strings.Contains(notice.Text, "w tools loading") {
			t.Errorf("loading notice = %+v, want a transient notice that names w", notice)
		}
		call := protocol.ToolCall{Name: "mcp__w__a"}
		res, err := ts.Resolver.ResolveTool(first, call.Name).Run(first, call)
		if err != nil || !res.IsError || res.Text != "mcp: w tools are still loading; try again shortly" {
			t.Errorf("call mid-load = %+v, %v, want the still loading result", res, err)
		}
		if waited := time.Since(start); waited != loadingWait {
			t.Errorf("the call mid-load waited %v, want %v", waited, loadingWait)
		}
		close(list.release)
		synctest.Wait()
		for _, turnID := range []string{"turn1", "turn2"} {
			ts = s.Toolset(turn.WithID(context.Background(), turnID), nil, nil, "")
			if after := toolDefinitions(t, ts); after != before {
				t.Errorf("tool definitions in %s changed when the server loaded:\n%s\n%s", turnID, before, after)
			}
		}
		if n := ts.Notices[1]; n.Text != "" {
			t.Errorf("loading notice after the load = %q, want none", n.Text)
		}
		res, err = ts.Resolver.ResolveTool(first, call.Name).Run(first, call)
		if err != nil || res.IsError || res.Text != "ok" {
			t.Errorf("call after the load = %+v, %v, want the tool result", res, err)
		}
	})
}
