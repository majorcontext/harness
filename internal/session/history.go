package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const (
	historyDefaultLimit = 500
	historyMaxLimit     = 2000
	historyTruncate     = 2000
)

var historySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"offset": {
			"type": "integer",
			"minimum": 0,
			"description": "Number of messages (oldest first) to skip before the returned page. Defaults to 0. Use the next_offset value from a prior call's response to continue reading a long history."
		},
		"limit": {
			"type": "integer",
			"minimum": 1,
			"description": "Maximum number of messages to return in this call. Defaults to 500."
		}
	}
}`)

const historyDescription = "Read the PRIOR conversation history for this session: messages that already happened before this turn, either on a different model or in a part of this conversation you have not seen. Call this once, before responding, whenever you are continuing a conversation you have not already read. Supports offset/limit pagination for long histories."

// historyTool reads the live history of its session.
type historyTool struct{ a *Actor }

func (historyTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: turn.HistoryTool, Description: historyDescription, InputSchema: historySchema}
}

func (h historyTool) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct{ Offset, Limit int }
	if len(c.Arguments) > 0 {
		if err := json.Unmarshal(c.Arguments, &in); err != nil {
			return protocol.ToolResult{}, fmt.Errorf("%s: invalid arguments: %w", turn.HistoryTool, err)
		}
	}
	msgs, err := call(ctx, h.a, func(reply func([]eventlog.Message, error)) { reply(h.a.state.History(), nil) })
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{Text: historyText(msgs, in.Offset, in.Limit)}, nil
}

// historyText renders a page of msgs, oldest first.
func historyText(msgs []eventlog.Message, offset, limit int) string {
	total := len(msgs)
	if total == 0 {
		return "No prior conversation history for this session."
	}
	offset = min(max(offset, 0), total)
	if limit <= 0 || limit > historyMaxLimit {
		limit = historyDefaultLimit
	}
	end := min(offset+limit, total)
	names := map[string]string{}
	for _, m := range msgs[:offset] {
		callNames(m, names)
	}
	var b strings.Builder
	b.WriteString("The following is PRIOR conversation history for this session that already happened. It is context for you to read, not a new message to respond to.\n\n")
	if end == offset {
		fmt.Fprintf(&b, "(no messages in the requested range; %d total)\n", total)
	} else {
		fmt.Fprintf(&b, "Showing messages %d-%d of %d total (oldest first).\n\n", offset+1, end, total)
		for _, m := range msgs[offset:end] {
			callNames(m, names)
			flatten(&b, m, names)
		}
	}
	if end < total {
		fmt.Fprintf(&b, "\nMore history is available. Call %s again with offset=%d to continue.\n", turn.HistoryTool, end)
	}
	return b.String()
}

func callNames(m eventlog.Message, names map[string]string) {
	for _, p := range m.Parts {
		if p.Type == eventlog.PartToolCall {
			names[p.CallID] = p.Name
		}
	}
}

func flatten(b *strings.Builder, m eventlog.Message, names map[string]string) {
	switch m.Role {
	case eventlog.RoleUser:
		text := textOf(m)
		if text == "" {
			text = "(no text)"
		}
		fmt.Fprintf(b, "User: %s\n", text)
	case eventlog.RoleAssistant:
		for _, p := range m.Parts {
			switch {
			case p.Type == eventlog.PartText && p.Text != "":
				fmt.Fprintf(b, "Assistant: %s\n", p.Text)
			case p.Type == eventlog.PartReasoning:
				b.WriteString("Assistant: [thinking]\n")
			case p.Type == eventlog.PartToolCall:
				fmt.Fprintf(b, "Assistant called tool %s(%s)\n", p.Name, clip(string(p.Arguments)))
			}
		}
	case eventlog.RoleTool:
		for _, p := range m.Parts {
			if p.Type != eventlog.PartToolResult {
				continue
			}
			name, status := names[p.CallID], "ok"
			if name == "" {
				name = p.CallID
			}
			if p.IsError {
				status = "error"
			}
			fmt.Fprintf(b, "Tool result (%s, %s): %s\n", name, status, clip(p.Text))
		}
	}
}

// textOf joins the text parts of m with newlines.
func textOf(m eventlog.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == eventlog.PartText || p.Type == eventlog.PartEngineContext {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func clip(s string) string {
	if len(s) <= historyTruncate {
		return s
	}
	return fmt.Sprintf("%s... (truncated, %d bytes total)", s[:historyTruncate], len(s))
}
