//go:build unix

package server_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/process"
	"github.com/majorcontext/harness/internal/testpoll"
	"github.com/majorcontext/harness/protocol"
)

func TestCloseEndsAStartAtTheReadyGateAndStopsItsProcess(t *testing.T) {
	workDir := t.TempDir()
	r, url := boxWith(t, workDir, config.ProcessSpec{
		Command: []string{"sh", "-c", "echo up; sleep 100"}, ReadyRegex: "never", ReadyTimeoutS: 3600,
	})
	started := make(chan int, 1)
	go func() {
		resp, err := http.Post(url+"/processes/dev/start", "", nil)
		if err != nil {
			started <- 0
			return
		}
		_ = resp.Body.Close()
		started <- resp.StatusCode
	}()
	log := filepath.Join(workDir, ".harness", "proc", "dev.log")
	testpoll.Until(t, 30*time.Second, "the process never printed its first line", func() bool {
		b, _ := os.ReadFile(log)
		return strings.Contains(string(b), "up")
	})
	var list []process.Info
	want(t, "list status", call(t, "GET", url+"/processes", "", &list), http.StatusOK)
	pid := list[0].Status.PID
	if pid == 0 {
		t.Fatalf("list = %+v, want the started process with a pid", list)
	}

	ctx, cancel := context.WithCancel(context.Background())
	closed := make(chan error, 1)
	go func() { closed <- r.Close(ctx) }()
	errorRows(t, url, []errorRow{{"POST", "/processes/dev/stop", http.StatusServiceUnavailable, protocol.CodeDraining}})
	cancel()
	if err := <-closed; !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", err)
	}
	if code := <-started; code == http.StatusOK {
		t.Errorf("start at the ready gate = %d after Close, want it to fail", code)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("kill(%d, 0) = %v after Close returned, want ESRCH: Close stops the process that a route started", pid, err)
	}
	errorRows(t, url, []errorRow{{"POST", "/processes/dev/start", http.StatusServiceUnavailable, protocol.CodeDraining}})
}
