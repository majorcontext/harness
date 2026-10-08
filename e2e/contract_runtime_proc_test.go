package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

const devScript = "echo ready-line; echo second; sleep 100"

func devProcesses() map[string]any {
	return map[string]any{"processes": map[string]any{"dev": map[string]any{
		"command":         []string{"sh", "-c", devScript},
		"ready_regex":     "second",
		"ready_timeout_s": 10,
		"ports":           []int{3000},
	}}}
}

func TestContractRuntimeProcesses(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		for _, row := range []struct {
			name string
			run  func(t *testing.T, h host)
		}{
			{"process_tool_from_the_model", processToolFromModel},
			{"process_status_line_follows_a_start_by_one_turn", processStatusLine},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				row.run(t, h)
			})
		}
	})
	if os.Getenv(runtimeEnv) == "" {
		return
	}
	t.Run("runtime", func(t *testing.T) {
		for _, row := range []struct {
			name string
			run  func(t *testing.T)
		}{
			{"process_tool_needs_a_workdir", processNeedsWorkdir},
			{"process_close_stops_processes_when_its_ctx_ends", processCloseStops},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				row.run(t)
			})
		}
	})
}

// processHost opens h on a scripted model with the dev process configured.
func processHost(t *testing.T, h host, steps ...harnesstest.Step) (driver, *harnesstest.Server) {
	t.Helper()
	fake := harnesstest.New(t, steps...)
	return h.open(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(devProcesses())), nil), fake
}

// normProcessResult masks the fields of a process tool result that vary per
// run: the pid, the elapsed time, and the workdir inside the log path.
func normProcessResult(t *testing.T, workdir, content string) string {
	t.Helper()
	if real, err := filepath.EvalSymlinks(workdir); err == nil {
		content = strings.ReplaceAll(content, real, "<workdir>")
	}
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

// processToolFromModel runs every action of the process tool.
func processToolFromModel(t *testing.T, h host) {
	const prefix = "process: "
	proc := func(kv ...any) harnesstest.ToolCall { return ftTool("process", ftArgs(kv...)) }
	rtCommand := []string{"sh", "-c", "echo rt-up; sleep 100"}
	d, fake := processHost(t, h, toolChain(
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
		proc("action", "declare", "name", "web"),
		proc("action", "status"),
		proc("action", "explode", "name", "dev"),
		proc(),
	)...)
	id := d.Create(t)
	d.Submit(t, id, "go")
	d.WaitIdle(t, id)
	workdir := d.Workdir()

	// Known defect of serve, pinned: a stopped process still reports ready true.
	const log = `"log":"<workdir>/.harness/proc/dev.log"`
	want := []struct {
		isError bool
		content string
	}{
		{false, `[{"command":["sh","-c","` + devScript + `"],"name":"dev","origin":"config","ports":[3000],"ready_regex":"second","ready_timeout":"10s","status":{` + log + `,"name":"dev","ports":[3000],"ready":false}}]`},
		{false, `{` + log + `,"name":"dev","ports":[3000],"ready":false}`},
		{false, `{` + log + `,"name":"dev","ports":[3000],"ready":true,"state":"ready"}`},
		{false, `{` + log + `,"logs":"second","name":"dev","ports":[3000],"ready":true,"state":"ready"}`},
		{false, `{` + log + `,"name":"dev","ports":[3000],"ready":true,"state":"ready"}`},
		{false, `{"exit_code":-1,` + log + `,"name":"dev","ports":[3000],"ready":true,"state":"stopped"}`},
		{true, prefix + `unknown process "nope"`},
		{false, `{"name":"rt","ok":true,"origin":"runtime"}`},
		{false, `{"log":"<workdir>/.harness/proc/rt.log","name":"rt","ready":true,"state":"ready"}`},
		{false, `{"exit_code":-1,"log":"<workdir>/.harness/proc/rt.log","name":"rt","ready":true,"state":"stopped"}`},
		{true, prefix + `"dev" is a config-declared process and cannot be redeclared at runtime`},
		{true, prefix + `"dev" is config-declared and cannot be undeclared`},
		{false, `{"name":"rt","ok":true}`},
		{true, `process: command is required (non-empty argv)`},
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

func processStart(name string) harnesstest.ToolCall {
	return harnesstest.ToolCall{ID: "toolu_" + name, Name: "process", Input: map[string]any{"action": "start", "name": name}}
}

var statusLineRE = regexp.MustCompile(`\n\n<harness-engine-context>\n\[processes: dev ready :3000 since \S+Z log=\.harness/proc/dev\.log\]\n</harness-engine-context>$`)

func processStatusLine(t *testing.T, h host) {
	d, fake := processHost(t, h,
		harnesstest.Step{Name: "start", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{processStart("dev")}}},
		harnesstest.Step{Name: "started", Match: harnesstest.LastToolResult("process"), Reply: harnesstest.Reply{Text: "started"}},
		harnesstest.Step{Name: "again", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{processStart("nope")}}},
		harnesstest.Step{Name: "failed", Match: harnesstest.LastToolResult("process"), Reply: harnesstest.Reply{Text: "ok"}})
	id := d.Create(t)
	for _, text := range []string{"run", "again"} {
		d.Submit(t, id, text)
		d.WaitIdle(t, id)
	}
	reqs := fake.Requests()
	if len(reqs) != 4 {
		t.Fatalf("model requests = %d, want 4", len(reqs))
	}
	system := func(i int) string { return reqs[i].System }
	if strings.Contains(system(0), "[processes:") || system(1) != system(0) || !statusLineRE.MatchString(system(2)) || system(3) != system(2) {
		t.Errorf("status lines = %q, want none in the turn of the start, and one in each later turn",
			[]string{statusLineRE.FindString(system(0)), statusLineRE.FindString(system(1)), statusLineRE.FindString(system(2)), statusLineRE.FindString(system(3))})
	}
}

func processNeedsWorkdir(t *testing.T) {
	fake := harnesstest.New(t,
		harnesstest.Step{Name: "start", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{processStart("dev")}}},
		harnesstest.Step{Name: "failed", Match: harnesstest.LastToolResult("process"), Reply: harnesstest.Reply{Text: "ok"}})
	d := newRuntimeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(devProcesses())), false, "", nil)
	id := d.Create(t)
	d.Submit(t, id, "run")
	d.WaitIdle(t, id)
	results := toolResults(d.Messages(t, id))
	reqs := fake.Requests()
	if len(results) != 1 || !results[0].IsError || results[0].Content != "no such tool available: process" {
		t.Errorf("tool results = %v, want the error no such tool available: process", results)
	}
	if slices.Contains(reqs[0].Tools, "process") || strings.Contains(reqs[len(reqs)-1].System, "[processes:") {
		t.Errorf("tools %q, last system prompt %q: want no process tool and no status line", reqs[0].Tools, reqs[len(reqs)-1].System)
	}
}

func processCloseStops(t *testing.T) {
	d, fake := processHost(t, runtimeHost,
		harnesstest.Step{Name: "start", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{processStart("dev")}}},
		harnesstest.Step{Name: "hold", Match: harnesstest.LastToolResult("process"), Reply: harnesstest.Reply{Text: "working", Block: true}})
	id := d.Create(t)
	d.Submit(t, id, "go")
	if !fake.AwaitRequests(2, waitBound) {
		t.Fatal("the turn did not reach its second model request")
	}
	m := regexp.MustCompile(`"pid":(\d+),`).FindStringSubmatch(toolResults(d.Messages(t, id))[0].Content)
	if m == nil {
		t.Fatal("the start result has no pid")
	}
	pid, _ := strconv.Atoi(m[1])
	d.Restart(t, true)
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d after a Close whose ctx ended: kill 0 = %v, want ESRCH", pid, err)
	}
}
