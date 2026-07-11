package process

import (
	"net"
	"net/http"
	"time"
)

// readyPollInterval is how often the ready_port/ready_http gate re-checks
// after an unsuccessful attempt. Var so tests can shrink it; the
// production default (250ms) is modest enough not to meaningfully delay a
// process that becomes ready quickly, without hammering the target.
var readyPollInterval = 250 * time.Millisecond

// readyProbeTimeout bounds a single ready_port dial or ready_http GET
// attempt, so a connection that hangs (rather than failing fast) cannot
// stall a poll cycle well past the next tick.
const readyProbeTimeout = 2 * time.Second

// httpReadyClient is reused across every ready_http probe attempt (and
// every managed process using one) — a *http.Client is safe for
// concurrent use and there is no per-target state to keep separate.
var httpReadyClient = &http.Client{Timeout: readyProbeTimeout}

// pollReady is the shared driver behind the ready_port and ready_http
// gates: it calls check immediately, then again every interval, until
// check reports true (onMatch fires exactly once, then pollReady returns)
// or stop closes (the process exited before ever becoming ready — see
// Manager.spawn, which wires stop to the managedProcess's doneCh so this
// goroutine can never outlive the process it is polling). Unlike the
// ready_regex watcher — which matches inline as the process's own output
// streams through cmd.Stdout, with nothing to poll — a TCP dial or HTTP
// GET has no event to subscribe to, hence the poll loop. See
// docs/design/managed-processes.md for the rationale (a log-regex gate
// can match the wrong task's output in a multiplexed runner; a port/HTTP
// probe cannot).
func pollReady(stop <-chan struct{}, interval time.Duration, check func() bool, onMatch func()) {
	if interval <= 0 {
		interval = readyPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if check() {
			onMatch()
			return
		}
		select {
		case <-ticker.C:
		case <-stop:
			return
		}
	}
}

// checkPort reports whether a plain TCP dial to addr (host:port) succeeds
// right now. A connection failure of any kind (nothing listening yet,
// refused, timed out) is "not ready", never an error a caller has to
// handle.
func checkPort(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, readyProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// checkHTTP reports whether a GET to url returns any non-5xx status right
// now — the design's "the server answered at all" bar, deliberately
// looser than "returned 200": a 404 or a redirect still proves the
// process is up and serving. A request failure (connection refused, DNS,
// timeout) is "not ready", never an error a caller has to handle.
func checkHTTP(url string) bool {
	resp, err := httpReadyClient.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < http.StatusInternalServerError
}
