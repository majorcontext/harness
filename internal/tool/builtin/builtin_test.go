package builtin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

var bg = context.Background()

type call struct {
	tool string
	args map[string]any
	// want is the result text, with <workdir> for the work directory. An
	// error result starts with "ERR ".
	want string
}

func c(tool, want string, kv ...any) call {
	args := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		args[kv[i].(string)] = kv[i+1]
	}
	return call{tool, args, want}
}

// chain runs one turn that makes each call in order, one call for each
// model request, in a runtime with workDir, and returns the result texts
// and the tool names of the first request.
func chain(t *testing.T, workDir string, cfg config.Config, calls []call) ([]string, []string) {
	t.Helper()
	var steps []harnesstest.Step
	for i, cl := range calls {
		steps = append(steps, harnesstest.Step{Name: fmt.Sprint("call", i), Match: answered(i),
			Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: fmt.Sprint("call_", i), Name: cl.tool, Input: cl.args}}}})
	}
	steps = append(steps, harnesstest.Step{Name: "done", Match: answered(len(calls)), Reply: harnesstest.Reply{Text: "done"}})
	r, s := start(t, workDir, cfg, steps...)
	return turn(t, r, "s1", "go"), s.Requests()[0].Tools
}

// start returns a runtime with workDir whose model replies with steps.
func start(t *testing.T, workDir string, cfg config.Config, steps ...harnesstest.Step) (*harness.Runtime, *harnesstest.OpenAI) {
	t.Helper()
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...)
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	cfg.Providers = map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
		BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), WorkDir: workDir, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(bg) })
	return r, s
}

// turn creates session id, runs one turn on text, and returns its tool result texts.
func turn(t *testing.T, r *harness.Runtime, id, text string) []string {
	t.Helper()
	sess, err := r.Create(bg, protocol.CreateSession{ID: id, Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Submit(bg, protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: text}}}); err != nil {
		t.Fatal(err)
	}
	var results []string
	for e, err := range sess.Events(bg, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			break
		}
		results = append(results, toolResults(t, e)...)
	}
	return results
}

// answered matches a request that holds n assistant messages.
func answered(n int) harnesstest.Matcher {
	return func(r harnesstest.Request) bool {
		got := 0
		for _, m := range r.Messages {
			if m.Role == "assistant" {
				got++
			}
		}
		return got == n
	}
}

func toolResults(t *testing.T, e protocol.Event) []string {
	t.Helper()
	if e.Kind != "item.completed" {
		return nil
	}
	var item struct {
		Message struct {
			Parts []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				IsError bool   `json:"is_error"`
			} `json:"parts"`
		} `json:"message"`
	}
	if err := json.Unmarshal(e.Data, &item); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range item.Message.Parts {
		if p.Type == "tool_result" {
			out = append(out, map[bool]string{true: "ERR "}[p.IsError]+p.Text)
		}
	}
	return out
}

func check(t *testing.T, dir string, calls []call, got []string) {
	t.Helper()
	if len(got) != len(calls) {
		t.Fatalf("results = %q, want %d", got, len(calls))
	}
	for i, cl := range calls {
		if want := strings.ReplaceAll(cl.want, "<workdir>", dir); got[i] != want {
			t.Errorf("call %d %s %v = %q, want %q", i, cl.tool, cl.args, got[i], want)
		}
	}
}

func TestToolsNeedAWorkDir(t *testing.T) {
	builtins := []string{"bash", "edit_file", "glob", "grep", "ls", "read_file", "read_tool_result", "session_info", "write_file"}
	for _, workDir := range []bool{true, false} {
		t.Run(fmt.Sprint("workdir=", workDir), func(t *testing.T) {
			dir, want := "", []string(nil)
			if workDir {
				dir, want = t.TempDir(), append([]string{"model", "process", "task"}, builtins...)
				slices.Sort(want)
			}
			_, tools := chain(t, dir, config.Config{}, nil)
			if !slices.Equal(tools, want) {
				t.Fatalf("tools = %q, want %q", tools, want)
			}
		})
	}
}

func TestGrepMarksTruncationOnlyWhenAMatchIsDropped(t *testing.T) {
	var all []string
	for i := 1; i <= 500; i++ {
		all = append(all, fmt.Sprintf("f.txt:%d:x", i))
	}
	exact := strings.Join(all, "\n")
	over := strings.ReplaceAll(exact, "f.txt", "g.txt") + "\n[truncated: showing 500 matches]"
	calls := []call{
		c("bash", "", "command", "mkdir proj && (yes x | head -500; echo y) > proj/f.txt && (yes x | head -501) > proj/g.txt"),
		c("grep", exact, "pattern", "^x$", "path", "proj/f.txt"),
		c("grep", over, "pattern", "^x$", "path", "proj/g.txt"),
	}
	dir := t.TempDir()
	got, _ := chain(t, dir, config.Config{}, calls)
	check(t, dir, calls, got)
}
