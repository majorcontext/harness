package acp

import (
	"context"
	"encoding/base64"
	"io"
	"slices"
	"strings"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// replayPage is the number of messages that a replay reads at once.
const replayPage = protocol.MaxMessageLimit

// noToolOutput is the content that a reader shows for a tool result with no text.
const noToolOutput = "(no output)"

// replay sends the conversation of r as updates, oldest first, and leaves in st
// what the pump needs for the calls that have no result. r is a view, so its
// messages do not change while the replay reads them.
func (h *held) replay(ctx context.Context, r harness.Reader, st *pumpState) error {
	sent := map[uint64]bool{}
	next := uint64(1)
	for {
		page, err := r.Messages(ctx, next+replayPage, replayPage)
		if err != nil {
			return err
		}
		skip := 0
		if page.FirstSeq > 0 && next > page.FirstSeq {
			skip = int(next - page.FirstSeq)
		}
		if page.FirstSeq <= 1 {
			if err := h.replayCommands(page.Commands, "", sent); err != nil {
				return err
			}
		}
		for _, m := range page.Messages[min(skip, len(page.Messages)):] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := h.replayMessage(ctx, r, m, st); err != nil {
				return err
			}
			if err := h.replayCommands(page.Commands, m.ID, sent); err != nil {
				return err
			}
		}
		if page.LastSeq >= page.Total {
			return nil
		}
		next = page.LastSeq + 1
	}
}

// replayCommands sends the typed commands that followed the message after.
func (h *held) replayCommands(cmds []protocol.MessageCommand, after string, sent map[uint64]bool) error {
	for _, c := range cmds {
		if c.AfterMessageID != after || sent[c.Seq] {
			continue
		}
		sent[c.Seq] = true
		if c.Status == protocol.CommandAccepted || c.Text == "" {
			continue
		}
		if err := h.chunk("agent_message_chunk", "cmd_"+c.InputID, c.Text); err != nil {
			return err
		}
	}
	return nil
}

func (h *held) replayMessage(ctx context.Context, r harness.Reader, m protocol.Message, st *pumpState) error {
	switch m.Role {
	case protocol.RoleUser:
		return h.replayUser(ctx, r, m)
	case protocol.RoleAssistant:
		lm := logMessage(m)
		if err := h.assistantText(&protocol.ItemCompleted{ItemID: strings.TrimPrefix(m.ID, "msg_"), Message: lm}, st.sent); err != nil {
			return err
		}
		return h.toolCall(lm, st)
	case protocol.RoleTool:
		return h.toolResult(ctx, logMessage(m), st)
	}
	return nil
}

// replayUser sends the text of a user message, then each image of it.
func (h *held) replayUser(ctx context.Context, r harness.Reader, m protocol.Message) error {
	var text strings.Builder
	for _, part := range m.Parts {
		if part.Type == protocol.MessagePartText {
			text.WriteString(part.Text)
		}
	}
	if text.Len() > 0 {
		if err := h.update(chunkUpdateBody{Kind: "user_message_chunk", Content: textContent{Type: "text", Text: text.String()}, MessageID: m.ID}); err != nil {
			return err
		}
	}
	for _, part := range m.Parts {
		if part.Type != protocol.MessagePartBlob || !strings.HasPrefix(part.MediaType, "image/") {
			continue
		}
		data, err := readAll(ctx, r, part.Key)
		if err != nil {
			h.log.Warn("acp: user image unavailable", "session", h.id, "blob", part.Key, "error", err.Error())
			continue
		}
		body := map[string]any{"sessionUpdate": "user_message_chunk", "messageId": m.ID,
			"content": imageContent{Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: part.MediaType}}
		if err := h.update(body); err != nil {
			return err
		}
	}
	return nil
}

func readAll(ctx context.Context, r harness.Reader, key string) ([]byte, error) {
	b, err := r.Blob(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Body.Close() }()
	return io.ReadAll(b.Body)
}

// seedDiffs records in st the diff of each call in the newest page of r that has
// no result, so a tool_call_update after a resume keeps the diff of its call.
func (h *held) seedDiffs(ctx context.Context, r harness.Reader, st *pumpState) error {
	page, err := r.Messages(ctx, 0, protocol.DefaultMessageLimit)
	if err != nil {
		return err
	}
	for _, m := range page.Messages {
		for _, part := range logMessage(m).Parts {
			switch part.Type {
			case protocol.LogPartToolCall:
				if body := describeCall(part, h.cwd); len(body.Content) > 0 {
					st.diffs[part.CallID] = body.Content
				}
			case protocol.LogPartToolResult:
				delete(st.diffs, part.CallID)
			}
		}
	}
	return nil
}

// logMessage maps a message of a reader to the log message that the pump reads.
// A tool result with no text reads "(no output)" in a reader, and is empty in the log.
func logMessage(m protocol.Message) protocol.LogMessage {
	lm := protocol.LogMessage{Role: m.Role, ParentCallID: m.ParentCallID, Parts: make([]protocol.LogPart, 0, len(m.Parts))}
	for _, p := range m.Parts {
		lp := protocol.LogPart{Type: p.Type, Text: p.Text, CallID: p.CallID, Name: p.Name, Arguments: slices.Clone(p.Arguments), IsError: p.IsError}
		if p.Type == protocol.MessagePartToolResult && p.Content != noToolOutput {
			lp.Text = p.Content
		}
		lm.Parts = append(lm.Parts, lp)
	}
	return lm
}
