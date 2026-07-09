package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
)

// TestNotifyBacklogNeverBlocksReadLoop proves that conn.run's read loop
// never blocks on notification delivery. dispatch's notification branch
// (dispatchNotification) queues onto notifyCh for the dedicated
// runNotifications goroutine (see conn.runNotifications), but that queue is
// bounded (notifyQueueSize); a sustained backlog from a slow or wedged
// notification handler must never stop the read loop from doing anything
// else on this connection — most importantly, delivering RPC responses back
// to pending calls (conn.call blocks on those).
//
// Before the fix, dispatch's notification case was a blocking send —
// `select { case c.notifyCh <- msg: case <-c.closed: }` — with no default.
// Wedge the single notification-dispatch goroutine with a slow "hook/event"
// handler (runNotifications only ever calls the handler one at a time, so
// one slow call wedges every notification behind it), then flood notifyCh
// past capacity from the test's own goroutine, exactly like a peer would
// over the wire. On the old code, the message that overflows the channel
// durably blocks the read-loop goroutine inside that select — and since our
// writer goroutine's next pipe Write can then never be matched by another
// Read, it blocks too. Every goroutine touching this connection ends up
// durably blocked with no timers pending, which is precisely what
// synctest.Test treats as a deadlock and fails the test on — a bounded,
// deterministic red signal with no sleeps or wall-clock timeouts (see
// AGENTS.md's synctest guidance, and TestEventQueueFullDropsWithoutBlocking
// in events_test.go for the same pattern one layer up).
func TestNotifyBacklogNeverBlocksReadLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer, connSide := net.Pipe()

		release := make(chan struct{})
		wedged := make(chan struct{})
		var wedgeOnce sync.Once
		handler := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
			if method == "hook/event" {
				wedgeOnce.Do(func() { close(wedged) })
				<-release
				return nil, nil
			}
			return map[string]bool{"ok": true}, nil
		}

		c := newConn(connSide, handler)
		go c.run() //nolint:errcheck

		// release must fire (unblocking the wedged handler goroutine)
		// before the bubble ends, or synctest reports a leaked-goroutine
		// deadlock; Cleanups run after the test body, which is exactly
		// where we want the wedge lifted.
		t.Cleanup(func() {
			close(release)
			_ = peer.Close()
			_ = connSide.Close()
		})

		w := bufio.NewWriter(peer)
		r := bufio.NewReader(peer)
		send := func(msg rpcMessage) {
			t.Helper()
			raw, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, '\n')
			if _, err := w.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
		}

		// First hook/event notification: runNotifications dequeues it
		// immediately and blocks in the handler until release fires,
		// wedging notification dispatch for the rest of the test. net.Pipe
		// is synchronous, so waiting for <-wedged also guarantees notifyCh
		// is empty again (the message has already left the channel).
		send(rpcMessage{JSONRPC: "2.0", Method: "hook/event", Params: json.RawMessage(`{}`)})
		<-wedged

		// Fill notifyCh to exactly its capacity, then send well past it.
		// None of this may block the read loop.
		const overflow = 200
		for i := 0; i < notifyQueueSize+overflow; i++ {
			send(rpcMessage{JSONRPC: "2.0", Method: "hook/event", Params: json.RawMessage(`{}`)})
		}

		// (a) the read loop must still be alive and answering requests,
		// completely unaffected by the saturated notification queue.
		id := int64(1)
		send(rpcMessage{JSONRPC: "2.0", ID: &id, Method: "ping"})
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("reading response: %v", err)
		}
		var resp rpcMessage
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("unmarshaling response: %v", err)
		}
		if resp.ID == nil || *resp.ID != id {
			t.Fatalf("response id = %v, want %d", resp.ID, id)
		}
		if resp.Error != nil {
			t.Fatalf("response error = %+v", resp.Error)
		}

		// (b) exactly the messages that couldn't fit must be dropped and
		// counted — not silently discarded, and not somehow squeezed into
		// (or leaked past) the bounded channel.
		if got := c.notifyDropped.Load(); got != uint64(overflow) {
			t.Fatalf("notifyDropped = %d, want %d (notifyCh capacity %d exhausted, rest dropped)",
				got, overflow, notifyQueueSize)
		}
	})
}

// TestShutdownBypassesNotifyBacklog proves that the one undroppable
// notification in this protocol — shutdown — is never starved by a
// backlogged notifyCh: dispatchNotification hands it to the read loop
// inline instead of queuing it, so it always takes effect immediately, even
// while notifyCh is completely saturated with undelivered hook/event
// notifications.
func TestShutdownBypassesNotifyBacklog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer, connSide := net.Pipe()

		release := make(chan struct{})
		wedged := make(chan struct{})
		var wedgeOnce sync.Once
		var c *conn
		handler := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
			if method == "hook/event" {
				wedgeOnce.Do(func() { close(wedged) })
				<-release
				return nil, nil
			}
			if method == methodShutdown {
				// Mirrors sdk.go's server.handle: shutdown's job is to
				// close the connection.
				_ = c.close()
			}
			return nil, nil
		}

		c = newConn(connSide, handler)
		runDone := make(chan error, 1)
		go func() { runDone <- c.run() }()
		t.Cleanup(func() {
			close(release)
			_ = peer.Close()
			_ = connSide.Close()
		})

		w := bufio.NewWriter(peer)
		send := func(msg rpcMessage) {
			t.Helper()
			raw, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, '\n')
			if _, err := w.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
		}

		send(rpcMessage{JSONRPC: "2.0", Method: "hook/event", Params: json.RawMessage(`{}`)})
		<-wedged
		for i := 0; i < notifyQueueSize; i++ {
			send(rpcMessage{JSONRPC: "2.0", Method: "hook/event", Params: json.RawMessage(`{}`)})
		}

		// notifyCh is now completely saturated; shutdown must still take
		// effect, closing the connection and returning from run(). Both
		// receives below block until that happens; if shutdown were stuck
		// behind the backlog instead, every goroutine involved (this one,
		// the read loop, and the wedged notification handler) would end up
		// durably blocked, which synctest reports as a deadlock — the same
		// bounded, sleep-free red signal as TestNotifyBacklogNeverBlocksReadLoop.
		send(rpcMessage{JSONRPC: "2.0", Method: "shutdown"})
		<-c.closed
		if err := <-runDone; err == nil {
			t.Fatal("run() returned nil, want the read error from the now-closed connection")
		}
	})
}

// fakeClientAPI implements ClientAPI for TestHostNotificationsDroppedNeverBlocksResponses:
// only MCPCall does anything (its behavior is test-controlled); the other
// two methods are never exercised by that test.
type fakeClientAPI struct {
	mcpCall func(ctx context.Context, req *MCPCallRequest) (*MCPCallResult, error)
}

func (f *fakeClientAPI) SessionMessages(context.Context, *SessionMessagesRequest) (*SessionMessagesResponse, error) {
	return nil, errors.New("fakeClientAPI: SessionMessages not implemented")
}

func (f *fakeClientAPI) MCPCall(ctx context.Context, req *MCPCallRequest) (*MCPCallResult, error) {
	return f.mcpCall(ctx, req)
}

func (f *fakeClientAPI) Generate(context.Context, *GenerateRequest) (*GenerateResponse, error) {
	return nil, errors.New("fakeClientAPI: Generate not implemented")
}

// TestHostNotificationsDroppedNeverBlocksResponses is the Host-level
// counterpart to TestNotifyBacklogNeverBlocksReadLoop: it exercises the same
// fix through the public API (Host.NotificationsDropped) rather than by
// reaching into conn's unexported field, and does so via the same conn
// machinery Host actually uses to talk to a plugin process.
//
// dispatchNotification's drop rule protects both sides of a connection, not
// just a plugin's inbound hook/event stream: a plugin could just as well
// send the harness a notification-shaped message for a method that would
// normally be a request (here, "client/mcp.call" with no id). This proves
// that an adversarial or malfunctioning plugin flooding the harness with
// those cannot wedge Host's read loop either — the flood is dropped past
// notifyCh's capacity (observable via Host.NotificationsDropped) instead of
// blocking, so a legitimate hook round trip (SystemTransform) started before
// the flood still completes once the fake plugin gets around to answering
// it.
func TestHostNotificationsDroppedNeverBlocksResponses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hostSide, fakePluginSide := net.Pipe()

		release := make(chan struct{})
		wedged := make(chan struct{})
		var wedgeOnce sync.Once
		client := &fakeClientAPI{
			mcpCall: func(context.Context, *MCPCallRequest) (*MCPCallResult, error) {
				wedgeOnce.Do(func() { close(wedged) })
				<-release
				return &MCPCallResult{}, nil
			},
		}

		const pluginName = "adversary"
		manifest := Manifest{Name: pluginName, ProtocolVersion: ProtocolVersion, Hooks: []Hook{HookSystemTransform}}
		h := newTestHost(t, Options{Client: client}, Spec{
			Manifest: manifest,
			dial:     func() (io.ReadWriteCloser, error) { return hostSide, nil },
		})
		t.Cleanup(func() {
			close(release)
			_ = hostSide.Close()
			_ = fakePluginSide.Close()
		})

		fakeDone := make(chan struct{})
		go func() {
			defer close(fakeDone)
			r := bufio.NewReader(fakePluginSide)
			w := bufio.NewWriter(fakePluginSide)
			readMsg := func() rpcMessage {
				line, err := r.ReadBytes('\n')
				if err != nil {
					return rpcMessage{}
				}
				var m rpcMessage
				_ = json.Unmarshal(line, &m)
				return m
			}
			writeMsg := func(m rpcMessage) {
				raw, err := json.Marshal(m)
				if err != nil {
					t.Error(err)
					return
				}
				raw = append(raw, '\n')
				if _, err := w.Write(raw); err != nil {
					return
				}
				_ = w.Flush()
			}

			// Answer the initialize handshake so the instance starts.
			init := readMsg()
			manifestRaw, err := json.Marshal(manifest)
			if err != nil {
				t.Error(err)
				return
			}
			writeMsg(rpcMessage{JSONRPC: "2.0", ID: init.ID, Result: manifestRaw})

			// Flood the harness with notification-shaped "client/mcp.call"
			// messages: the first one wedges Host's single notification
			// consumer inside fakeClientAPI.MCPCall (via wedgeOnce/release
			// above), then every send after notifyCh fills must be
			// dropped rather than block this write (net.Pipe is
			// synchronous, so each send here paces against Host's read
			// loop actually consuming it).
			const overflow = 200
			for i := 0; i < notifyQueueSize+overflow; i++ {
				writeMsg(rpcMessage{JSONRPC: "2.0", Method: "client/mcp.call", Params: json.RawMessage(`{}`)})
			}

			// Only now read and answer the hook/system.transform request
			// Host sent independently (and blocked writing/awaiting,
			// possibly since before the flood even started) — proving
			// Host's read loop was never stuck on the flood above.
			req := readMsg()
			respRaw, err := json.Marshal(SystemTransformResponse{Segments: []string{"adversary survived"}})
			if err != nil {
				t.Error(err)
				return
			}
			writeMsg(rpcMessage{JSONRPC: "2.0", ID: req.ID, Result: respRaw})
		}()

		got := h.SystemTransform(context.Background(), &SystemTransformRequest{SessionID: "s1"})
		<-fakeDone

		if len(got) != 1 || got[0] != "adversary survived" {
			t.Fatalf("segments = %v, want [adversary survived] (Host's read loop must not have been wedged by the flood)", got)
		}
		if dropped := h.NotificationsDropped(pluginName); dropped == 0 {
			t.Fatal("Host.NotificationsDropped(\"adversary\") = 0, want > 0 after flooding past notifyCh's capacity")
		}
	})
}
