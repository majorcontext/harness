package modelapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/turn"
)

// toMessage maps m to the provider message. A blob part reads its bytes through req.Blob.
// A tool result whose image the store lacks keeps its text, so one lost blob does not fail every later turn.
func (be *Backend) toMessage(ctx context.Context, req turn.Request, m protocol.LogMessage) (message.Message, error) {
	out := message.Message{Role: message.Role(m.Role)}
	for _, p := range m.Parts {
		switch p.Type {
		case protocol.LogPartText:
			out.Parts = append(out.Parts, &message.Text{Text: p.Text, Citations: citations(p.Citations), ProviderData: p.ProviderData})
		case protocol.LogPartWebSearch:
			w := &message.WebSearch{ID: p.CallID, Query: p.Query, ProviderData: p.ProviderData}
			if p.IsError {
				w.Error = p.Text
			}
			for _, r := range p.Results {
				w.Results = append(w.Results, message.SearchResult(r))
			}
			out.Parts = append(out.Parts, w)
		case protocol.LogPartEngineContext:
			out.Parts = append(out.Parts, &message.EngineContext{Text: p.Text})
		case protocol.LogPartReasoning:
			out.Parts = append(out.Parts, &message.Reasoning{Text: p.Text, ProviderData: p.ProviderData})
		case protocol.LogPartToolCall:
			args := p.Arguments
			if p.ArgumentsText != "" {
				args = json.RawMessage(p.ArgumentsText)
			}
			out.Parts = append(out.Parts, &message.ToolCall{CallID: p.CallID, Name: p.Name, Arguments: args, ProviderData: p.ProviderData})
		case protocol.LogPartToolResult:
			content := message.Parts{&message.Text{Text: p.Text}}
			for _, b := range p.Blobs {
				data, err := readBlob(ctx, req, b.BlobKey)
				if errors.Is(err, fs.ErrNotExist) {
					be.log.Warn("harness: a tool result image is missing from the store, so the model gets the text only", "session", req.SessionID, "call", p.CallID, "blob", b.BlobKey)
					continue
				}
				if err != nil {
					return message.Message{}, fmt.Errorf("modelapi: tool result image %s: %w", b.BlobKey, err)
				}
				content = append(content, &message.Blob{MediaType: b.MediaType, Data: data})
			}
			out.Parts = append(out.Parts, &message.ToolResult{CallID: p.CallID, IsError: p.IsError, Content: content})
		case protocol.LogPartBlob:
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

func fromMessage(m *message.Message) protocol.LogMessage {
	out := protocol.LogMessage{Role: protocol.RoleAssistant}
	if m == nil {
		return out
	}
	for _, p := range m.Parts {
		switch p := p.(type) {
		case *message.Text:
			part := protocol.LogPart{Type: protocol.LogPartText, Text: p.Text, ProviderData: p.ProviderData}
			for _, c := range p.Citations {
				part.Citations = append(part.Citations, protocol.Citation(c))
			}
			out.Parts = append(out.Parts, part)
		case *message.WebSearch:
			part := protocol.LogPart{Type: protocol.LogPartWebSearch, CallID: p.ID, Query: p.Query, IsError: p.Error != "", Text: p.Error, ProviderData: p.ProviderData}
			for _, r := range p.Results {
				part.Results = append(part.Results, protocol.SearchResult(r))
			}
			out.Parts = append(out.Parts, part)
		case *message.Reasoning:
			if p.Text != "" || len(p.ProviderData) > 0 {
				out.Parts = append(out.Parts, protocol.LogPart{Type: protocol.LogPartReasoning, Text: p.Text, ProviderData: p.ProviderData})
			}
		case *message.ToolCall:
			args := p.Arguments
			// A model can send arguments that are not valid JSON. The wire sends nil as {}.
			if !json.Valid(args) {
				args = nil
			}
			out.Parts = append(out.Parts, protocol.LogPart{Type: protocol.LogPartToolCall, CallID: p.CallID, Name: p.Name, Arguments: args, ArgumentsText: argumentsText(args), ProviderData: p.ProviderData})
		}
	}
	return out
}

// argumentsText returns args when the log encoding of args would change its bytes, else "". A Responses or
// chat-completions request sends the arguments as a string, and the next request must send the bytes of the model.
func argumentsText(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	if encoded, err := json.Marshal(args); err == nil && bytes.Equal(encoded, args) {
		return ""
	}
	return string(args)
}

func citations(cs []protocol.Citation) []message.Citation {
	var out []message.Citation
	for _, c := range cs {
		out = append(out, message.Citation(c))
	}
	return out
}
