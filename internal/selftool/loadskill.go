package selftool

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/majorcontext/harness/internal/skill"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// LoadSkillName is the name of the load_skill tool.
const LoadSkillName = "load_skill"

const loadSkillDescription = "Load a file of an embedded skill that the system prompt lists: its SKILL.md by default, " +
	"or another file of the skill at a slash-separated path relative to the skill root."

const loadSkillSchema = `{"type":"object","properties":{"name":{"type":"string","description":"The skill name"},` +
	`"file":{"type":"string","description":"A file of the skill, relative to its root. Default: SKILL.md"}},"required":["name"]}`

// SkillReader returns one file of an embedded skill of a session.
type SkillReader func(ctx context.Context, session, name, file string) ([]byte, error)

type loadSkill struct {
	read    SkillReader
	session string
	names   []string
}

// NewLoadSkill returns the load_skill tool of a session that lists the skills names.
func NewLoadSkill(read SkillReader, session string, names []string) turn.Tool {
	return loadSkill{read, session, slices.Clone(names)}
}

func (loadSkill) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: LoadSkillName, Description: loadSkillDescription, InputSchema: json.RawMessage(loadSkillSchema), ReadOnly: true}
}

func (t loadSkill) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct{ Name, File string }
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("%s: invalid arguments: %w", LoadSkillName, err)
	}
	if !slices.Contains(t.names, in.Name) {
		return protocol.ToolResult{}, fmt.Errorf("%s: unknown skill %s", LoadSkillName, in.Name)
	}
	file := in.File
	if file == "" {
		file = skill.Filename
	}
	if path.IsAbs(file) || path.Clean(file) != file || slices.Contains(strings.Split(file, "/"), "..") {
		return protocol.ToolResult{}, fmt.Errorf("%s: the file %q is not a clean path inside the skill", LoadSkillName, file)
	}
	b, err := t.read(ctx, t.session, in.Name, file)
	if err != nil {
		return protocol.ToolResult{}, fmt.Errorf("%s: %w", LoadSkillName, err)
	}
	if !utf8.Valid(b) {
		return protocol.ToolResult{Text: fmt.Sprintf("[binary file %s, %d bytes]", file, len(b))}, nil
	}
	return protocol.ToolResult{Text: string(b)}, nil
}
