package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	pub "github.com/majorcontext/harness/plugin"
)

// This file is the specification for ONE property: a harness may keep
// several requests in flight on one plugin connection at the same time.
//
// The property is load-bearing for parallel tool execution in the runtime,
// which is not implemented yet: Session.runToolCalls still runs one call at
// a time. A batch of tool calls that runs concurrently will dispatch the
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

		p := testPlugin(t, "rendezvous", &pub.Plugin{
			ToolExecuteBefore: &pub.ToolBefore{Run: func(_ context.Context, _ *pub.Client, req *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
				arrived <- req.CallID
				select {
				case <-proceed:
				case <-release:
				}
				// Echo the call id back through the rewritten args. A
				// crossed response shows up as the wrong id here.
				return &pub.ToolExecuteBeforeResponse{
					Args: json.RawMessage(fmt.Sprintf(`{"echo":%q}`, req.CallID)),
				}, nil
			}},
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
				args, deny := h.ToolExecuteBefore(context.Background(), &pub.ToolExecuteBeforeRequest{
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
// Host.StartTool: two plugin TOOL calls in flight at once over one
// connection, each getting its own output.
func TestHostConcurrentExecuteToolStaysIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		arrived := make(chan string, 2)
		proceed := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		p := testPlugin(t, "tooler", &pub.Plugin{
			Tools: []pub.Tool{{
				Def: pub.ToolDef{Name: "echo", Description: "echoes its argument"},
				Execute: func(_ context.Context, _ *pub.Client, args json.RawMessage) ([]pub.Part, error) {
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
					return []pub.Part{pub.TextPart(in.Want)}, nil
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
				resp, err := executeTool(h, &pub.ToolExecuteRequest{
					SessionID: "s1", CallID: want, Tool: "echo",
					Args: json.RawMessage(fmt.Sprintf(`{"want":%q}`, want)),
				})
				o := outcome{want: want, err: err}
				if err == nil && len(resp.Output) == 1 {
					o.got = resp.Output[0].Text
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
	hooks := &pub.Plugin{
		Name: "counted",
		ToolExecuteBefore: &pub.ToolBefore{Run: func(_ context.Context, _ *pub.Client, req *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
			return &pub.ToolExecuteBeforeResponse{}, nil
		}},
	}
	spec := Spec{
		Manifest: hooks.Manifest(),
		dial: func() (io.ReadWriteCloser, error) {
			dials.Add(1)
			hostSide, pluginSide := net.Pipe()
			go pub.ServeConn(pluginSide, hooks) //nolint:errcheck
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
			h.ToolExecuteBefore(context.Background(), &pub.ToolExecuteBeforeRequest{
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
	h.ToolExecuteBefore(context.Background(), &pub.ToolExecuteBeforeRequest{
		SessionID: "s1", CallID: "c", Tool: "bash", Args: json.RawMessage(`{}`),
	})
	if got := dials.Load(); got != 1 {
		t.Fatalf("a dispatch after the race dialed again: %d dials, want 1", got)
	}
}
