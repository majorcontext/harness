package builtin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
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

// The file_tools_* rows are the contract rows of the engine, with the
// result texts of their goldens.
var contractRows = []struct {
	name  string
	calls []call
}{
	{"file_tools_roundtrip", []call{
		c("write_file", "wrote 6 bytes to <workdir>/proj/sub/c.txt", "path", "proj/sub/c.txt", "content", "gamma\n"),
		c("write_file", "wrote 16 bytes to <workdir>/proj/b.txt", "path", "proj/b.txt", "content", "beta\nneedle two\n"),
		c("write_file", "wrote 30 bytes to <workdir>/proj/a.txt", "path", "proj/a.txt", "content", "alpha\nNeedle one\nneedle three\n"),
		c("read_file", "1→alpha\n2→Needle one\n3→needle three", "path", "proj/a.txt"),
		c("read_file", "2→Needle one\n[truncated: showing lines 2-2 of 3]", "path", "proj/a.txt", "offset", 2, "limit", 1),
		c("edit_file", "replaced 1 occurrence(s) in <workdir>/proj/a.txt", "path", "proj/a.txt", "old_string", "alpha", "new_string", "ALPHA"),
		c("edit_file", "ERR edit_file: old_string not found in <workdir>/proj/a.txt", "path", "proj/a.txt", "old_string", "absent", "new_string", "x"),
		c("grep", "a.txt:3:needle three\nb.txt:2:needle two", "pattern", "needle", "path", "proj"),
		c("grep", "a.txt:2:Needle one\na.txt:3:needle three", "pattern", "needle", "path", "proj", "case_insensitive", true, "glob", "a.*"),
		c("glob", "a.txt\nb.txt\nsub/c.txt", "pattern", "**/*.txt", "path", "proj"),
		c("ls", "sub/\na.txt\nb.txt", "path", "proj"),
		c("read_file", "ERR read_file: stat <workdir>/proj/missing.txt: no such file or directory", "path", "proj/missing.txt"),
	}},
	{"file_tools_write_edit_guards", []call{
		c("write_file", "wrote 12 bytes to <workdir>/proj/g.txt", "path", "proj/g.txt", "content", "one\ntwo\ntwo\n"),
		c("bash", "", "command", "printf 'seed\\n' > proj/seed.txt"),
		c("write_file", "ERR write_file: <workdir>/proj/seed.txt exists and has not been read this session; read it first (or use edit_file)", "path", "proj/seed.txt", "content", "x"),
		c("read_file", "1→seed", "path", "proj/seed.txt"),
		c("write_file", "wrote 9 bytes to <workdir>/proj/seed.txt", "path", "proj/seed.txt", "content", "replaced\n"),
		c("bash", "", "command", "printf 'outside\\n' > proj/seed.txt"),
		c("write_file", "ERR write_file: <workdir>/proj/seed.txt changed on disk since it was read; read it again before overwriting", "path", "proj/seed.txt", "content", "again"),
		c("edit_file", "ERR edit_file: old_string matches 2 times in <workdir>/proj/g.txt; provide more surrounding context to make it unique, or set replace_all to true", "path", "proj/g.txt", "old_string", "two", "new_string", "2"),
		c("edit_file", "replaced 2 occurrence(s) in <workdir>/proj/g.txt", "path", "proj/g.txt", "old_string", "two", "new_string", "2", "replace_all", true),
		c("read_file", "1→one\n2→2\n3→2", "path", "proj/g.txt"),
		c("edit_file", "ERR edit_file: old_string and new_string are identical", "path", "proj/g.txt", "old_string", "one", "new_string", "one"),
		c("edit_file", "ERR edit_file: open <workdir>/proj/nope.txt: no such file or directory", "path", "proj/nope.txt", "old_string", "a", "new_string", "b"),
		c("write_file", "ERR write_file: missing path or content argument", "path", "proj/x.txt"),
		c("edit_file", "ERR edit_file: missing path or old_string argument", "path", "proj/g.txt", "old_string", "", "new_string", "x"),
	}},
	{"file_tools_read_edges", []call{
		c("write_file", "wrote 0 bytes to <workdir>/proj/empty.txt", "path", "proj/empty.txt", "content", ""),
		c("read_file", "(empty file)", "path", "proj/empty.txt"),
		c("write_file", "wrote 6 bytes to <workdir>/proj/three.txt", "path", "proj/three.txt", "content", "a\nb\nc\n"),
		c("read_file", "ERR read_file: offset 9 is past end of file (3 lines)", "path", "proj/three.txt", "offset", 9),
		c("read_file", "ERR read_file: <workdir>/proj is a directory", "path", "proj"),
		c("read_file", "ERR read_file: missing path argument"),
		c("read_file", "2→b\n3→c", "path", "proj/three.txt", "offset", 2),
		c("read_file", "1→a\n[truncated: showing lines 1-1 of 3]", "path", "proj/three.txt", "limit", 1),
	}},
	{"file_tools_search_edges", []call{
		c("write_file", "wrote 11 bytes to <workdir>/proj/one.txt", "path", "proj/one.txt", "content", "Alpha\nbeta\n"),
		c("bash", "", "command", "printf 'alpha\\0' > proj/bin.dat && mkdir proj/empty"),
		c("grep", "ERR grep: invalid pattern \"(\": error parsing regexp: missing closing ): `(`", "pattern", "(", "path", "proj"),
		c("grep", "(no matches)", "pattern", "zzz", "path", "proj"),
		c("grep", "one.txt:1:Alpha", "pattern", "alpha", "path", "proj", "case_insensitive", true),
		c("grep", "one.txt:1:Alpha", "pattern", "alpha", "path", "proj/one.txt", "case_insensitive", true),
		c("grep", "ERR grep: stat <workdir>/proj/nope: no such file or directory", "pattern", "x", "path", "proj/nope"),
		c("grep", "ERR grep: missing pattern argument"),
		c("glob", "(no matches)", "pattern", "proj/*.md"),
		c("glob", "proj/one.txt", "pattern", "proj/*.txt"),
		c("glob", "ERR glob: stat <workdir>/proj/nope: no such file or directory", "pattern", "*", "path", "proj/nope"),
		c("glob", "ERR glob: missing pattern argument"),
		c("ls", "empty/\nbin.dat\none.txt", "path", "proj"),
		c("ls", "(empty directory)", "path", "proj/empty"),
		c("ls", "ERR ls: open <workdir>/proj/nope: no such file or directory", "path", "proj/nope"),
	}},
	{"file_size_cap", []call{
		c("write_file", "wrote 2 bytes to <workdir>/s.txt", "path", "s.txt", "content", "s\n"),
		c("bash", "", "command", "dd if=/dev/zero of=big.txt bs=1 count=0 seek=20971521 2>/dev/null && dd if=/dev/zero of=s.txt bs=1 count=0 seek=20971521 2>/dev/null"),
		c("read_file", "ERR read_file: <workdir>/big.txt: read <workdir>/big.txt: file is above the 20971520-byte limit of the file tools; read part of it with grep or bash", "path", "big.txt"),
		c("edit_file", "ERR edit_file: read <workdir>/big.txt: file is above the 20971520-byte limit of the file tools; read part of it with grep or bash", "path", "big.txt", "old_string", "a", "new_string", "b"),
		c("write_file", "ERR write_file: <workdir>/s.txt changed on disk since it was read; read it again before overwriting", "path", "s.txt", "content", "x"),
	}},
	{"bash_output_and_exit_status", []call{
		c("bash", "hi\n", "command", "echo hi"),
		c("bash", "ERR out\n\nexit status 3", "command", "echo out; exit 3"),
		c("bash", "ERR exit status 4", "command", "exit 4"),
		c("bash", "ERR bash: missing command argument"),
		c("bash", "x\n\n[note: a backgrounded process may still be running; its output after this point was not captured]", "command", "echo x; sleep 3 &"),
	}},
}

func TestFileSearchAndShellTools(t *testing.T) {
	for _, row := range contractRows {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			got, _ := chain(t, dir, config.Config{}, row.calls)
			check(t, dir, row.calls, got)
		})
	}
}

func TestReadFileImage(t *testing.T) {
	dir := t.TempDir()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []call{c("read_file", fmt.Sprintf("image (image/png), %d bytes, 3x2 pixels", b.Len()), "path", "a.txt")}
	got, _ := chain(t, dir, config.Config{}, calls)
	check(t, dir, calls, got)
}

func TestToolsNeedAWorkDir(t *testing.T) {
	builtins := []string{"bash", "edit_file", "glob", "grep", "ls", "read_file", "read_tool_result", "session_info", "write_file"}
	for _, workDir := range []bool{true, false} {
		t.Run(fmt.Sprint("workdir=", workDir), func(t *testing.T) {
			dir, want := "", []string(nil)
			if workDir {
				dir, want = t.TempDir(), append([]string{"process", "task"}, builtins...)
				slices.Sort(want)
			}
			_, tools := chain(t, dir, config.Config{}, nil)
			if !slices.Equal(tools, want) {
				t.Fatalf("tools = %q, want %q", tools, want)
			}
		})
	}
}

func TestWriteGuardBelongsToOneSession(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	call := func(name string, in map[string]any) harnesstest.Reply {
		return harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: name, Name: name, Input: in}}}
	}
	r, _ := start(t, dir, config.Config{},
		harnesstest.Step{Name: "read", Match: harnesstest.LastUserText("read"), Reply: call("read_file", map[string]any{"path": "f.txt"})},
		harnesstest.Step{Name: "read done", Match: harnesstest.LastToolResult("read_file"), Reply: harnesstest.Reply{Text: "done"}},
		harnesstest.Step{Name: "write", Match: harnesstest.LastUserText("write"), Reply: call("write_file", map[string]any{"path": "f.txt", "content": "y"})},
		harnesstest.Step{Name: "write done", Match: harnesstest.LastToolResult("write_file"), Reply: harnesstest.Reply{Text: "done"}})
	turn(t, r, "a", "read")
	want := "ERR write_file: " + filepath.Join(dir, "f.txt") + " exists and has not been read this session; read it first (or use edit_file)"
	if got := turn(t, r, "b", "write"); !slices.Equal(got, []string{want}) {
		t.Fatalf("write_file in another session = %q, want %q", got, want)
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
