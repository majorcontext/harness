package harness_test

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"syscall"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func TestConfiguredProcesses(t *testing.T) {
	pidRE := regexp.MustCompile(`"pid":(\d+),`)
	statusRE := regexp.MustCompile(`\n\n\[processes: dev ready :3000 since \S+Z log=\.harness/proc/dev\.log\]$`)
	for _, workDir := range []bool{true, false} {
		t.Run(map[bool]string{true: "a WorkDir runs the process tool", false: "no WorkDir has no process tool"}[workDir], func(t *testing.T) {
			s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{},
				harnesstest.Step{Name: "start", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{
					ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "process", Input: map[string]any{"action": "start", "name": "dev"}}}}},
				harnesstest.Step{Name: "started", Match: harnesstest.LastToolResult("process"), Reply: harnesstest.Reply{Text: "started"}},
				harnesstest.Step{Name: "again", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "ok"}})
			t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
			opts := harness.Options{Store: harness.NewMemStore(), Config: config.Config{
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
			converse(t, sess, "run", "again")
			reqs := s.Requests()
			got := reqs[1].Messages[len(reqs[1].Messages)-1].Parts[0].Text
			m := pidRE.FindStringSubmatch(got)
			closeRuntime(t, r)
			if !workDir {
				if got != "[tool error] "+noTool+"process" || slices.Contains(reqs[0].Tools, "process") || statusRE.MatchString(reqs[2].System) {
					t.Errorf("tools %q, result %q, last system prompt %q: want no process tool and no status line", reqs[0].Tools, got, reqs[2].System)
				}
				return
			}
			want := `{"name":"dev","state":"ready","ready":true,"log":"` + opts.WorkDir + `/.harness/proc/dev.log","ports":[3000]}`
			if m == nil || pidRE.ReplaceAllString(got, "") != want {
				t.Fatalf("start result = %s, want %s with a pid", got, want)
			}
			if !slices.Contains(reqs[0].Tools, "process") {
				t.Errorf("request tools = %q, want the process tool", reqs[0].Tools)
			}
			if reqs[1].System != reqs[0].System || statusRE.MatchString(reqs[0].System) || !statusRE.MatchString(reqs[2].System) {
				t.Errorf("status lines = %q, want one only from the turn after the start, and one prompt for each turn",
					[]string{statusRE.FindString(reqs[0].System), statusRE.FindString(reqs[1].System), statusRE.FindString(reqs[2].System)})
			}
			pid, _ := strconv.Atoi(m[1])
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("process %d after Close: kill 0 = %v, want ESRCH", pid, err)
			}
		})
	}
}
