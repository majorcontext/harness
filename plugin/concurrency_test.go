package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
)

// This file is the specification for ONE property: a harness may keep
// several requests in flight on one plugin connection at the same time.
//
// The property is load-bearing for parallel tool execution in the engine. A
// batch of tool calls that runs concurrently dispatches the
// tool.execute.before / tool.execute.after hooks concurrently too, all over
// the same plugin pipe. If that pipe paired requests with responses by
// ARRIVAL ORDER, or if two writers could interleave the bytes of two
// frames, a parallel batch would cross the results of two unrelated tool
// calls — a silent, data-corrupting failure.
//
// The tests below prove the four mechanisms that make the pipe safe:
//
//  1. TestConcurrentCallsMultiplexByID — conn.call correlates a response by
//     its JSON-RPC id, never by arrival order.
//  2. TestConcurrentWritesNeverInterleaveFrames — conn.write's wmu keeps
//     every frame whole on a transport that tears a Write into chunks.
//  3. TestHostConcurrentToolHooksStayIndependent and
//     TestHostConcurrentExecuteToolStaysIndependent — the same property
//     through the production Host API, with two hooks genuinely in flight.
//  4. TestConcurrentFirstDispatchSpawnsOnce — a concurrent first dispatch
//     spawns one plugin process, not two.
//
// See PROTOCOL.md, "Concurrency", for the contract these tests hold.

// rendezvousTimeout is the hook deadline for the Host-level tests below. It
// is deliberately huge: every one of those tests runs inside a synctest
// bubble, so a hook that can never complete makes the bubble's fake clock
// jump straight to this deadline and the test fails at once, with no
// wall-clock cost. A small value would let a healthy dispatch look like a
// timeout instead.
const rendezvousTimeout = time.Hour

// TestConcurrentCallsMultiplexByID proves conn.call correlates a response
// with its request by the JSON-RPC id, not by the order the peer answers.
//
// The fake peer holds the FIRST request open until the SECOND request has
// arrived, then answers the second one first. Both calls are therefore in
// flight together, and the responses come back in the opposite order. Each
// caller must still get its own result.
//
// Determinism: net.Pipe is synchronous, so a request line is on the wire
// only after the test reads it. The test reads request 1 before it starts
// call 2, and answers only after it has read both. No sleep, and no
// deadline, gates any step.
func TestConcurrentCallsMultiplexByID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer, connSide := net.Pipe()
		c := newConn(connSide, func(context.Context, string, json.RawMessage) (any, error) {
			return nil, errors.New("plugin: test peer sends no requests")
		})
		go c.run() //nolint:errcheck // stream end is expected at cleanup
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
			err := c.call(context.Background(), method, map[string]string{"method": method}, &got)
			done <- outcome{method: method, got: got, err: err}
		}

		readRequest := func() rpcMessage {
			t.Helper()
			line, err := r.ReadBytes('\n')
			if err != nil {
				t.Fatalf("reading request: %v", err)
			}
			var msg rpcMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				t.Fatalf("unmarshaling request %q: %v", line, err)
			}
			if msg.ID == nil {
				t.Fatalf("request %q carries no id", line)
			}
			return msg
		}
		respond := func(msg rpcMessage, result string) {
			t.Helper()
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: msg.ID, Result: raw})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(append(out, '\n')); err != nil {
				t.Fatalf("writing response: %v", err)
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("flushing response: %v", err)
			}
		}

		// Call "first" goes out and stays unanswered.
		go call("first")
		req1 := readRequest()
		if req1.Method != "first" {
			t.Fatalf("request 1 method = %q, want %q", req1.Method, "first")
		}

		// Call "second" goes out while "first" is still in flight. Reading
		// its line proves the connection accepted a second concurrent
		// request instead of serializing behind the first.
		go call("second")
		req2 := readRequest()
		if req2.Method != "second" {
			t.Fatalf("request 2 method = %q, want %q", req2.Method, "second")
		}
		if *req1.ID == *req2.ID {
			t.Fatalf("both requests carry id %d: ids must be unique per call", *req1.ID)
		}

		// Answer in REVERSE order. Correlation by id is the only thing that
		// can route these two responses correctly.
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
		if got := results["first"]; got != "result-first" {
			t.Errorf("call \"first\" got %q, want %q (response crossed with the other call)", got, "result-first")
		}
		if got := results["second"]; got != "result-second" {
			t.Errorf("call \"second\" got %q, want %q (response crossed with the other call)", got, "result-second")
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
	c := newConn(&tearingConn{ReadWriteCloser: connSide, chunk: 16}, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("plugin: test peer sends no requests")
	})
	t.Cleanup(func() {
		_ = peer.Close()
		_ = connSide.Close()
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
			_ = c.notify("hook/event", map[string]any{"writer": i, "pad": pad})
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
		var msg rpcMessage
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

// TestHostConcurrentToolHooksStayIndependent drives the PRODUCTION Host API
// (Host.ToolExecuteBefore), not a hand-built conn, and proves two hook
// dispatches to ONE plugin run at the same time and never cross results.
//
// The fake plugin is a rendezvous: each invocation announces itself on
// arrived and then waits. The test reads BOTH announcements before it
// releases either. A transport that served one request at a time could
// never produce the second announcement, so the bubble would run out of
// runnable goroutines, jump to rendezvousTimeout, and fail.
func TestHostConcurrentToolHooksStayIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		arrived := make(chan string, 2)
		proceed := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		p := testPlugin(t, "rendezvous", &Hooks{
			ToolExecuteBefore: func(_ context.Context, _ *Client, req *ToolExecuteBeforeRequest) (*ToolExecuteBeforeResponse, error) {
				arrived <- req.CallID
				select {
				case <-proceed:
				case <-release:
				}
				// Echo the call id back through the rewritten args. A
				// crossed response shows up as the wrong id here.
				return &ToolExecuteBeforeResponse{
					Args: json.RawMessage(fmt.Sprintf(`{"echo":%q}`, req.CallID)),
				}, nil
			},
		})
		h := newTestHost(t, Options{HookTimeout: rendezvousTimeout}, p)

		type outcome struct {
			callID string
			args   string
			deny   string
		}
		done := make(chan outcome, 2)
		for _, id := range []string{"call-a", "call-b"} {
			go func() {
				args, deny := h.ToolExecuteBefore(context.Background(), &ToolExecuteBeforeRequest{
					SessionID: "s1", CallID: id, Tool: "bash", Args: json.RawMessage(`{}`),
				})
				done <- outcome{callID: id, args: string(args), deny: deny}
			}()
		}

		// Both hooks must be in flight together before either may finish.
		first, second := <-arrived, <-arrived
		if first == second {
			t.Fatalf("both dispatches reported call id %q: the two calls were not independent", first)
		}
		close(proceed)

		for range 2 {
			o := <-done
			if o.deny != "" {
				t.Fatalf("call %s was denied: %q", o.callID, o.deny)
			}
			want := fmt.Sprintf(`{"echo":%q}`, o.callID)
			if o.args != want {
				t.Errorf("call %s got args %s, want %s (result crossed with the other call)", o.callID, o.args, want)
			}
		}
	})
}

// TestHostConcurrentExecuteToolStaysIndependent is the same proof for
// Host.ExecuteTool: two plugin TOOL calls in flight at once over one
// connection, each getting its own output.
func TestHostConcurrentExecuteToolStaysIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		arrived := make(chan string, 2)
		proceed := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		p := testPlugin(t, "tooler", &Hooks{
			Tools: []Tool{{
				Def: ToolDef{Name: "echo", Description: "echoes its argument"},
				Execute: func(_ context.Context, _ *Client, args json.RawMessage) (message.Parts, error) {
					var in struct {
						Want string `json:"want"`
					}
					if err := json.Unmarshal(args, &in); err != nil {
						return nil, err
					}
					arrived <- in.Want
					select {
					case <-proceed:
					case <-release:
					}
					return message.Parts{&message.Text{Text: in.Want}}, nil
				},
			}},
		})
		h := newTestHost(t, Options{HookTimeout: rendezvousTimeout}, p)

		type outcome struct {
			want string
			got  string
			err  error
		}
		done := make(chan outcome, 2)
		for _, want := range []string{"alpha", "beta"} {
			go func() {
				resp, err := h.ExecuteTool(context.Background(), &ToolExecuteRequest{
					SessionID: "s1", CallID: want, Tool: "echo",
					Args: json.RawMessage(fmt.Sprintf(`{"want":%q}`, want)),
				})
				o := outcome{want: want, err: err}
				if err == nil && len(resp.Output) == 1 {
					if txt, ok := resp.Output[0].(*message.Text); ok {
						o.got = txt.Text
					}
				}
				done <- o
			}()
		}

		first, second := <-arrived, <-arrived
		if first == second {
			t.Fatalf("both tool calls reported %q: the two calls were not independent", first)
		}
		close(proceed)

		for range 2 {
			o := <-done
			if o.err != nil {
				t.Fatalf("tool call %q: %v", o.want, o.err)
			}
			if o.got != o.want {
				t.Errorf("tool call %q returned %q (output crossed with the other call)", o.want, o.got)
			}
		}
	})
}

// TestConcurrentFirstDispatchSpawnsOnce proves a plugin process is spawned
// exactly once when several dispatches reach a not-yet-started instance at
// the same time. instance.start holds inst.mu across the whole
// dial-plus-handshake, so the losers of the race wait and then reuse the
// one connection.
//
// Two claims, checked separately. The sequential claim is deterministic on
// its own. The concurrent claim depends on goroutine scheduling, so the
// hammer run (-count=1000 -cpu=2 -race) is what makes it a real guard;
// -race also reports the unsynchronized field writes a missing lock causes.
func TestConcurrentFirstDispatchSpawnsOnce(t *testing.T) {
	const dispatchers = 8

	var dials atomic.Int64
	hooks := &Hooks{
		ToolExecuteBefore: func(_ context.Context, _ *Client, req *ToolExecuteBeforeRequest) (*ToolExecuteBeforeResponse, error) {
			return &ToolExecuteBeforeResponse{}, nil
		},
	}
	spec := Spec{
		Manifest: Manifest{Name: "counted", ProtocolVersion: ProtocolVersion, Hooks: hooks.hookList()},
		dial: func() (io.ReadWriteCloser, error) {
			dials.Add(1)
			hostSide, pluginSide := net.Pipe()
			go serve(pluginSide, Manifest{Name: "counted"}, hooks) //nolint:errcheck
			return hostSide, nil
		},
	}
	h := newTestHost(t, Options{HookTimeout: rendezvousTimeout}, spec)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range dispatchers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			h.ToolExecuteBefore(context.Background(), &ToolExecuteBeforeRequest{
				SessionID: "s1", CallID: "c", Tool: "bash", Args: json.RawMessage(`{}`),
			})
		}()
	}
	close(start)
	wg.Wait()
	if got := dials.Load(); got != 1 {
		t.Fatalf("%d concurrent dispatches dialed the plugin %d times, want exactly 1", dispatchers, got)
	}

	// A later dispatch reuses the same process too: a started instance is
	// never re-dialed.
	h.ToolExecuteBefore(context.Background(), &ToolExecuteBeforeRequest{
		SessionID: "s1", CallID: "c", Tool: "bash", Args: json.RawMessage(`{}`),
	})
	if got := dials.Load(); got != 1 {
		t.Fatalf("a dispatch after the race dialed again: %d dials, want 1", got)
	}
}
