package acp

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"iter"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/majorcontext/harness/protocol"
)

// toolKinds maps a tool name to its ACP tool kind. A name that is not here is
// "other", which includes every MCP tool and plugin tool.
var toolKinds = map[string]string{
	"read_file":        "read",
	"read_tool_result": "read",
	"write_file":       "edit",
	"edit_file":        "edit",
	"glob":             "search",
	"grep":             "search",
	"ls":               "search",
	"bash":             "execute",
	"process":          "execute",
}

// fileTools are the tools whose path argument is a file location.
var fileTools = map[string]bool{
	"read_file": true, "write_file": true, "edit_file": true,
}

const titleRunes = 100

type location struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

type diffItem struct {
	Type    string  `json:"type"`
	Path    string  `json:"path"`
	OldText *string `json:"oldText"`
	NewText string  `json:"newText"`
}

type contentItem struct {
	Type    string `json:"type"`
	Content any    `json:"content"`
}

type imageContent struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

type toolCallBody struct {
	Kind      string          `json:"sessionUpdate"`
	ID        string          `json:"toolCallId"`
	Name      string          `json:"name"`
	Title     string          `json:"title"`
	ToolKind  string          `json:"kind"`
	Status    string          `json:"status"`
	Content   []any           `json:"content,omitempty"`
	Locations []location      `json:"locations,omitempty"`
	RawInput  json.RawMessage `json:"rawInput,omitempty"`
}

type toolUpdateBody struct {
	Kind      string `json:"sessionUpdate"`
	ID        string `json:"toolCallId"`
	Status    string `json:"status"`
	Content   []any  `json:"content,omitempty"`
	RawOutput string `json:"rawOutput"`
}

// toolArgs holds the arguments of the file and shell tools that the adapter reads.
type toolArgs struct {
	Path      string  `json:"path"`
	FilePath  string  `json:"file_path"`
	Offset    int     `json:"offset"`
	OldString string  `json:"old_string"`
	NewString string  `json:"new_string"`
	Content   *string `json:"content"`
	Command   string  `json:"command"`
	Pattern   string  `json:"pattern"`
}

func (a toolArgs) path() string {
	if a.Path != "" {
		return a.Path
	}
	return a.FilePath
}

// describeCall maps a tool call to the fields of its tool_call update.
func describeCall(part protocol.LogPart, cwd string) toolCallBody {
	var args toolArgs
	_ = json.Unmarshal(part.Arguments, &args)
	kind := toolKinds[part.Name]
	if kind == "" {
		kind = "other"
	}
	body := toolCallBody{Kind: "tool_call", ID: part.CallID, Name: part.Name, Title: callTitle(part.Name, args),
		ToolKind: kind, Status: "in_progress", RawInput: part.Arguments}
	path := args.path()
	if path == "" || !fileTools[part.Name] {
		return body
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	loc := location{Path: path}
	if kind == "read" && args.Offset > 0 {
		loc.Line = args.Offset
	}
	body.Locations = []location{loc}
	switch part.Name {
	case "edit_file", "Edit":
		if args.OldString != "" {
			old := args.OldString
			body.Content = []any{diffItem{Type: "diff", Path: path, OldText: &old, NewText: args.NewString}}
		}
	case "write_file", "Write":
		if args.Content != nil {
			body.Content = []any{diffItem{Type: "diff", Path: path, NewText: *args.Content}}
		}
	}
	return body
}

func callTitle(name string, a toolArgs) string {
	detail := cmp.Or(a.Command, a.Pattern, a.path())
	detail, _, _ = strings.Cut(detail, "\n")
	if utf8.RuneCountInString(detail) > titleRunes {
		detail = string([]rune(detail)[:titleRunes]) + "..."
	}
	if detail == "" {
		return name
	}
	return name + " " + detail
}

func textItem(text string) contentItem {
	return contentItem{Type: "content", Content: textContent{Type: "text", Text: text}}
}

// toolCall sends the tool_call update of every tool_call part of a completed
// assistant message, and records the ones that keep their diff.
func (h *held) toolCall(msg protocol.LogMessage, st *pumpState) error {
	for _, part := range msg.Parts {
		if part.Type != protocol.LogPartToolCall {
			continue
		}
		body := describeCall(part, h.cwd)
		if len(body.Content) > 0 {
			st.diffs[part.CallID] = body.Content
		}
		if err := h.send("session/update", map[string]any{"sessionId": h.id, "update": body}); err != nil {
			return err
		}
	}
	return nil
}

// toolResult sends the tool_call_update of every tool_result part of a
// completed tool message. A diff the call sent stays as the content of a call
// that succeeded; an error keeps only its text.
func (h *held) toolResult(ctx context.Context, msg protocol.LogMessage, st *pumpState) error {
	for _, part := range msg.Parts {
		if part.Type != protocol.LogPartToolResult {
			continue
		}
		body := toolUpdateBody{Kind: "tool_call_update", ID: part.CallID, Status: "completed", RawOutput: part.Text}
		diff := st.diffs[part.CallID]
		delete(st.diffs, part.CallID)
		switch {
		case part.IsError:
			body.Status = "failed"
			if part.Text != "" {
				body.Content = []any{textItem(part.Text)}
			}
		case diff != nil:
			body.Content = diff
		case part.Text != "":
			body.Content = []any{textItem(part.Text)}
		}
		body.Content = append(body.Content, h.images(ctx, part)...)
		if err := h.send("session/update", map[string]any{"sessionId": h.id, "update": body}); err != nil {
			return err
		}
	}
	return nil
}

// images returns the image content items of the blobs of a tool result. A blob
// that is not an image, or that the store cannot read, is left out.
func (h *held) images(ctx context.Context, part protocol.LogPart) []any {
	var out []any
	for _, b := range part.Blobs {
		if !strings.HasPrefix(b.MediaType, "image/") {
			continue
		}
		data, err := h.readBlob(ctx, b.BlobKey)
		if err != nil {
			h.log.Warn("acp: tool result image unavailable", "session", h.id, "blob", b.BlobKey, "error", err.Error())
			continue
		}
		out = append(out, contentItem{Type: "content", Content: imageContent{Type: "image",
			Data: base64.StdEncoding.EncodeToString(data), MimeType: b.MediaType}})
	}
	return out
}

func (h *held) readBlob(ctx context.Context, key string) ([]byte, error) {
	r, err := h.toolImage(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// FindToolImage reports whether events, the events of one session, hold an
// item.completed record whose tool result names key as an image blob. A host
// implements Runtimes.ToolImage with it, so a key that the log of the session
// does not name as a tool-result image is never read.
func FindToolImage(events iter.Seq2[protocol.Event, error], key string) (bool, error) {
	for e, err := range events {
		if err != nil {
			return false, err
		}
		if e.Kind != protocol.KindItemCompleted {
			continue
		}
		p, err := e.Payload()
		if err != nil {
			continue
		}
		done, ok := p.(*protocol.ItemCompleted)
		if !ok || done.Message.Role != protocol.RoleTool {
			continue
		}
		for _, part := range done.Message.Parts {
			for _, b := range part.Blobs {
				if b.BlobKey == key && strings.HasPrefix(b.MediaType, "image/") {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
