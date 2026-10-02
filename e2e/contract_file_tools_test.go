package e2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

type captureWorkdir struct{ dst *string }

func (a captureWorkdir) run(_ *testing.T, r *run) {
	*a.dst = r.drv.Workdir()
}

// scrubWorkdir rewrites the session workdir to "<workdir>" in every string of
// the observation: tool results quote the absolute path they resolved.
func scrubWorkdir(t *testing.T, obs observation, workdir string) observation {
	t.Helper()
	raw := string(mustJSON(t, obs))
	paths := []string{workdir}
	if real, err := filepath.EvalSymlinks(workdir); err == nil && real != workdir {
		paths = []string{real, workdir}
	}
	for _, p := range paths {
		quoted, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		raw = strings.ReplaceAll(raw, strings.Trim(string(quoted), `"`), "<workdir>")
	}
	var out observation
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal scrubbed observation: %v", err)
	}
	return out
}

func assistantTurns(n int) harnesstest.Matcher {
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

// toolChain scripts one tool call per model request, in order, then a final
// text reply. One call at a time keeps file mtimes and results ordered.
func toolChain(calls ...harnesstest.ToolCall) []harnesstest.Step {
	steps := make([]harnesstest.Step, 0, len(calls)+1)
	for i, c := range calls {
		c.ID = fmt.Sprintf("toolu_%d", i+1)
		steps = append(steps, harnesstest.Step{
			Name:  fmt.Sprintf("call%d", i+1),
			Match: assistantTurns(i),
			Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{c}},
		})
	}
	return append(steps, harnesstest.Step{Name: "done", Match: assistantTurns(len(calls)), Reply: harnesstest.Reply{Text: "done"}})
}

func TestContractFileTools(t *testing.T) {
	skipShort(t)
	tool := func(name string, in map[string]any) harnesstest.ToolCall {
		return harnesstest.ToolCall{Name: name, Input: in}
	}
	write := func(path, content string) harnesstest.ToolCall {
		return tool("write_file", map[string]any{"path": path, "content": content})
	}
	read := func(in map[string]any) harnesstest.ToolCall { return tool("read_file", in) }
	edit := func(path, old, repl string) harnesstest.ToolCall {
		return tool("edit_file", map[string]any{"path": path, "old_string": old, "new_string": repl})
	}
	bash := func(cmd string) harnesstest.ToolCall { return tool("bash", map[string]any{"command": cmd}) }
	m := func(kv ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	// Files are written newest-first in alphabetical order, so glob's
	// newest-first order and its alphabetical tie-break agree.
	table := []scenario{
		{
			name: "file_tools_roundtrip",
			model: toolChain(
				write("proj/sub/c.txt", "gamma\n"),
				write("proj/b.txt", "beta\nneedle two\n"),
				write("proj/a.txt", "alpha\nNeedle one\nneedle three\n"),
				read(m("path", "proj/a.txt")),
				read(m("path", "proj/a.txt", "offset", 2, "limit", 1)),
				edit("proj/a.txt", "alpha", "ALPHA"),
				edit("proj/a.txt", "absent", "x"),
				read(m("path", "proj/a.txt")),
				tool("grep", m("pattern", "needle", "path", "proj")),
				tool("grep", m("pattern", "needle", "path", "proj", "case_insensitive", true, "glob", "a.*")),
				tool("glob", m("pattern", "**/*.txt", "path", "proj")),
				tool("ls", m("path", "proj")),
				read(m("path", "proj/missing.txt")),
			),
		},
		{
			name: "file_tools_write_edit_guards",
			model: toolChain(
				write("proj/g.txt", "one\ntwo\ntwo\n"),
				bash("printf 'seed\\n' > proj/seed.txt"),
				write("proj/seed.txt", "x"),
				read(m("path", "proj/seed.txt")),
				write("proj/seed.txt", "replaced\n"),
				bash("printf 'outside\\n' > proj/seed.txt"),
				write("proj/seed.txt", "again"),
				edit("proj/g.txt", "two", "2"),
				tool("edit_file", m("path", "proj/g.txt", "old_string", "two", "new_string", "2", "replace_all", true)),
				read(m("path", "proj/g.txt")),
				edit("proj/g.txt", "one", "one"),
				edit("proj/nope.txt", "a", "b"),
				tool("write_file", m("path", "proj/x.txt")),
				edit("proj/g.txt", "", "x"),
			),
		},
		{
			name: "file_tools_read_edges",
			model: toolChain(
				write("proj/empty.txt", ""),
				read(m("path", "proj/empty.txt")),
				write("proj/three.txt", "a\nb\nc\n"),
				read(m("path", "proj/three.txt", "offset", 9)),
				read(m("path", "proj")),
				read(m()),
				read(m("path", "proj/three.txt", "offset", 2)),
				read(m("path", "proj/three.txt", "limit", 1)),
			),
		},
		{
			name: "file_tools_search_edges",
			model: toolChain(
				write("proj/one.txt", "Alpha\nbeta\n"),
				bash("printf 'alpha\\0' > proj/bin.dat && mkdir proj/empty"),
				tool("grep", m("pattern", "(", "path", "proj")),
				tool("grep", m("pattern", "zzz", "path", "proj")),
				tool("grep", m("pattern", "alpha", "path", "proj", "case_insensitive", true)),
				tool("grep", m("pattern", "alpha", "path", "proj/one.txt", "case_insensitive", true)),
				tool("grep", m("pattern", "x", "path", "proj/nope")),
				tool("grep", m()),
				tool("glob", m("pattern", "proj/*.md")),
				tool("glob", m("pattern", "proj/*.txt")),
				tool("glob", m("pattern", "*", "path", "proj/nope")),
				tool("glob", m()),
				tool("ls", m("path", "proj")),
				tool("ls", m("path", "proj/empty")),
				tool("ls", m("path", "proj/nope")),
			),
		},
	}
	for _, sc := range table {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			var workdir string
			sc.actions = []action{
				captureWorkdir{dst: &workdir},
				create{as: "a"},
				submit{as: "a", text: "go"},
				waitIdle{as: "a"},
			}
			obs := runScenario(t, sc)
			compareGolden(t, sc.name, scrubWorkdir(t, obs, workdir))
		})
	}
}
