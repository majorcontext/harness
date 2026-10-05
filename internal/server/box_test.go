package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/protocol"
)

// serveBox serves a Runtime with process dev in workDir, which can be empty.
func serveBox(t *testing.T, workDir string) string {
	t.Helper()
	_, url := box(t, workDir)
	return url
}

func box(t *testing.T, workDir string) (*harness.Runtime, string) {
	t.Helper()
	return boxWith(t, workDir, config.ProcessSpec{Command: []string{"sh", "-c", "echo one; echo two; sleep 100"}, ReadyRegex: "two"})
}

// boxWith serves a Runtime with process dev as dev.
func boxWith(t *testing.T, workDir string, dev config.ProcessSpec) (*harness.Runtime, string) {
	t.Helper()
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), WorkDir: workDir, Config: config.Config{
		Processes: map[string]config.ProcessSpec{"dev": dev}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(func() {
		_ = r.Close(context.Background())
		srv.Close()
	})
	return r, srv.URL
}

func TestProcessActionsAfterCloseAreDraining(t *testing.T) {
	r, url := box(t, t.TempDir())
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	errorRows(t, url, []errorRow{
		{"POST", "/processes/dev/start", http.StatusServiceUnavailable, protocol.CodeDraining},
		{"POST", "/processes/dev/restart", http.StatusServiceUnavailable, protocol.CodeDraining},
		{"POST", "/processes/dev/stop", http.StatusServiceUnavailable, protocol.CodeDraining},
	})
	var list []process.Info
	want(t, "list status", call(t, "GET", url+"/processes", "", &list), http.StatusOK)
	if len(list) != 1 || list[0].Status.State != "" {
		t.Errorf("list after Close = %+v, want dev never started", list)
	}
}

func TestBoxRoutesWithoutAWorkDir(t *testing.T) {
	url := serveBox(t, "")
	var list []process.Info
	if code := call(t, "GET", url+"/processes", "", &list); code != http.StatusOK || list == nil || len(list) != 0 {
		t.Errorf("GET /processes = %d %v, want 200 []", code, list)
	}
	errorRows(t, url, []errorRow{
		{"POST", "/processes/dev/start", http.StatusNotFound, protocol.CodeProcessNotFound},
		{"GET", "/processes/dev/logs", http.StatusNotFound, protocol.CodeProcessNotFound},
		{"GET", "/workspace/changes", http.StatusNotFound, protocol.CodeInvalidRequest},
	})
}

type errorRow struct {
	method, path string
	status       int
	code         string
}

func errorRows(t *testing.T, url string, rows []errorRow) {
	t.Helper()
	for _, row := range rows {
		var got protocol.ErrorBody
		if code := call(t, row.method, url+row.path, "", &got); code != row.status || got.Error.Code != row.code {
			t.Errorf("%s %s = %d %+v, want %d %s", row.method, row.path, code, got.Error, row.status, row.code)
		}
	}
}
