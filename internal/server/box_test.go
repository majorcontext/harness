package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), WorkDir: workDir, Config: config.Config{
		Processes: map[string]config.ProcessSpec{"dev": {Command: []string{"sh", "-c", "echo one; echo two; sleep 100"}, ReadyRegex: "two"}}}})
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

func status(t *testing.T, method, url string) process.Status {
	t.Helper()
	var st process.Status
	if code := call(t, method, url, "", &st); code != http.StatusOK {
		t.Fatalf("%s %s = %d", method, url, code)
	}
	return st
}

func TestProcessRoutes(t *testing.T) {
	url := serveBox(t, t.TempDir())
	var list []process.Info
	want(t, "list status", call(t, "GET", url+"/processes", "", &list), http.StatusOK)
	if len(list) != 1 || list[0].Name != "dev" || list[0].Origin != process.OriginConfig || list[0].Status.State != "" {
		t.Errorf("list = %+v, want dev from config, never started", list)
	}
	started := status(t, "POST", url+"/processes/dev/start")
	if started.State != process.StateReady || started.PID == 0 {
		t.Errorf("start = %+v, want ready", started)
	}
	want(t, "pid of a second start", status(t, "POST", url+"/processes/dev/start").PID, started.PID)
	var logs struct {
		Content string         `json:"content"`
		Status  process.Status `json:"status"`
	}
	want(t, "logs status", call(t, "GET", url+"/processes/dev/logs?tail=1", "", &logs), http.StatusOK)
	if logs.Content != "two" || logs.Status.PID != started.PID {
		t.Errorf("logs = %+v, want the last line and the status", logs)
	}
	if restarted := status(t, "POST", url+"/processes/dev/restart"); restarted.State != process.StateReady || restarted.PID == started.PID {
		t.Errorf("restart = %+v, want ready with a new pid", restarted)
	}
	want(t, "stop", status(t, "POST", url+"/processes/dev/stop").State, process.StateStopped)
	errorRows(t, url, []errorRow{
		{"POST", "/processes/nope/start", http.StatusNotFound, protocol.CodeProcessNotFound},
		{"POST", "/processes/nope/stop", http.StatusNotFound, protocol.CodeProcessNotFound},
		{"POST", "/processes/nope/restart", http.StatusNotFound, protocol.CodeProcessNotFound},
		{"GET", "/processes/nope/logs", http.StatusNotFound, protocol.CodeProcessNotFound},
		{"POST", "/processes/dev/explode", http.StatusNotFound, protocol.CodeInvalidRequest},
	})
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

func TestWorkspaceChanges(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.html"), []byte("<b>&</b>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	url := serveBox(t, dir)
	resp, err := http.Get(url + "/workspace/changes?scope=uncommitted")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var got protocol.WorkspaceChanges
	if err := json.Unmarshal(body, &got); err != nil || resp.StatusCode != http.StatusOK || len(got.Files) != 1 || got.Files[0].Path != "a.html" {
		t.Fatalf("GET /workspace/changes = %d %s, want a.html added", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "+<b>&</b>") {
		t.Errorf("body escapes the patch as HTML: %s", body)
	}
	errorRows(t, url, []errorRow{
		{"GET", "/workspace/changes?scope=bogus", http.StatusBadRequest, protocol.CodeInvalidRequest},
		{"GET", "/workspace/changes?dir=/nonexistent", http.StatusBadRequest, protocol.CodeInvalidRequest},
		{"GET", "/workspace/changes", http.StatusConflict, protocol.CodeNoBase},
	})
	plain := serveBox(t, t.TempDir())
	errorRows(t, plain, []errorRow{{"GET", "/workspace/changes?dir=.", http.StatusConflict, protocol.CodeNotAGitRepo}})
}
