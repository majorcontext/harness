package wire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// TestNotifyBacklogNeverBlocksReadLoop proves a saturated notification queue
// never stops the read loop from answering requests, and that each dropped
// notification is counted.
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

		c := NewConn(connSide, handler)
		go c.Run() //nolint:errcheck

		t.Cleanup(func() {
			close(release)
			_ = peer.Close()
			_ = connSide.Close()
		})

		w := bufio.NewWriter(peer)
		r := bufio.NewReader(peer)
		send := func(msg Message) {
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

		send(Message{JSONRPC: "2.0", Method: "hook/event", Params: json.RawMessage(`{}`)})
		<-wedged

		const overflow = 200
		for i := 0; i < NotifyQueueSize+overflow; i++ {
			send(Message{JSONRPC: "2.0", Method: "hook/event", Params: json.RawMessage(`{}`)})
		}

		id := int64(1)
		send(Message{JSONRPC: "2.0", ID: &id, Method: "ping"})
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("reading response: %v", err)
		}
		var resp Message
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("unmarshaling response: %v", err)
		}
		if resp.ID == nil || *resp.ID != id {
			t.Fatalf("response id = %v, want %d", resp.ID, id)
		}
		if resp.Error != nil {
			t.Fatalf("response error = %+v", resp.Error)
		}

		if got := c.NotifyDropped(); got != uint64(overflow) {
			t.Fatalf("notifyDropped = %d, want %d (notifyCh capacity %d exhausted, rest dropped)",
				got, overflow, NotifyQueueSize)
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
		var c *Conn
		handler := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
			if method == "hook/event" {
				wedgeOnce.Do(func() { close(wedged) })
				<-release
				return nil, nil
			}
			if method == MethodShutdown {
				// Mirrors sdk.go's server.handle: shutdown's job is to
				// close the connection.
				_ = c.Close()
			}
			return nil, nil
		}

		c = NewConn(connSide, handler)
		runDone := make(chan error, 1)
		go func() { runDone <- c.Run() }()
		t.Cleanup(func() {
			close(release)
			_ = peer.Close()
			_ = connSide.Close()
		})

		w := bufio.NewWriter(peer)
		send := func(msg Message) {
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

		send(Message{JSONRPC: "2.0", Method: "hook/event", Params: json.RawMessage(`{}`)})
		<-wedged
		for i := 0; i < NotifyQueueSize; i++ {
			send(Message{JSONRPC: "2.0", Method: "hook/event", Params: json.RawMessage(`{}`)})
		}

		// notifyCh is now completely saturated; shutdown must still take
		// effect, closing the connection and returning from run(). Both
		// receives below block until that happens; if shutdown were stuck
		// behind the backlog instead, every goroutine involved (this one,
		// the read loop, and the wedged notification handler) would end up
		// durably blocked, which synctest reports as a deadlock — the same
		// bounded, sleep-free red signal as TestNotifyBacklogNeverBlocksReadLoop.
		send(Message{JSONRPC: "2.0", Method: "shutdown"})
		<-c.Done()
		if err := <-runDone; err == nil {
			t.Fatal("run() returned nil, want the read error from the now-closed connection")
		}
	})
}

// TestConcurrentCallsMultiplexByID proves Call matches a response to its
// request by ID. The peer answers the second request first.
func TestConcurrentCallsMultiplexByID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer, connSide := net.Pipe()
		c := NewConn(connSide, func(context.Context, string, json.RawMessage) (any, error) {
			return nil, errors.New("plugin: test peer sends no requests")
		})
		go c.Run() //nolint:errcheck // stream end is expected at cleanup
		t.Cleanup(func() {
			_ = peer.Close()
			_ = connSide.Close()
		})

		r := bufio.NewReader(peer)
		w := bufio.NewWriter(peer)

		type outcome struct {
			method string
			got    string
			err    error
		}
		done := make(chan outcome, 2)
		call := func(method string) {
			var got string
			err := c.Call(context.Background(), method, map[string]string{"method": method}, &got)
			done <- outcome{method: method, got: got, err: err}
		}

		readRequest := func() Message {
			t.Helper()
			line, err := r.ReadBytes('\n')
			if err != nil {
				t.Fatalf("reading request: %v", err)
			}
			var msg Message
			if err := json.Unmarshal(line, &msg); err != nil {
				t.Fatalf("unmarshaling request %q: %v", line, err)
			}
			if msg.ID == nil {
				t.Fatalf("request %q carries no id", line)
			}
			return msg
		}
		respond := func(msg Message, result string) {
			t.Helper()
			raw, _ := json.Marshal(result)
			out, _ := json.Marshal(Message{JSONRPC: "2.0", ID: msg.ID, Result: raw})
			if _, err := w.Write(append(out, '\n')); err != nil || w.Flush() != nil {
				t.Fatalf("writing response: %v", err)
			}
		}

		go call("first")
		req1 := readRequest()

		go call("second")
		req2 := readRequest()
		if req1.Method != "first" || req2.Method != "second" || *req1.ID == *req2.ID {
			t.Fatalf("requests = %q/%d and %q/%d, want first and second with distinct ids", req1.Method, *req1.ID, req2.Method, *req2.ID)
		}

		respond(req2, "result-second")
		respond(req1, "result-first")

		results := make(map[string]string, 2)
		for range 2 {
			o := <-done
			if o.err != nil {
				t.Fatalf("call %q: %v", o.method, o.err)
			}
			results[o.method] = o.got
		}
		for _, name := range []string{"first", "second"} {
			if got, want := results[name], "result-"+name; got != want {
				t.Errorf("call %q got %q, want %q (response crossed with the other call)", name, got, want)
			}
		}
	})
}

// tearingConn wraps a stream and splits every Write into small chunks, with
// a scheduling point between them. A real pipe does this: a write above
// PIPE_BUF is not atomic. conn.write holds wmu across its whole rwc.Write
// call, so the chunks of one frame stay together; without that lock two
// writers interleave their chunks and produce garbage lines.
//
// net.Pipe alone cannot show this defect, because net.Pipe serializes a
// whole Write internally (its own wrMu). tearingConn removes that cover.
type tearingConn struct {
	io.ReadWriteCloser
	chunk int
}

func (t *tearingConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(t.chunk, len(p))
		got, err := t.ReadWriteCloser.Write(p[:n])
		written += got
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// TestConcurrentWritesNeverInterleaveFrames proves conn.write serializes
// whole frames. Many goroutines write at once over a transport that tears
// every Write into 16-byte chunks. Every line the peer reads must be one
// complete, well-formed JSON-RPC message, and the peer must see EXACTLY the
// frames that were sent — no missing frame and no extra one.
func TestConcurrentWritesNeverInterleaveFrames(t *testing.T) {
	const writers = 16

	peer, connSide := net.Pipe()
	c := NewConn(&tearingConn{ReadWriteCloser: connSide, chunk: 16}, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("plugin: test peer sends no requests")
	})
	// Close the conn itself, not just the pipes: newConn starts the
	// runNotifications goroutine, and only conn.close (through conn.fail)
	// closes the channel that goroutine exits on. Closing the pipes alone
	// leaks it.
	t.Cleanup(func() {
		_ = c.Close()
		_ = peer.Close()
	})

	// Pad each frame well past the chunk size, so an unserialized write is
	// certain to be cut apart and mixed with another writer's bytes.
	pad := strings.Repeat("x", 512)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = c.Notify("hook/event", map[string]any{"writer": i, "pad": pad})
		}()
	}
	close(start)

	r := bufio.NewReader(peer)
	seen := make(map[int]int, writers)
	for range writers {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("reading frame: %v", err)
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("frame is not one whole JSON-RPC message: %v\nline: %q", err, line)
		}
		var params struct {
			Writer int    `json:"writer"`
			Pad    string `json:"pad"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			t.Fatalf("frame params are damaged: %v\nline: %q", err, line)
		}
		if params.Pad != pad {
			t.Fatalf("frame from writer %d carries a damaged payload (%d bytes, want %d)",
				params.Writer, len(params.Pad), len(pad))
		}
		seen[params.Writer]++
	}
	wg.Wait()

	// The surplus direction: every writer appears exactly once, and no
	// writer appears twice.
	if len(seen) != writers {
		t.Fatalf("saw %d distinct writers, want %d: %v", len(seen), writers, seen)
	}
	for i := range writers {
		if seen[i] != 1 {
			t.Errorf("writer %d appeared %d times, want exactly 1", i, seen[i])
		}
	}
}

// TestHandlerContextEndsWithTheConn proves the context of a request handler
// ends when the stream ends.
func TestHandlerContextEndsWithTheConn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer, connSide := net.Pipe()
		started, ended := make(chan struct{}), make(chan error, 1)
		c := NewConn(connSide, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
			close(started)
			<-ctx.Done()
			ended <- ctx.Err()
			return nil, ctx.Err()
		})
		go c.Run() //nolint:errcheck // stream end is the point of the test
		go func() { _, _ = peer.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"x"}` + "\n")) }()
		<-started
		_ = peer.Close()
		if err := <-ended; !errors.Is(err, context.Canceled) {
			t.Fatalf("handler ctx err = %v, want canceled", err)
		}
		_ = connSide.Close()
		c.Wait()
	})
}
