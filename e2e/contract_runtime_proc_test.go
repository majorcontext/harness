package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

const devScript = "echo ready-line; echo second; sleep 100"

func devProcesses() map[string]any {
	return map[string]any{"processes": map[string]any{"dev": map[string]any{
		"command":         []string{"sh", "-c", devScript},
		"ready_regex":     "ready-line",
		"ready_timeout_s": 10,
		"ports":           []int{3000},
	}}}
}

func TestContractRuntimeProcesses(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"process_http_lifecycle", processHTTPLifecycle},
		{"process_http_unknown_name_is_404", processHTTPUnknown},
		{"process_tool_from_the_model", processToolFromModel},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func processStatus(t *testing.T, d *httpDriver, method, path string) map[string]any {
	t.Helper()
	res := d.call(t, method, path, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("%s %s = %d %v\nstderr:\n%s", method, path, res.Status, res.Body, d.Stderr())
	}
	return bodyOf(t, res)
}

func pidOf(t *testing.T, status map[string]any) int64 {
	t.Helper()
	n, err := status["pid"].(json.Number).Int64()
	if err != nil || n <= 0 {
		t.Fatalf("status = %v, want a positive pid", status)
	}
	return n
}

func processHTTPLifecycle(t *testing.T) {
	workdir := runtimeWorkdir(t, nil)
	d, _ := startRuntime(t, workdir, devProcesses())
	logPath := filepath.Join(workdir, ".harness", "proc", "dev.log")

	res := d.call(t, http.MethodGet, "/process", nil)
	list, _ := res.Body.([]any)
	if res.Status != http.StatusOK || len(list) != 1 {
		t.Fatalf("GET /process = %d %v, want one declared process", res.Status, res.Body)
	}
	info := list[0].(map[string]any)
	status := info["status"].(map[string]any)
	if info["name"] != "dev" || info["origin"] != "config" || status["ready"] != false || status["state"] != nil || status["log"] != logPath {
		t.Errorf("never-started process = %v, want a config process with no state, not ready, log %s", info, logPath)
	}

	started := processStatus(t, d, http.MethodPost, "/process/dev/start")
	if started["state"] != "ready" || started["ready"] != true || started["log"] != logPath {
		t.Errorf("start = %v, want ready with log %s", started, logPath)
	}
	pid := pidOf(t, started)
	if again := processStatus(t, d, http.MethodPost, "/process/dev/start"); pidOf(t, again) != pid {
		t.Errorf("second start = %v, want the same running process (pid %d)", again, pid)
	}

	logs := processStatus(t, d, http.MethodGet, "/process/dev/logs")
	if logs["content"] != "ready-line\nsecond" {
		t.Errorf("logs content = %q, want both lines", logs["content"])
	}
	if tail := processStatus(t, d, http.MethodGet, "/process/dev/logs?tail=1"); tail["content"] != "second" {
		t.Errorf("logs tail=1 content = %q, want the last line", tail["content"])
	}

	restarted := processStatus(t, d, http.MethodPost, "/process/dev/restart")
	if restarted["state"] != "ready" || pidOf(t, restarted) == pid {
		t.Errorf("restart = %v, want a ready process with a new pid", restarted)
	}

	stopped := processStatus(t, d, http.MethodPost, "/process/dev/stop")
	if stopped["state"] != "stopped" {
		t.Errorf("stop = %v, want state stopped", stopped)
	}
	listed := d.call(t, http.MethodGet, "/process", nil).Body.([]any)[0].(map[string]any)["status"].(map[string]any)
	if listed["state"] != "stopped" {
		t.Errorf("GET /process after stop = %v, want state stopped", listed)
	}
}

func processHTTPUnknown(t *testing.T) {
	d, _ := startRuntime(t, runtimeWorkdir(t, nil), devProcesses())
	for _, call := range []struct{ method, path string }{
		{http.MethodPost, "/process/nope/start"},
		{http.MethodPost, "/process/nope/stop"},
		{http.MethodPost, "/process/nope/restart"},
		{http.MethodGet, "/process/nope/logs"},
	} {
		res := d.call(t, call.method, call.path, nil)
		if res.Status != http.StatusNotFound || bodyOf(t, res)["error"] != "no such process" {
			t.Errorf("%s %s = %d %v, want 404 no such process", call.method, call.path, res.Status, res.Body)
		}
	}
}

// normProcessResult masks the fields of a process tool result that vary per
// run: the pid, the elapsed time, and the workdir inside the log path.
func normProcessResult(t *testing.T, workdir, content string) string {
	t.Helper()
	content = strings.ReplaceAll(content, workdir, "<workdir>")
	var v any
	if json.Unmarshal([]byte(content), &v) != nil {
		return content
	}
	var strip func(v any)
	strip = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			delete(x, "pid")
			delete(x, "elapsed")
			for _, e := range x {
				strip(e)
			}
		case []any:
			for _, e := range x {
				strip(e)
			}
		}
	}
	strip(v)
	var out strings.Builder
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out.String())
}

func processToolFromModel(t *testing.T) {
	workdir := runtimeWorkdir(t, nil)
	proc := func(kv ...any) harnesstest.ToolCall { return ftTool("process", ftArgs(kv...)) }
	rtCommand := []string{"sh", "-c", "echo rt-up; sleep 100"}
	d, fake := startRuntime(t, workdir, devProcesses(), toolChain(
		proc("action", "list"),
		proc("action", "status", "name", "dev"),
		proc("action", "start", "name", "dev"),
		proc("action", "logs", "name", "dev", "tail", 1),
		proc("action", "restart", "name", "dev"),
		proc("action", "stop", "name", "dev"),
		proc("action", "start", "name", "nope"),
		proc("action", "declare", "name", "rt", "command", rtCommand, "ready_regex", "rt-up"),
		proc("action", "start", "name", "rt"),
		proc("action", "stop", "name", "rt"),
		proc("action", "declare", "name", "dev", "command", []string{"true"}),
		proc("action", "undeclare", "name", "dev"),
		proc("action", "undeclare", "name", "rt"),
		proc("action", "status"),
		proc("action", "explode", "name", "dev"),
		proc(),
	)...)
	id := runTurn(t, d, "go")

	// Known defects, pinned: a Manager error gets the "process:" prefix twice, and a stopped process still reports ready true.
	const log = `"log":"<workdir>/.harness/proc/dev.log"`
	want := []struct {
		isError bool
		content string
	}{
		{false, `[{"command":["sh","-c","` + devScript + `"],"name":"dev","origin":"config","ports":[3000],"ready_regex":"ready-line","ready_timeout":"10s","status":{` + log + `,"name":"dev","ports":[3000],"ready":false}}]`},
		{false, `{` + log + `,"name":"dev","ports":[3000],"ready":false}`},
		{false, `{` + log + `,"name":"dev","ports":[3000],"ready":true,"state":"ready"}`},
		{false, `{` + log + `,"logs":"second","name":"dev","ports":[3000],"ready":true,"state":"ready"}`},
		{false, `{` + log + `,"name":"dev","ports":[3000],"ready":true,"state":"ready"}`},
		{false, `{"exit_code":-1,` + log + `,"name":"dev","ports":[3000],"ready":true,"state":"stopped"}`},
		{true, `process: process: unknown process "nope"`},
		{false, `{"name":"rt","ok":true,"origin":"runtime"}`},
		{false, `{"log":"<workdir>/.harness/proc/rt.log","name":"rt","ready":true,"state":"ready"}`},
		{false, `{"exit_code":-1,"log":"<workdir>/.harness/proc/rt.log","name":"rt","ready":true,"state":"stopped"}`},
		{true, `process: process: "dev" is a config-declared process and cannot be redeclared at runtime`},
		{true, `process: process: "dev" is config-declared and cannot be undeclared`},
		{false, `{"name":"rt","ok":true}`},
		{true, `process: name is required for action "status"`},
		{true, `process: unknown action "explode"`},
		{true, `process: action is required`},
	}
	results := toolResults(d.Messages(t, id))
	if len(results) != len(want) {
		t.Fatalf("got %d tool results, want %d", len(results), len(want))
	}
	for i, w := range want {
		got := normProcessResult(t, workdir, results[i].Content)
		if got != w.content || results[i].IsError != w.isError {
			t.Errorf("tool result %d = (error %v) %s\n  want (error %v) %s", i+1, results[i].IsError, got, w.isError, w.content)
		}
	}
	if tools := fake.Requests()[0].Tools; !slices.Contains(tools, "process") {
		t.Errorf("model request tools = %v, want the process tool", tools)
	}
}
