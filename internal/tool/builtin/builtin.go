// Package builtin holds the file, search, and shell tools of a coding agent.
package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// Names are the names of the tools that Tools returns.
var Names = []string{"read_file", "write_file", "edit_file", "glob", "grep", "ls", "bash"}

// Tools returns the file, search, and shell tools of workDir. Each call
// returns a new set: the read guard of write_file belongs to one session.
func Tools(workDir string) []turn.Tool {
	d := dir{root: workDir, read: &guard{hashes: map[string][32]byte{}}, mem: &budget{limit: readBudgetBytes}}
	return []turn.Tool{d.keyed(d.readFile()), d.keyed(d.writeFile()), d.keyed(d.editFile()), d.glob(), d.grep(), d.ls(), d.bash()}
}

// dir resolves the paths of the tools of one session.
type dir struct {
	root string
	read *guard
	mem  *budget
}

// resolve joins a relative path to the work directory.
func (d dir) resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(d.root, path)
}

// base returns the resolved path, or the work directory when path is empty.
func (d dir) base(path string) string {
	if path == "" {
		return d.root
	}
	return d.resolve(path)
}

type tool struct {
	spec protocol.ToolSpec
	run  func(ctx context.Context, args json.RawMessage) (protocol.ToolResult, error)
	key  func(args json.RawMessage) string
}

func newTool(name, description, schema string, run func(context.Context, json.RawMessage) (string, error)) tool {
	return newResultTool(name, description, schema, func(ctx context.Context, args json.RawMessage) (protocol.ToolResult, error) {
		text, err := run(ctx, args)
		return protocol.ToolResult{Text: text}, err
	})
}

func newResultTool(name, description, schema string, run func(context.Context, json.RawMessage) (protocol.ToolResult, error)) tool {
	return tool{spec: protocol.ToolSpec{Name: name, Description: description, InputSchema: json.RawMessage(schema)}, run: run}
}

// keyed makes the calls of t on one file run in call order.
func (d dir) keyed(t tool) tool {
	t.key = d.pathKey
	return t
}

// Key names the resource of a call, or "" when the tool has none.
func (t tool) Key(call protocol.ToolCall) string {
	if t.key == nil {
		return ""
	}
	return t.key(call.Arguments)
}

// pathKey names the file that a call of read_file, write_file, or edit_file
// touches, so that two spellings of one path, also through a symlink, share a
// key. Calls whose path cannot be read share one key. A hard link is a
// second name that this key does not join.
func (d dir) pathKey(args json.RawMessage) string {
	var in struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(args, &in) != nil || in.Path == "" {
		return "path:<unparsed>"
	}
	path := d.resolve(in.Path)
	abs, err := filepath.Abs(path)
	if err != nil {
		return "path:" + filepath.Clean(path)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return "path:" + real
	}
	rest := ""
	for p := abs; ; {
		parent := filepath.Dir(p)
		rest = filepath.Join(filepath.Base(p), rest)
		if parent == p {
			return "path:" + abs
		}
		if real, err := filepath.EvalSymlinks(parent); err == nil {
			return "path:" + filepath.Join(real, rest)
		}
		p = parent
	}
}

func (t tool) Spec() protocol.ToolSpec { return t.spec }

func (t tool) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	return t.run(ctx, call.Arguments)
}
