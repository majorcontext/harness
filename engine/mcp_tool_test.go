package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// mcpToolConnect runs the mcp tool's connect action against s directly,
// without any *testing.T dependency — safe to call from a non-test
// goroutine (concurrency tests below spawn several).
func mcpToolConnect(ctx context.Context, s *Session, server string) (mcpToolConnectResult, error) {
	tool, ok := s.tools[mcpSessionToolName]
	if !ok {
		return mcpToolConnectResult{}, errors.New("mcp tool absent")
	}
	args, err := json.Marshal(mcpToolArgs{Action: "connect", Server: server})
	if err != nil {
		return mcpToolConnectResult{}, err
	}
	parts, err := tool.Run(ctx, s, args)
	if err != nil {
		return mcpToolConnectResult{}, err
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		return mcpToolConnectResult{}, errors.New("mcp connect result not text")
	}
	var res mcpToolConnectResult
	if err := json.Unmarshal([]byte(text.Text), &res); err != nil {
		return mcpToolConnectResult{}, err
	}
	return res, nil
}

// runMCPStatusAction runs the mcp tool's status action against s and
// decodes the result, t.Fatal on any failure.
func runMCPStatusAction(t *testing.T, s *Session) mcpToolStatusResult {
	t.Helper()
	tool, ok := s.tools[mcpSessionToolName]
	if !ok {
		t.Fatal("mcp tool absent")
	}
	parts, err := tool.Run(context.Background(), s, []byte(`{"action":"status"}`))
	if err != nil {
		t.Fatalf("mcp status: %v", err)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("mcp status result not text: %#v", parts[0])
	}
	var res mcpToolStatusResult
	if err := json.Unmarshal([]byte(text.Text), &res); err != nil {
		t.Fatalf("mcp status result not valid JSON: %v (%s)", err, text.Text)
	}
	return res
}

// runMCPConnectAction runs the mcp tool's connect action against s and
// t.Fatals on a tool error; use mcpToolConnect directly when an error is
// expected.
func runMCPConnectAction(t *testing.T, s *Session, server string) mcpToolConnectResult {
	t.Helper()
	res, err := mcpToolConnect(context.Background(), s, server)
	if err != nil {
		t.Fatalf("mcp connect(%s): %v", server, err)
	}
	return res
}

// callMCPToolExpectError runs the mcp tool's Run function directly with raw
// args and requires a non-nil error, returning its message.
func callMCPToolExpectError(t *testing.T, s *Session, args string) string {
	t.Helper()
	tool, ok := s.tools[mcpSessionToolName]
	if !ok {
		t.Fatal("mcp tool absent")
	}
	_, err := tool.Run(context.Background(), s, []byte(args))
	if err == nil {
		t.Fatalf("mcp tool run(%s): want error, got nil", args)
	}
	return err.Error()
}

// # Invariant 8: tool presence gating

func TestMCPToolAbsentWithoutConfiguredServers(t *testing.T) {
	s := NewSession(Config{})
	if _, ok := s.tools[mcpSessionToolName]; ok {
		t.Fatal("mcp tool present with no MCP registry configured, want absent")
	}
	for _, d := range s.toolDefs(context.Background()) {
		if d.Name == mcpSessionToolName {
			t.Fatal("mcp tool advertised in toolDefs with no MCP registry configured")
		}
	}
}

// TestMCPToolAbsentForRegistryWithoutConfigReader proves the narrow-
// interface gating (mcpConfigReader) degrades safely instead of panicking
// or forcing a connect: a plain MCPRegistry fake — like the ones
// cmd/harness/clientapi_test.go and server/clientapi_test.go already build,
// with only Tools/CallTool/CallServerTool — must keep compiling AND must
// not get the mcp tool, since it cannot answer "how many servers are
// configured" without connecting.
func TestMCPToolAbsentForRegistryWithoutConfigReader(t *testing.T) {
	s := NewSession(Config{MCP: minimalFakeMCPRegistry{}})
	if _, ok := s.tools[mcpSessionToolName]; ok {
		t.Fatal("mcp tool present for a registry without ConfiguredNames, want absent")
	}
}

// minimalFakeMCPRegistry implements only engine.MCPRegistry's three
// methods — the same minimal shape as the fakes in cmd/harness and server's
// own test files — with no Status/ConfiguredNames/Connect, to prove those
// don't need to exist for a plain MCPRegistry to keep working.
type minimalFakeMCPRegistry struct{}

func (minimalFakeMCPRegistry) Tools(context.Context) []provider.ToolDef { return nil }
func (minimalFakeMCPRegistry) CallTool(context.Context, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}
func (minimalFakeMCPRegistry) CallServerTool(context.Context, string, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}

func TestMCPToolPresentAndStatusWorksWhenConfiguredHealthy(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ping"}}}
	url := srv.start(t)
	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	s := NewSession(Config{MCP: mgr})
	if _, ok := s.tools[mcpSessionToolName]; !ok {
		t.Fatal("mcp tool absent with a configured MCP registry, want present")
	}

	var found bool
	for _, d := range s.toolDefs(context.Background()) { // triggers the first connect batch
		if d.Name == mcpSessionToolName {
			found = true
		}
	}
	if !found {
		t.Fatal("mcp tool not advertised in toolDefs with a configured MCP registry")
	}

	res := runMCPStatusAction(t, s)
	if len(res.Servers) != 1 || res.Servers[0].Name != "svc" || !res.Servers[0].Connected || res.Servers[0].Parked {
		t.Fatalf("status = %+v, want exactly one connected, non-parked \"svc\" entry", res)
	}
	if res.Servers[0].Reason != "" {
		t.Errorf("status reason for a connected server = %q, want empty", res.Servers[0].Reason)
	}
}

// # Invariant 3: status renders healthy / retrying / parked correctly, no raw error text.

func TestMCPToolStatusRetryingServerBeforeParked(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		leaky := errors.New("boom talking to http://internal.example/mcp?token=SECRET123")
		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			return nil, nil, leaky
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })
		s := NewSession(Config{MCP: mgr})

		mgr.Tools(context.Background()) // first attempt fails synchronously; background retry now waiting in backoff, not yet parked

		res := runMCPStatusAction(t, s)
		if len(res.Servers) != 1 {
			t.Fatalf("status = %+v, want exactly one entry", res)
		}
		got := res.Servers[0]
		if got.Connected {
			t.Error("Connected = true, want false")
		}
		if got.Parked {
			t.Error("Parked = true, want false (background retries not yet exhausted)")
		}
		if got.Attempts != 1 {
			t.Errorf("Attempts = %d, want 1", got.Attempts)
		}
		if got.Reason == "" {
			t.Error("Reason = \"\", want a classified reason")
		}
		if strings.Contains(got.Reason, "SECRET123") || strings.Contains(got.Reason, "internal.example") {
			t.Fatalf("Reason = %q, leaked the raw connect error text", got.Reason)
		}
	})
}

func TestMCPToolStatusParkedServer(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		committed := make(chan bool, 8)
		orig := mcpTestRetryCommitted
		t.Cleanup(func() { mcpTestRetryCommitted = orig })
		mcpTestRetryCommitted = func(server string, connected bool) { committed <- connected }

		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			return nil, nil, errors.New("boom") // never recovers
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })
		s := NewSession(Config{MCP: mgr})

		mgr.Tools(context.Background())
		for i := 0; i < mcpRetryMaxAttempts; i++ {
			if <-committed {
				t.Fatal("unexpected success; this server must never recover in this test")
			}
		}
		synctest.Wait() // let the goroutine finish exiting after parking

		res := runMCPStatusAction(t, s)
		if len(res.Servers) != 1 {
			t.Fatalf("status = %+v, want exactly one entry", res)
		}
		got := res.Servers[0]
		if got.Connected {
			t.Error("Connected = true, want false")
		}
		if !got.Parked {
			t.Error("Parked = false, want true once retries are exhausted")
		}
		if got.Reason == "" {
			t.Error("Reason = \"\", want a classified reason")
		}
	})
}

// # Invariant 4: connect on a parked server — failure stays classified and parked; success commits tools into the SAME session's very next toolDefs.

func TestMCPToolConnectOnParkedServerFailureStaysParked(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		committed := make(chan bool, 8)
		orig := mcpTestRetryCommitted
		t.Cleanup(func() { mcpTestRetryCommitted = orig })
		mcpTestRetryCommitted = func(server string, connected bool) { committed <- connected }

		leaky := errors.New("boom talking to http://internal.example/mcp?token=SECRET123")
		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			return nil, nil, leaky // every attempt, including the manual one below, fails
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })
		s := NewSession(Config{MCP: mgr})

		mgr.Tools(context.Background())
		for i := 0; i < mcpRetryMaxAttempts; i++ {
			<-committed
		}
		synctest.Wait()
		if !mgr.Status()[0].Parked {
			t.Fatal("precondition: want parked before the manual connect attempt")
		}

		_, err := mcpToolConnect(context.Background(), s, "svc")
		if err == nil {
			t.Fatal("connect on a still-failing parked server: want an error")
		}
		if strings.Contains(err.Error(), "SECRET123") || strings.Contains(err.Error(), "internal.example") {
			t.Fatalf("connect error = %q, leaked the raw connect error text", err)
		}

		st := mgr.Status()[0]
		if !st.Parked {
			t.Error("Parked = false after a failed manual connect, want still true")
		}
		if st.Connected {
			t.Error("Connected = true after a failed manual connect, want false")
		}
	})
}

// TestMCPToolConnectOnParkedServerSuccessCommitsToolsSameSession is
// invariant 4's headline test, red-verified: see the commit message for
// the evidence that this fails when Connect's commit step (rebuildToolsLocked
// + un-parking) is skipped.
//
// Everything — parking AND the later manual connect — runs inside the SAME
// synctest bubble: m.retryCtx (and so its Done() channel, which Connect's
// Close-interlock watcher selects on) is created once, at NewMCPManager
// time, and testing/synctest ties any channel/timer created inside a
// bubble to that bubble permanently — a select on it from outside (even
// after the bubble closure has returned) is a hard "select on synctest
// channel from outside bubble" fatal error, not just a flake. The manual
// connect itself needs no timer wait (a single immediate attempt via the
// fake mcpConnectFunc), so running it inside the bubble costs nothing.
func TestMCPToolConnectOnParkedServerSuccessCommitsToolsSameSession(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		var succeed atomic.Bool
		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			if succeed.Load() {
				return nil, []mcp.Tool{{Name: "ping"}}, nil
			}
			return nil, nil, errors.New("boom")
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })

		committed := make(chan bool, 8)
		orig := mcpTestRetryCommitted
		t.Cleanup(func() { mcpTestRetryCommitted = orig })
		mcpTestRetryCommitted = func(server string, connected bool) { committed <- connected }

		mgr.Tools(context.Background())
		for i := 0; i < mcpRetryMaxAttempts; i++ {
			if <-committed {
				t.Fatal("unexpected success during the parking phase")
			}
		}
		synctest.Wait()

		if !mgr.Status()[0].Parked {
			t.Fatal("precondition: want parked before the manual connect attempt")
		}

		s := NewSession(Config{MCP: mgr})
		for _, d := range s.toolDefs(context.Background()) {
			if d.Name == "mcp__svc__ping" {
				t.Fatal("tool present before recovery")
			}
		}

		succeed.Store(true)
		res, err := mcpToolConnect(context.Background(), s, "svc")
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if !res.Connected {
			t.Fatalf("connect result = %+v, want connected", res)
		}

		// The commit: tools present in the session's VERY NEXT toolDefs, no
		// new session, no other trigger — matching a background-retry
		// success exactly (see mcp.go's package doc).
		var found bool
		for _, d := range s.toolDefs(context.Background()) {
			if d.Name == "mcp__svc__ping" {
				found = true
			}
		}
		if !found {
			t.Fatal("mcp__svc__ping absent from toolDefs immediately after a successful connect")
		}

		st := mgr.Status()[0]
		if !st.Connected {
			t.Error("Connected = false after a successful connect, want true")
		}
		if st.Parked {
			t.Error("Parked = true after a successful connect, want false (un-parked)")
		}
	})
}

// # Invariant 5: no-op on already-connected; unknown-server error lists configured names.

func TestMCPToolConnectAlreadyConnectedIsFriendlyNoOp(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ping"}}}
	url := srv.start(t)
	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	mgr.Tools(context.Background())

	s := NewSession(Config{MCP: mgr})
	res := runMCPConnectAction(t, s, "svc")
	if !res.Connected {
		t.Fatal("want connected true")
	}
	if !strings.Contains(strings.ToLower(res.Message), "already") {
		t.Fatalf("message = %q, want it to mention already connected", res.Message)
	}
}

func TestMCPToolConnectUnknownServerListsConfiguredNames(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ping"}}}
	url := srv.start(t)
	mgr := NewMCPManager(map[string]MCPServerConfig{"svc-a": {URL: url}, "svc-b": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	mgr.Tools(context.Background())

	s := NewSession(Config{MCP: mgr})
	msg := callMCPToolExpectError(t, s, `{"action":"connect","server":"nope"}`)
	if !strings.Contains(msg, "svc-a") || !strings.Contains(msg, "svc-b") {
		t.Fatalf("error = %q, want it to list the configured server names", msg)
	}
}

func TestMCPToolConnectRequiresServer(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ping"}}}
	url := srv.start(t)
	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	mgr.Tools(context.Background())

	s := NewSession(Config{MCP: mgr})
	msg := callMCPToolExpectError(t, s, `{"action":"connect"}`)
	if !strings.Contains(msg, "server") {
		t.Fatalf("error = %q, want it to mention the missing server argument", msg)
	}
}

func TestMCPToolRejectsUnknownAction(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ping"}}}
	url := srv.start(t)
	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	mgr.Tools(context.Background())

	s := NewSession(Config{MCP: mgr})
	msg := callMCPToolExpectError(t, s, `{"action":"bogus"}`)
	if !strings.Contains(msg, "unknown action") {
		t.Fatalf("error = %q, want it to mention unknown action", msg)
	}
}

// # Invariant 6: double-connect impossible — in-flight guard serializes.

// TestMCPToolConnectConcurrentCallsSerializeViaInFlightGuard covers the
// "two concurrent tool connects" half of invariant 6. The server is parked
// first (bounded background retries exhausted), so no background goroutine
// is left alive to interfere — isolating this scenario from the
// tool-vs-background race covered by
// TestMCPManagerConnectReturnsInProgressWhileBackgroundRetryDialing below.
func TestMCPToolConnectConcurrentCallsSerializeViaInFlightGuard(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		committed := make(chan bool, 8)
		orig := mcpTestRetryCommitted
		t.Cleanup(func() { mcpTestRetryCommitted = orig })
		mcpTestRetryCommitted = func(server string, connected bool) { committed <- connected }

		var phase int32 // 0 while parking (fail fast), 1 once the two manual connects race
		gate := make(chan struct{})
		var dialsInFlight, maxConcurrentDials int32
		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			if atomic.LoadInt32(&phase) == 0 {
				return nil, nil, errors.New("boom")
			}
			n := atomic.AddInt32(&dialsInFlight, 1)
			for {
				max := atomic.LoadInt32(&maxConcurrentDials)
				if n <= max || atomic.CompareAndSwapInt32(&maxConcurrentDials, max, n) {
					break
				}
			}
			<-gate
			atomic.AddInt32(&dialsInFlight, -1)
			return nil, []mcp.Tool{{Name: "ping"}}, nil
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })

		mgr.Tools(context.Background())
		for i := 0; i < mcpRetryMaxAttempts; i++ {
			<-committed
		}
		synctest.Wait() // background retryServer goroutine has now exited (parked)

		atomic.StoreInt32(&phase, 1)
		s := NewSession(Config{MCP: mgr})

		results := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() {
				_, err := mcpToolConnect(context.Background(), s, "svc")
				results <- err
			}()
		}

		synctest.Wait() // both goroutines run until durably blocked: the winner is inside <-gate, the loser has already returned

		var loserErr error
		select {
		case loserErr = <-results:
		default:
			t.Fatal("want the loser to have already returned an in-progress error")
		}
		if loserErr == nil || !strings.Contains(loserErr.Error(), "in progress") {
			t.Fatalf("loser error = %v, want an already-in-progress error", loserErr)
		}

		if n := atomic.LoadInt32(&maxConcurrentDials); n != 1 {
			t.Fatalf("max concurrent dials = %d, want exactly 1 (the guard must serialize)", n)
		}

		close(gate) // let the winner's dial complete
		winnerErr := <-results
		if winnerErr != nil {
			t.Fatalf("winner error = %v, want nil", winnerErr)
		}

		st := mgr.Status()[0]
		if !st.Connected {
			t.Error("Connected = false after the winner commits, want true")
		}
	})
}

// TestMCPManagerConnectReturnsInProgressWhileBackgroundRetryDialing covers
// the "tool connect races a background retry's own dial" half of
// invariant 6, at the MCPManager level: Connect must return the
// already-in-progress error rather than double-dialing while retryServer's
// own attempt is in flight, and once that background attempt commits
// (here, successfully), a LATER Connect call must observe Connected and
// no-op cleanly — never re-dialing.
func TestMCPManagerConnectReturnsInProgressWhileBackgroundRetryDialing(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		var calls int32
		reachedAttempt := make(chan struct{})
		gate := make(chan struct{})
		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return nil, nil, errors.New("boom") // first attempt: fail fast, spawn the retry
			}
			close(reachedAttempt)
			<-gate
			return nil, []mcp.Tool{{Name: "ping"}}, nil
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })

		mgr.Tools(context.Background())
		<-reachedAttempt // the background retry's second attempt is now in flight, holding the guard

		if err := mgr.Connect(context.Background(), "svc"); err == nil || !strings.Contains(err.Error(), "in progress") {
			t.Fatalf("Connect while a background dial is in flight = %v, want an already-in-progress error", err)
		}

		close(gate) // let the background attempt finish (succeeds)
		synctest.Wait()

		if !mgr.Status()[0].Connected {
			t.Fatal("want the background retry's own commit to have succeeded")
		}

		if err := mgr.Connect(context.Background(), "svc"); err != nil {
			t.Fatalf("Connect after the background retry already succeeded = %v, want nil (clean no-op)", err)
		}
		if n := atomic.LoadInt32(&calls); n != 2 {
			t.Fatalf("connect dials = %d, want exactly 2 (the no-op Connect must not dial a third time)", n)
		}
	})
}

// TestMCPManagerConnectOnFreshManagerNeverAttempted covers the "name
// configured, first attempt just never happened" case Connect's own doc
// comment claims works: a manager whose Tools()/CallTool()/CallServerTool
// has never been called has a nil m.state (see ensureConnected), so a
// direct Connect call — bypassing the tool path, which always runs
// toolDefs -> s.cfg.MCP.Tools(ctx) first and so always populates m.state
// before any tool call is reachable — must still connect successfully
// instead of returning the defensive-fallback "not configured" error meant
// only for names Connect has genuinely never heard of.
func TestMCPManagerConnectOnFreshManagerNeverAttempted(t *testing.T) {
	withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
		return nil, []mcp.Tool{{Name: "ping"}}, nil
	})

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	if err := mgr.Connect(context.Background(), "svc"); err != nil {
		t.Fatalf("Connect on a fresh manager (no prior Tools() call) = %v, want nil", err)
	}
	status := mgr.Status()
	if len(status) != 1 || !status[0].Connected {
		t.Fatalf("Status() after Connect = %+v, want svc connected", status)
	}
}

// # Invariant 7: Close during a tool-triggered in-flight connect — no leak, no commit-after-close.

// TestMCPManagerCloseDuringInFlightToolConnectStopsPromptly mirrors
// TestMCPManagerCloseDuringInFlightRetryConnectStopsPromptly (the
// background-path version in mcp_test.go) but for a manual, tool-triggered
// Connect call: Close must cancel the in-flight dial's context immediately
// (proved by zero elapsed FAKE time) rather than waiting out the attempt's
// own ConnectTimeout, and Connect itself must report a cancellation rather
// than silently hanging or, worse, committing after Close already ran —
// asserted implicitly too: this test function returning while Connect's
// watcher goroutine were still alive would trip synctest's own
// goroutine-leak detection.
func TestMCPManagerCloseDuringInFlightToolConnectStopsPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int32
		reachedAttempt := make(chan struct{})
		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return nil, nil, errors.New("boom") // first attempt fails synchronously, populating state
			}
			close(reachedAttempt)
			<-ctx.Done() // the manual connect's own attempt: block until Close cancels it
			return nil, nil, ctx.Err()
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})

		mgr.Tools(context.Background()) // first attempt fails; the spawned background retry sits in its backoff wait and never dials again in this test

		connectDone := make(chan error, 1)
		go func() {
			connectDone <- mgr.Connect(context.Background(), "svc")
		}()
		<-reachedAttempt // the manual connect's dial is now in flight

		start := time.Now()
		if err := mgr.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("Close took %v of fake time, want exactly 0 (cancelling ctx must unblock the in-flight tool connect immediately)", elapsed)
		}

		if err := <-connectDone; err == nil {
			t.Error("Connect during Close = nil, want a cancellation error (no commit-after-Close)")
		}

		if mgr.Status()[0].Connected {
			t.Error("Status().Connected = true after Close raced an in-flight connect, want false (declined, not committed)")
		}
	})
}
