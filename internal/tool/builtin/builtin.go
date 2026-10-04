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
	d := dir{root: workDir, read: &guard{hashes: map[string][32]byte{}}}
	return []turn.Tool{d.readFile(), d.writeFile(), d.editFile(), d.glob(), d.grep(), d.ls(), d.bash()}
}

// dir resolves the paths of the tools of one session.
type dir struct {
	root string
	read *guard
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
	run  func(ctx context.Context, args json.RawMessage) (string, error)
}

func newTool(name, description, schema string, run func(context.Context, json.RawMessage) (string, error)) tool {
	return tool{protocol.ToolSpec{Name: name, Description: description, InputSchema: json.RawMessage(schema)}, run}
}

func (t tool) Spec() protocol.ToolSpec { return t.spec }

func (t tool) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	text, err := t.run(ctx, call.Arguments)
	return protocol.ToolResult{Text: text}, err
}
