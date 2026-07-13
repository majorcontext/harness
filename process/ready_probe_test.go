package process

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// TestPollReady_WaitsForCheckThenMatches drives pollReady's polling logic
// entirely on fake time inside a synctest bubble: the check function is
// gated on test-controlled state (an atomic.Bool), never a raw sleep. This
// is the shared driver behind both the ready_port and ready_http gates
// (see checkPort/checkHTTP below and docs/design/managed-processes.md for
// why a poll — rather than the ready_regex watcher's event-driven match —
// is the right shape for a probe that has nothing to subscribe to).
func TestPollReady_WaitsForCheckThenMatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ready atomic.Bool
		var attempts atomic.Int64
		check := func() bool {
			attempts.Add(1)
			return ready.Load()
		}
		matched := make(chan struct{})
		stop := make(chan struct{})
		defer close(stop)

		go pollReady(stop, 250*time.Millisecond, check, func() { close(matched) })

		// Let the goroutine make its first (immediate) attempt and settle
		// into its poll-interval wait — every goroutine in the bubble
		// durably blocked is exactly when synctest lets us observe this.
		synctest.Wait()
		if attempts.Load() != 1 {
			t.Fatalf("attempts after first settle = %d, want 1 (one immediate check, no match yet)", attempts.Load())
		}
		select {
		case <-matched:
			t.Fatal("matched before check ever returned true")
		default:
		}

		ready.Store(true)
		// Blocking on matched is itself the synchronization: the only two
		// goroutines in the bubble are this one (blocked here) and the
		// poller (blocked on its ticker) — synctest fast-forwards fake
		// time to the next tick deterministically, no wall-clock cost.
		<-matched
		if attempts.Load() < 2 {
			t.Fatalf("attempts = %d, want at least 2 (a poll after the interval elapsed)", attempts.Load())
		}
	})
}

// TestPollReady_StopsOnStopChannel encodes the "process exited before ever
// becoming ready" case: pollReady must give up (not leak the goroutine)
// once stop closes, never calling onMatch.
func TestPollReady_StopsOnStopChannel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		check := func() bool { return false }
		onMatch := func() { t.Fatal("onMatch called after stop") }
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			pollReady(stop, 250*time.Millisecond, check, onMatch)
			close(done)
		}()
		synctest.Wait()
		close(stop)
		<-done
	})
}

// TestCheckPort exercises the real TCP-dial probe against a real listener
// (network I/O, hence outside any synctest bubble — see AGENTS.md): false
// while nothing is listening on the port, true once a listener accepts
// connections on it. No timing is involved (dial either succeeds or fails
// synchronously against the two fixed states below), so no sleep is
// needed.
func TestCheckPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln.Close() // released: nothing listening yet

	if checkPort(addr) {
		t.Fatal("checkPort = true with nothing listening")
	}

	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("re-listening on %s: %v", addr, err)
	}
	defer ln2.Close()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	if !checkPort(addr) {
		t.Fatal("checkPort = false once a listener is accepting")
	}
}

// TestCheckHTTP exercises the real GET probe: false on a 5xx response,
// true on anything else (2xx/3xx/4xx all count as "the server answered",
// per the design's "any non-5xx status" rule) — gated on test-controlled
// handler state, never a sleep.
func TestCheckHTTP(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	if checkHTTP(srv.URL) {
		t.Fatal("checkHTTP = true while the handler answers 503")
	}

	status.Store(http.StatusNotFound)
	if !checkHTTP(srv.URL) {
		t.Fatal("checkHTTP = false for a 404 (non-5xx must count as ready)")
	}

	status.Store(http.StatusOK)
	if !checkHTTP(srv.URL) {
		t.Fatal("checkHTTP = false for a 200")
	}
}

// TestCheckHTTP_UnreachableIsFalse ensures a connection failure (nothing
// listening) is treated as "not ready yet", not an error that propagates.
func TestCheckHTTP_UnreachableIsFalse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if checkHTTP("http://" + addr) {
		t.Fatal("checkHTTP = true against an address nothing is listening on")
	}
}
