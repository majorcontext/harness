package openai

import (
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/message"
)

func toMessage(m eventlog.Message) message.Message {
	out := message.Message{Role: message.Role(m.Role)}
	for _, p := range m.Parts {
		switch p.Type {
		case eventlog.PartText:
			out.Parts = append(out.Parts, &message.Text{Text: p.Text})
		case eventlog.PartReasoning:
			out.Parts = append(out.Parts, &message.Reasoning{Text: p.Text})
		case eventlog.PartToolCall:
			out.Parts = append(out.Parts, &message.ToolCall{CallID: p.CallID, Name: p.Name, Arguments: p.Arguments})
		case eventlog.PartToolResult:
			out.Parts = append(out.Parts, &message.ToolResult{CallID: p.CallID, IsError: p.IsError,
				Content: message.Parts{&message.Text{Text: p.Text}}})
		}
	}
	return out
}

func fromMessage(m *message.Message) eventlog.Message {
	out := eventlog.Message{Role: eventlog.RoleAssistant}
	if m == nil {
		return out
	}
	for _, p := range m.Parts {
		switch p := p.(type) {
		case *message.Text:
			out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartText, Text: p.Text})
		case *message.Reasoning:
			if p.Text != "" {
				out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartReasoning, Text: p.Text})
			}
		case *message.ToolCall:
			out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartToolCall, CallID: p.CallID, Name: p.Name, Arguments: p.Arguments})
		}
	}
	return out
}
