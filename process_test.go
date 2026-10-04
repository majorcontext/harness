package harness_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

var pidRE = regexp.MustCompile(`"pid":(\d+),`)

func processCall(id, name string) harnesstest.Reply {
	return harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: id, Name: "process", Input: map[string]any{"action": "start", "name": name}}}}
}

// processRuntime runs dev (sleep 100 on :3000) in a temp WorkDir, or with no WorkDir.
func processRuntime(t *testing.T, workDir bool, tools []harness.Tool, steps ...harnesstest.Step) (*harness.Runtime, *harness.Session, *harnesstest.OpenAI, string) {
	t.Helper()
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...)
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	opts := harness.Options{Store: harness.NewMemStore(), Tools: tools, Config: config.Config{
		Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
			BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}},
		Processes: map[string]config.ProcessSpec{"dev": {Command: []string{"sleep", "100"}, Ports: []int{3000}}}}}
	if workDir {
		opts.WorkDir = t.TempDir()
	}
	r, err := harness.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	return r, sess, s, opts.WorkDir
}

func lastText(req harnesstest.Request) string {
	return req.Messages[len(req.Messages)-1].Parts[0].Text
}

func wantGone(t *testing.T, startResult string) {
	t.Helper()
	m := pidRE.FindStringSubmatch(startResult)
	if m == nil {
		t.Fatalf("start result %s has no pid", startResult)
	}
	pid, _ := strconv.Atoi(m[1])
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d after Close: kill 0 = %v, want ESRCH", pid, err)
	}
}

func TestConfiguredProcesses(t *testing.T) {
	statusRE := regexp.MustCompile(`\n\n<harness-engine-context>\n\[processes: dev ready :3000 since \S+Z log=\.harness/proc/dev\.log\]\n</harness-engine-context>$`)
	for _, workDir := range []bool{true, false} {
		t.Run(map[bool]string{true: "a WorkDir runs the process tool", false: "no WorkDir has no process tool"}[workDir], func(t *testing.T) {
			r, sess, s, dir := processRuntime(t, workDir, nil,
				harnesstest.Step{Name: "start", Match: harnesstest.LastUserText("run"), Reply: processCall("call_1", "dev")},
				harnesstest.Step{Name: "started", Match: harnesstest.LastToolResult("process"), Reply: harnesstest.Reply{Text: "started"}},
				harnesstest.Step{Name: "again", Match: harnesstest.LastUserText("again"), Reply: processCall("call_2", "nope")},
				harnesstest.Step{Name: "failed", Match: harnesstest.LastToolResult("process"), Reply: harnesstest.Reply{Text: "ok"}})
			converse(t, sess, "run", "again")
			reqs := s.Requests()
			got := lastText(reqs[1])
			closeRuntime(t, r)
			if !workDir {
				if got != "[tool error] "+noTool+"process" || slices.Contains(reqs[0].Tools, "process") || strings.Contains(reqs[2].System, "[processes:") {
					t.Errorf("tools %q, result %q, last system prompt %q: want no process tool and no status line", reqs[0].Tools, got, reqs[2].System)
				}
				return
			}
			want := `{"name":"dev","state":"ready","ready":true,"log":"` + dir + `/.harness/proc/dev.log","ports":[3000]}`
			if pidRE.ReplaceAllString(got, "") != want {
				t.Fatalf("start result = %s, want %s with a pid", got, want)
			}
			if !slices.Contains(reqs[0].Tools, "process") {
				t.Errorf("request tools = %q, want the process tool", reqs[0].Tools)
			}
			if reqs[1].System != reqs[0].System || strings.Contains(reqs[0].System, "[processes:") || !statusRE.MatchString(reqs[2].System) {
				t.Errorf("status lines = %q, want one only from the turn after the start, and one prompt for each turn",
					[]string{statusRE.FindString(reqs[0].System), statusRE.FindString(reqs[1].System), statusRE.FindString(reqs[2].System)})
			}
			if got := lastText(reqs[3]); got != `[tool error] process: unknown process "nope"` {
				t.Errorf("start of an unknown process = %q, want one process: prefix", got)
			}
			wantGone(t, got)
		})
	}
}

func TestCloseStopsProcessesWhenItsContextEnds(t *testing.T) {
	wait := newProbe("wait", true)
	r, sess, s, _ := processRuntime(t, true, []harness.Tool{wait},
		harnesstest.Step{Name: "start", Match: harnesstest.LastUserText("run"), Reply: processCall("call_1", "dev")},
		harnesstest.Step{Name: "wait", Match: harnesstest.LastToolResult("process"), Reply: harnesstest.Reply{
			ToolCalls: []harnesstest.ToolCall{{ID: "call_2", Name: "wait", Input: map[string]any{}}}}})
	if _, err := sess.Submit(bg, text("a", "run")); err != nil {
		t.Fatal(err)
	}
	<-wait.started
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := r.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want Canceled", err)
	}
	wantGone(t, lastText(s.Requests()[1]))
}
