package e2e

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

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

func ftTool(name string, in map[string]any) harnesstest.ToolCall {
	return harnesstest.ToolCall{Name: name, Input: in}
}

func ftWrite(path, content string) harnesstest.ToolCall {
	return ftTool("write_file", map[string]any{"path": path, "content": content})
}

func ftRead(in map[string]any) harnesstest.ToolCall { return ftTool("read_file", in) }

func ftEdit(path, old, repl string) harnesstest.ToolCall {
	return ftTool("edit_file", map[string]any{"path": path, "old_string": old, "new_string": repl})
}

func ftBash(cmd string) harnesstest.ToolCall { return ftTool("bash", map[string]any{"command": cmd}) }

func ftArgs(kv ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

var oneTurn = []action{
	create{as: "a"},
	submit{as: "a", text: "go"},
	waitIdle{as: "a"},
}

// Files are written newest-first in alphabetical order, so glob's
// newest-first order and its alphabetical tie-break agree.
func TestContractFileToolsWrites(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "file_tools_roundtrip",
			model: toolChain(
				ftWrite("proj/sub/c.txt", "gamma\n"),
				ftWrite("proj/b.txt", "beta\nneedle two\n"),
				ftWrite("proj/a.txt", "alpha\nNeedle one\nneedle three\n"),
				ftRead(ftArgs("path", "proj/a.txt")),
				ftRead(ftArgs("path", "proj/a.txt", "offset", 2, "limit", 1)),
				ftEdit("proj/a.txt", "alpha", "ALPHA"),
				ftEdit("proj/a.txt", "absent", "x"),
				ftRead(ftArgs("path", "proj/a.txt")),
				ftTool("grep", ftArgs("pattern", "needle", "path", "proj")),
				ftTool("grep", ftArgs("pattern", "needle", "path", "proj", "case_insensitive", true, "glob", "a.*")),
				ftTool("glob", ftArgs("pattern", "**/*.txt", "path", "proj")),
				ftTool("ls", ftArgs("path", "proj")),
				ftRead(ftArgs("path", "proj/missing.txt")),
			),
			actions: oneTurn,
		},
		{
			name: "file_tools_write_edit_guards",
			model: toolChain(
				ftWrite("proj/g.txt", "one\ntwo\ntwo\n"),
				ftBash("printf 'seed\\n' > proj/seed.txt"),
				ftWrite("proj/seed.txt", "x"),
				ftRead(ftArgs("path", "proj/seed.txt")),
				ftWrite("proj/seed.txt", "replaced\n"),
				ftBash("printf 'outside\\n' > proj/seed.txt"),
				ftWrite("proj/seed.txt", "again"),
				ftEdit("proj/g.txt", "two", "2"),
				ftTool("edit_file", ftArgs("path", "proj/g.txt", "old_string", "two", "new_string", "2", "replace_all", true)),
				ftRead(ftArgs("path", "proj/g.txt")),
				ftEdit("proj/g.txt", "one", "one"),
				ftEdit("proj/nope.txt", "a", "b"),
				ftTool("write_file", ftArgs("path", "proj/x.txt")),
				ftEdit("proj/g.txt", "", "x"),
			),
			actions: oneTurn,
		},
	})
}

func TestContractFileToolsEdges(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "file_tools_read_edges",
			model: toolChain(
				ftWrite("proj/empty.txt", ""),
				ftRead(ftArgs("path", "proj/empty.txt")),
				ftWrite("proj/three.txt", "a\nb\nc\n"),
				ftRead(ftArgs("path", "proj/three.txt", "offset", 9)),
				ftRead(ftArgs("path", "proj")),
				ftRead(ftArgs()),
				ftRead(ftArgs("path", "proj/three.txt", "offset", 2)),
				ftRead(ftArgs("path", "proj/three.txt", "limit", 1)),
			),
			actions: oneTurn,
		},
		{
			name: "file_tools_search_edges",
			model: toolChain(
				ftWrite("proj/one.txt", "Alpha\nbeta\n"),
				ftBash("printf 'alpha\\0' > proj/bin.dat && mkdir proj/empty"),
				ftTool("grep", ftArgs("pattern", "(", "path", "proj")),
				ftTool("grep", ftArgs("pattern", "zzz", "path", "proj")),
				ftTool("grep", ftArgs("pattern", "alpha", "path", "proj", "case_insensitive", true)),
				ftTool("grep", ftArgs("pattern", "alpha", "path", "proj/one.txt", "case_insensitive", true)),
				ftTool("grep", ftArgs("pattern", "x", "path", "proj/nope")),
				ftTool("grep", ftArgs()),
				ftTool("glob", ftArgs("pattern", "proj/*.md")),
				ftTool("glob", ftArgs("pattern", "proj/*.txt")),
				ftTool("glob", ftArgs("pattern", "*", "path", "proj/nope")),
				ftTool("glob", ftArgs()),
				ftTool("ls", ftArgs("path", "proj")),
				ftTool("ls", ftArgs("path", "proj/empty")),
				ftTool("ls", ftArgs("path", "proj/nope")),
			),
			actions: oneTurn,
		},
	})
}

func TestContractFileToolsLimits(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(mcpPNG)
	if err != nil {
		t.Fatal(err)
	}
	runScenarios(t, []scenario{
		{
			name: "file_tools_size_cap",
			model: toolChain(
				ftWrite("s.txt", "s\n"),
				ftBash("dd if=/dev/zero of=big.txt bs=1 count=0 seek=20971521 2>/dev/null && dd if=/dev/zero of=s.txt bs=1 count=0 seek=20971521 2>/dev/null"),
				ftRead(ftArgs("path", "big.txt")),
				ftEdit("big.txt", "a", "b"),
				ftWrite("s.txt", "x"),
			),
			actions: oneTurn,
		},
		{
			name: "bash_output_and_exit_status",
			model: toolChain(
				ftBash("echo hi"),
				ftBash("echo out; exit 3"),
				ftBash("exit 4"),
				ftTool("bash", ftArgs()),
				ftBash("echo x; sleep 3 &"),
			),
			actions: oneTurn,
		},
		{
			name:    "read_file_returns_an_image",
			model:   toolChain(ftRead(ftArgs("path", "a.txt"))),
			actions: append([]action{writeFile{path: "a.txt", body: string(png)}}, oneTurn...),
		},
		{
			name: "a_tool_result_image_reaches_the_model_after_a_restart",
			model: append(toolChain(ftRead(ftArgs("path", "a.png"))),
				harnesstest.Step{Name: "again", Match: assistantTurns(2), Reply: harnesstest.Reply{Text: "seen again"}}),
			actions: append(append([]action{writeFile{path: "a.png", body: string(png)}}, oneTurn...),
				restart{}, submit{as: "a", text: "again"}, waitIdle{as: "a"}),
		},
		{
			name: "a_missing_tool_result_image_leaves_the_text_for_the_model",
			model: append(toolChain(ftRead(ftArgs("path", "a.png"))),
				harnesstest.Step{Name: "again", Match: assistantTurns(2), Reply: harnesstest.Reply{Text: "seen again"}}),
			actions: append(append([]action{writeFile{path: "a.png", body: string(png)}}, oneTurn...),
				dropToolBlobs{}, submit{as: "a", text: "again"}, waitIdle{as: "a"}),
		},
		{
			name: "write_guard_belongs_to_one_session",
			model: append(promptChain("read", ftRead(ftArgs("path", "f.txt"))),
				promptChain("write", ftWrite("f.txt", "y"))...),
			actions: []action{
				writeFile{path: "f.txt", body: "x\n"},
				create{as: "a"},
				submit{as: "a", text: "read"},
				waitIdle{as: "a"},
				create{as: "b"},
				submit{as: "b", text: "write"},
				waitIdle{as: "b"},
			},
		},
	})
}

// promptChain is toolChain for the session whose first message is prompt.
func promptChain(prompt string, calls ...harnesstest.ToolCall) []harnesstest.Step {
	steps := toolChain(calls...)
	first := func(r harnesstest.Request) bool {
		return len(r.Messages) > 0 && len(r.Messages[0].Parts) > 0 && r.Messages[0].Parts[0].Text == prompt
	}
	for i := range steps {
		match := steps[i].Match
		steps[i].Name = prompt + " " + steps[i].Name
		steps[i].Match = func(r harnesstest.Request) bool { return first(r) && match(r) }
	}
	return steps
}

func TestContractSessionInfo(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "session_info_reports_the_session",
			model:   toolChain(ftTool("session_info", map[string]any{})),
			actions: oneTurn,
		},
		{
			name: "session_info_reports_what_the_session_loaded",
			model: append(promptChain("first",
				ftWrite("AGENTS.md", "project rule\n"),
				ftWrite(".agents/skills/alpha/SKILL.md", skillFile("alpha", "the alpha skill", "alpha body")),
				ftTool("session_info", map[string]any{})),
				promptChain("second", ftTool("session_info", map[string]any{}))...),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				waitIdle{as: "a"},
				create{as: "b"},
				submit{as: "b", text: "second"},
				waitIdle{as: "b"},
			},
		},
	})
}
