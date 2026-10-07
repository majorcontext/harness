package modelapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/turn"
)

// toMessage maps m to the provider message. A blob part reads its bytes through req.Blob.
func toMessage(ctx context.Context, req turn.Request, m eventlog.Message) (message.Message, error) {
	out := message.Message{Role: message.Role(m.Role)}
	for _, p := range m.Parts {
		switch p.Type {
		case eventlog.PartText:
			out.Parts = append(out.Parts, &message.Text{Text: p.Text})
		case eventlog.PartEngineContext:
			out.Parts = append(out.Parts, &message.EngineContext{Text: p.Text})
		case eventlog.PartReasoning:
			out.Parts = append(out.Parts, &message.Reasoning{Text: p.Text, ProviderData: p.ProviderData})
		case eventlog.PartToolCall:
			out.Parts = append(out.Parts, &message.ToolCall{CallID: p.CallID, Name: p.Name, Arguments: p.Arguments})
		case eventlog.PartToolResult:
			out.Parts = append(out.Parts, &message.ToolResult{CallID: p.CallID, IsError: p.IsError,
				Content: message.Parts{&message.Text{Text: p.Text}}})
		case eventlog.PartBlob:
			data, err := readBlob(ctx, req, p.BlobKey)
			if err != nil {
				return message.Message{}, fmt.Errorf("modelapi: attachment %s: %w", p.BlobKey, err)
			}
			out.Parts = append(out.Parts, &message.Blob{MediaType: p.MediaType, Data: data})
		}
	}
	return out, nil
}

func readBlob(ctx context.Context, req turn.Request, key string) ([]byte, error) {
	if req.Blob == nil {
		return nil, errors.New("the request has no blob reader")
	}
	return req.Blob(ctx, key)
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
			if p.Text != "" || len(p.ProviderData) > 0 {
				out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartReasoning, Text: p.Text, ProviderData: p.ProviderData})
			}
		case *message.ToolCall:
			args := p.Arguments
			// The output cap can cut the arguments mid-value. The wire sends nil as {}.
			if !json.Valid(args) {
				args = nil
			}
			out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartToolCall, CallID: p.CallID, Name: p.Name, Arguments: args})
		}
	}
	return out
}

func hasOutput(m eventlog.Message) bool {
	return slices.ContainsFunc(m.Parts, func(p eventlog.Part) bool {
		return p.Type == eventlog.PartToolCall || p.Type == eventlog.PartText && p.Text != ""
	})
}
