package tracing

import (
	"encoding/json"
	"strings"

	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/protocol"
)

// DetailsEvent is the name of the span event that holds the content of a model call.
const DetailsEvent = "gen_ai.client.inference.operation.details"

type part map[string]any

type message struct {
	Role         string `json:"role"`
	Parts        []part `json:"parts"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// Messages returns the JSON of msgs in the shape of gen_ai.input.messages. A
// blob is a reference to its key and never its bytes, and text goes through
// the secret mask.
func Messages(msgs []protocol.LogMessage) string { return encode(shape(msgs, "")) }

// Output returns the JSON of msgs in the shape of gen_ai.output.messages, with finish as the finish reason.
func Output(msgs []protocol.LogMessage, finish string) string { return encode(shape(msgs, finish)) }

// SystemInstructions returns the JSON of text in the shape of gen_ai.system_instructions.
func SystemInstructions(text string) string {
	return encode([]part{{"type": "text", "content": toolresult.Mask(text)}})
}

// ToolDefinitions returns the JSON of specs in the shape of gen_ai.tool.definitions.
func ToolDefinitions(specs []protocol.ToolSpec) string {
	out := make([]part, len(specs))
	for i, s := range specs {
		out[i] = part{"type": "function", "name": s.Name, "description": s.Description, "parameters": json.RawMessage(s.InputSchema)}
	}
	return encode(out)
}

func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func shape(msgs []protocol.LogMessage, finish string) []message {
	out := make([]message, 0, len(msgs))
	for _, m := range msgs {
		o := message{Role: m.Role, Parts: []part{}, FinishReason: finish}
		for _, p := range m.Parts {
			o.Parts = append(o.Parts, partOf(p)...)
		}
		out = append(out, o)
	}
	return out
}

func partOf(p protocol.LogPart) []part {
	switch p.Type {
	case protocol.LogPartText:
		return []part{{"type": "text", "content": toolresult.Mask(p.Text)}}
	case protocol.LogPartReasoning:
		return []part{{"type": "reasoning", "content": toolresult.Mask(p.Text)}}
	case protocol.LogPartToolCall:
		return []part{{"type": "tool_call", "id": p.CallID, "name": p.Name, "arguments": arguments(p.Arguments)}}
	case protocol.LogPartToolResult:
		out := []part{{"type": "tool_call_response", "id": p.CallID, "response": toolresult.Mask(p.Text)}}
		for _, b := range p.Blobs {
			out = append(out, partOf(b)...)
		}
		return out
	case protocol.LogPartBlob:
		return []part{{"type": "file", "modality": modality(p.MediaType), "mime_type": p.MediaType, "file_id": p.BlobKey, "size_bytes": p.Bytes}}
	}
	return []part{{"type": p.Type, "content": toolresult.Mask(p.Text)}}
}

func modality(mediaType string) string {
	if strings.HasPrefix(mediaType, "image/") {
		return "image"
	}
	return "document"
}

// arguments returns the masked arguments of a call as JSON when they stay valid, else as a string.
func arguments(raw json.RawMessage) any {
	masked := toolresult.Mask(string(raw))
	if json.Valid([]byte(masked)) {
		return json.RawMessage(masked)
	}
	return masked
}

// Arguments returns the masked arguments of a tool call as a JSON string.
func Arguments(raw json.RawMessage) string { return encode(arguments(raw)) }

// Result returns the masked text of a tool result.
func Result(text string) string { return toolresult.Mask(text) }
