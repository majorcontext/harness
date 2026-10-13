package pluginsrc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/plugin"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
	pub "github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
)

// MCPCall runs tool of an MCP server of the config with args.
type MCPCall func(ctx context.Context, server, tool string, args json.RawMessage) (protocol.ToolResult, error)

// Generate resolves model, makes one model call of req with no tools, and
// returns the resolved model, the reply text, and the usage.
type Generate func(ctx context.Context, model string, req turn.Request) (modelref.ModelRef, string, protocol.Usage, error)

// Calls holds what the runtime lends to the client API of the plugins.
type Calls struct {
	History  History
	Blob     Blob
	MCP      MCPCall
	Generate Generate
}

func (c client) MCPCall(ctx context.Context, req *pub.MCPCallRequest) (*pub.MCPCallResult, error) {
	res, err := c.calls.MCP(ctx, req.Server, req.Tool, req.Args)
	if err != nil {
		return nil, err
	}
	content := []pub.Part{}
	if res.Text != "" {
		content = append(content, pub.TextPart(res.Text))
	}
	for _, b := range res.Blobs {
		content = append(content, pub.Part{Type: pub.PartBlob, MediaType: b.MediaType, Data: b.Data})
	}
	return &pub.MCPCallResult{Content: content, IsError: res.IsError}, nil
}

// convert copies in to out through their shared JSON, which the canonical
// message type validates.
func convert(in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (c client) Generate(ctx context.Context, req *pub.GenerateRequest) (*pub.GenerateResponse, error) {
	if req.MaxTokens < 0 {
		return nil, errors.New("client/generate: max_tokens must not be negative")
	}
	var msgs []message.Message
	if err := convert(req.Messages, &msgs); err != nil {
		return nil, err
	}
	blobs := map[string][]byte{}
	history, err := logMessages(msgs, blobs)
	if err != nil {
		return nil, err
	}
	if len(history) == 0 {
		return nil, errors.New("client/generate: messages are empty")
	}
	treq := turn.Request{Instructions: req.System, History: history, MaxTokens: req.MaxTokens,
		Blob: func(_ context.Context, key string) ([]byte, error) {
			if b, ok := blobs[key]; ok {
				return b, nil
			}
			return nil, fmt.Errorf("client/generate: blob %q is missing", key)
		}}
	ref, text, usage, err := c.calls.Generate(ctx, req.Model, treq)
	if err != nil {
		return nil, errors.New("client/generate: " + plugin.SanitizeSessionError(toolresult.Mask(err.Error())))
	}
	reply := message.Message{Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}, Model: ref}
	var out pub.Message
	if err := convert(reply, &out); err != nil {
		return nil, err
	}
	return &pub.GenerateResponse{Message: out, Usage: &usage}, nil
}

func logMessages(msgs []message.Message, blobs map[string][]byte) ([]protocol.LogMessage, error) {
	out := make([]protocol.LogMessage, 0, len(msgs))
	for i, m := range msgs {
		switch m.Role {
		case message.RoleUser, message.RoleAssistant, message.RoleTool:
		default:
			return nil, fmt.Errorf("client/generate: message %d has role %q", i, m.Role)
		}
		lm := protocol.LogMessage{Role: string(m.Role)}
		for _, p := range m.Parts {
			lp, err := logPart(p, blobs)
			if err != nil {
				return nil, fmt.Errorf("client/generate: message %d: %w", i, err)
			}
			lm.Parts = append(lm.Parts, lp)
		}
		out = append(out, lm)
	}
	return out, nil
}

func logPart(p message.Part, blobs map[string][]byte) (protocol.LogPart, error) {
	switch p := p.(type) {
	case *message.Text:
		return protocol.LogPart{Type: protocol.LogPartText, Text: p.Text}, nil
	case *message.Reasoning:
		return protocol.LogPart{Type: protocol.LogPartReasoning, Text: p.Text}, nil
	case *message.ToolCall:
		return protocol.LogPart{Type: protocol.LogPartToolCall, CallID: p.CallID, Name: p.Name, Arguments: p.Arguments}, nil
	case *message.Blob:
		return logBlob(p, blobs)
	case *message.ToolResult:
		out := protocol.LogPart{Type: protocol.LogPartToolResult, CallID: p.CallID, IsError: p.IsError}
		for _, c := range p.SafeContent() {
			switch c := c.(type) {
			case *message.Text:
				out.Text += c.Text
			case *message.Blob:
				b, err := logBlob(c, blobs)
				if err != nil {
					return out, err
				}
				out.Blobs = append(out.Blobs, b)
			default:
				return out, errors.New("a tool result has a part that is neither text nor a blob")
			}
		}
		return out, nil
	}
	return protocol.LogPart{}, errors.New("a part has a type that generate does not accept")
}

func logBlob(b *message.Blob, blobs map[string][]byte) (protocol.LogPart, error) {
	if b.URL != "" || len(b.Data) == 0 {
		return protocol.LogPart{}, errors.New("a blob part needs inline data")
	}
	key := "plugin-blob-" + strconv.Itoa(len(blobs))
	blobs[key] = b.Data
	return protocol.LogPart{Type: protocol.LogPartBlob, MediaType: b.MediaType, BlobKey: key, Bytes: len(b.Data)}, nil
}
