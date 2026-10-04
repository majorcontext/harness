package toolresult

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/majorcontext/harness/protocol"
)

// ToolName names the read_tool_result tool.
const ToolName = "read_tool_result"

const (
	defaultLimit    = 200
	maxLimit        = 2000
	defaultMaxBytes = 16384
	maxMaxBytes     = 65536
	knownList       = 20
	// minMaxBytes is the floor of max_bytes for a short handle and tool name.
	minMaxBytes = 256
	// noticeReserve keeps room for the notice that follows the lines.
	noticeReserve = 128
	// minBodyRoom is the room for lines that a max_bytes must leave after
	// the preamble and the notice.
	minBodyRoom = 64
	// matchContext is the bytes before a match that a cut line keeps.
	matchContext = 64
)

const description = "Read back a large tool result that was retained out of context. " +
	"When a tool produces more output than fits inline, its result is replaced by a " +
	"short preview carrying a handle (trh_N); the full output is kept on disk and read " +
	"back through this tool. " +
	"Range mode: offset (1-based first line, default 1) and limit (lines, default 200, " +
	"max 2000). Search mode: search is a LITERAL substring (not a regex) — when set, " +
	"offset/limit are ignored and matching lines are returned with their line numbers. " +
	"Output is always bounded by max_bytes (default 16384, max 65536) and says so when " +
	"truncated, so read in windows rather than trying to pull the whole result back."

const schema = `{
	"type": "object",
	"properties": {
		"handle": {"type": "string", "description": "The trh_N handle from a retained tool result's preview header"},
		"offset": {"type": "integer", "description": "1-based first line to return (range mode; default 1)"},
		"limit": {"type": "integer", "description": "Maximum lines to return (range mode; default 200, max 2000)"},
		"search": {"type": "string", "description": "Literal substring to find; returns matching lines with line numbers. Not a regex."},
		"max_bytes": {"type": "integer", "description": "Maximum output bytes (default 16384, max 65536)"}
	},
	"required": ["handle"]
}`

// Store holds the retained results of one session.
type Store interface {
	// Retained returns the retained results in handle order.
	Retained(ctx context.Context) ([]Meta, error)
	// Open opens a blob. A missing blob fails with an error matching fs.ErrNotExist.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// Tool is the read_tool_result tool of one session.
type Tool struct{ s Store }

// NewTool returns the read_tool_result tool over s.
func NewTool(s Store) Tool { return Tool{s} }

// Spec describes the tool.
func (Tool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: ToolName, Description: description, InputSchema: json.RawMessage(schema)}
}

type args struct {
	Handle   string `json:"handle"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
	Search   string `json:"search"`
	MaxBytes int    `json:"max_bytes"`
}

// Run reads one bounded window of a retained result.
func (t Tool) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	var in args
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("%s: invalid arguments: %w", ToolName, err)
	}
	if in.Handle == "" {
		return protocol.ToolResult{}, fmt.Errorf("%s: a %q argument is required", ToolName, "handle")
	}
	if _, ok := Number(in.Handle); !ok {
		return protocol.ToolResult{}, fmt.Errorf("%s: malformed handle %q (want %sN, e.g. %s1)", ToolName, in.Handle, Prefix, Prefix)
	}
	m, err := t.lookup(ctx, in.Handle)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	maxBytes := clamp(in.MaxBytes, defaultMaxBytes, maxMaxBytes)
	if floor := floor(m, in); maxBytes < floor {
		return protocol.ToolResult{}, fmt.Errorf("%s: max_bytes %d is below the minimum %d for this result (handle=%s tool=%q)",
			ToolName, maxBytes, floor, m.Handle, m.Tool)
	}
	data, err := t.read(ctx, m)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	if in.Search != "" {
		return protocol.ToolResult{Text: search(sc, m, in.Search, maxBytes)}, nil
	}
	return protocol.ToolResult{Text: window(sc, m, in.Offset, in.Limit, maxBytes)}, nil
}

func (t Tool) lookup(ctx context.Context, handle string) (Meta, error) {
	metas, err := t.s.Retained(ctx)
	if err != nil {
		return Meta{}, err
	}
	known := make([]string, 0, len(metas))
	for _, m := range metas {
		if m.Handle == handle {
			return m, nil
		}
		known = append(known, m.Handle)
	}
	if len(known) == 0 {
		return Meta{}, fmt.Errorf("%s: unknown handle %q (this session has retained no tool results)", ToolName, handle)
	}
	known = known[max(len(known)-knownList, 0):]
	return Meta{}, fmt.Errorf("%s: unknown handle %q (this session's handles: %s)", ToolName, handle, strings.Join(known, ", "))
}

// read never names the blob path: the error text reaches the model.
func (t Tool) read(ctx context.Context, m Meta) ([]byte, error) {
	rc, err := t.s.Open(ctx, m.Key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: handle %q is known but its retained data is no longer on disk (%d bytes from tool %q); it cannot be read back",
			ToolName, m.Handle, m.Bytes, m.Tool)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read handle %q", ToolName, m.Handle)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read handle %q", ToolName, m.Handle)
	}
	return data, nil
}

// floor is the smallest max_bytes that leaves room for lines after the
// preamble of this request, which grows with the tool name and the counts.
func floor(m Meta, in args) int {
	var preamble string
	if in.Search != "" {
		preamble = searchPreamble(m, in.Search)
	} else {
		offset := max(in.Offset, 1)
		preamble = rangePreamble(m, offset, clamp(in.Limit, defaultLimit, maxLimit))
	}
	return max(len(preamble)+noticeReserve+minBodyRoom, minMaxBytes)
}

func rangePreamble(m Meta, offset, limit int) string {
	return fmt.Sprintf("%s (tool=%s, %d bytes, %d lines) lines %d-%d:\n", m.Handle, m.Tool, m.Bytes, m.Lines, offset, offset+limit-1)
}

func searchPreamble(m Meta, needle string) string {
	return fmt.Sprintf("%s (tool=%s, %d bytes, %d lines) lines matching %q:\n", m.Handle, m.Tool, m.Bytes, m.Lines, needle)
}

// clamp returns def for v <= 0 and caps v at most.
func clamp(v, def, most int) int {
	if v <= 0 {
		return def
	}
	return min(v, most)
}
