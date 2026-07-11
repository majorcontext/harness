// Manager-level integration tests for the ready_port/ready_http gates,
// following process_test.go's sanctioned real-subprocess exception: the
// managed "process" here is a harmless `sh -c "sleep 100"` — it never has
// to be the thing that opens the port or answers HTTP, since Manager's
// gate only cares whether the target is dialable/answering, wherever that
// comes from. That lets these tests control exactly when the target
// becomes reachable (opening a real net.Listener/httptest.Server from the
// test goroutine itself) without depending on any external tool (nc,
// python) being present in the test environment.
package process

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStartWithReadyPort_BlocksUntilPortOpen(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln.Close() // released: Start must block with nothing listening yet

	m := NewManager(dir, map[string]Def{
		"dev": {
			Command:      []string{"sh", "-c", "sleep 100"},
			ReadyPort:    port,
			ReadyTimeout: 5 * time.Second,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	t.Cleanup(func() { m.Stop(context.Background(), "dev") })

	done := make(chan Status, 1)
	errc := make(chan error, 1)
	go func() {
		st, err := m.Start(ctx, "dev")
		if err != nil {
			errc <- err
			return
		}
		done <- st
	}()

	// Confirm Start is genuinely blocked (real out-of-process spawn plus
	// at least one failed poll attempt) before opening the listener —
	// deadline-bound poll over observable state, this file's sanctioned
	// pattern for a real-subprocess boundary (see the package doc).
	waitForState(t, m, "dev", StateStarting, 3*time.Second)

	select {
	case st := <-done:
		t.Fatalf("Start returned %+v before the port ever opened", st)
	default:
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

	select {
	case err := <-errc:
		t.Fatalf("Start: %v", err)
	case st := <-done:
		if st.State != StateReady || !st.Ready {
			t.Fatalf("Start status = %+v, want ready", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not unblock after the port opened")
	}
}

func TestStartWithReadyPort_TimesOut(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // never re-listened: the gate must time out

	m := NewManager(dir, map[string]Def{
		"dev": {
			Command:      []string{"sh", "-c", "sleep 100"},
			ReadyPort:    port,
			ReadyTimeout: 50 * time.Millisecond,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() { m.Stop(context.Background(), "dev") })

	st, err := m.Start(ctx, "dev")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != StateRunning {
		t.Fatalf("Start status = %+v, want running (timed out but left running)", st)
	}
	if !strings.Contains(st.Note, "timed out") {
		t.Errorf("Note = %q, want a timeout note", st.Note)
	}

	st2, err := m.Status("dev")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st2.State != StateRunning {
		t.Fatalf("Status after timeout = %+v, want still running (never killed)", st2)
	}
}

func TestStartWithReadyHTTP_BlocksUntilServing(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	url := "http://" + ln.Addr().String() + "/healthz"
	ln.Close() // released: Start must block with nothing serving yet

	m := NewManager(dir, map[string]Def{
		"dev": {
			Command:      []string{"sh", "-c", "sleep 100"},
			ReadyHTTP:    url,
			ReadyTimeout: 5 * time.Second,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	t.Cleanup(func() { m.Stop(context.Background(), "dev") })

	done := make(chan Status, 1)
	errc := make(chan error, 1)
	go func() {
		st, err := m.Start(ctx, "dev")
		if err != nil {
			errc <- err
			return
		}
		done <- st
	}()

	waitForState(t, m, "dev", StateStarting, 3*time.Second)
	select {
	case st := <-done:
		t.Fatalf("Start returned %+v before anything was serving", st)
	default:
	}

	ln2, err := net.Listen("tcp", strings.TrimPrefix(strings.TrimSuffix(url, "/healthz"), "http://"))
	if err != nil {
		t.Fatalf("re-listening: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener.Close()
	srv.Listener = ln2
	srv.Start()
	defer srv.Close()

	select {
	case err := <-errc:
		t.Fatalf("Start: %v", err)
	case st := <-done:
		if st.State != StateReady || !st.Ready {
			t.Fatalf("Start status = %+v, want ready", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not unblock once the HTTP endpoint started answering")
	}
}
