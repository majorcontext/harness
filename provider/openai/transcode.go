package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// Family is the provider family key: ModelRef.Provider values and
// ProviderData tags this adapter reads and writes.
const Family = "openai"

// apiRequest is the OpenAI Responses API request body. Input is a flat,
// heterogeneous list of items encoded as raw JSON so that stored reasoning
// items can be replayed verbatim.
type apiRequest struct {
	Model           string            `json:"model"`
	Instructions    string            `json:"instructions,omitempty"`
	Input           []json.RawMessage `json:"input"`
	Tools           []apiToolDef      `json:"tools,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	TopP            *float64          `json:"top_p,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	Stream          bool              `json:"stream"`
	// Store is always false and Include always requests encrypted reasoning so
	// multi-turn conversations work without server-side response state.
	Store   bool     `json:"store"`
	Include []string `json:"include"`
}

type apiToolDef struct {
	Type        string          `json:"type"` // always "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// apiMessageItem is an input item of type "message": a role plus a list of
// content parts (input_text, input_image, output_text).
type apiMessageItem struct {
	Type    string           `json:"type"` // "message"
	Role    string           `json:"role"`
	Content []apiContentPart `json:"content"`
}

type apiContentPart struct {
	Type     string `json:"type"` // input_text | output_text | input_image | input_file
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

type apiFunctionCall struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type apiFunctionCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// wireIDPattern is what OpenAI accepts for client-supplied call IDs.
var wireIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// wireCallID preserves the canonical CallID when it is already wire-safe
// (true for calls that originated on OpenAI, keeping the prompt cache warm)
// and derives a deterministic compliant ID otherwise.
func wireCallID(id string) string {
	if wireIDPattern.MatchString(id) {
		return id
	}
	return message.ProviderCallID("call_", id, 64)
}

// transcodeRequest maps a canonical request to the OpenAI Responses API.
func transcodeRequest(req *provider.Request) (*apiRequest, error) {
	out := &apiRequest{
		Model:           req.Model.Model,
		Instructions:    strings.Join(req.System, "\n\n"),
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		MaxOutputTokens: req.MaxTokens,
		Stream:          true,
		Store:           false,
		Include:         []string{"reasoning.encrypted_content"},
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, apiToolDef{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}

	for i := range req.Messages {
		m := &req.Messages[i]
		items, err := transcodeMessage(m)
		if err != nil {
			return nil, fmt.Errorf("openai: message %s: %w", m.ID, err)
		}
		out.Input = append(out.Input, items...)
	}
	if len(out.Input) == 0 {
		return nil, fmt.Errorf("openai: request has no transcodable messages")
	}
	return out, nil
}

// transcodeMessage expands one canonical message into a sequence of Responses
// input items. Contiguous text/image parts are grouped into a single message
// item; tool calls, tool results, and reasoning are each their own item.
func transcodeMessage(m *message.Message) ([]json.RawMessage, error) {
	role := "user"
	if m.Role == message.RoleAssistant {
		role = "assistant"
	}

	var items []json.RawMessage
	var pending *apiMessageItem

	flush := func() error {
		if pending != nil && len(pending.Content) > 0 {
			raw, err := json.Marshal(pending)
			if err != nil {
				return err
			}
			items = append(items, raw)
		}
		pending = nil
		return nil
	}

	for _, p := range m.Parts {
		switch v := p.(type) {
		case *message.Text:
			if v.Text == "" {
				continue
			}
			if pending == nil {
				pending = &apiMessageItem{Type: "message", Role: role}
			}
			ct := "input_text"
			if role == "assistant" {
				ct = "output_text"
			}
			pending.Content = append(pending.Content, apiContentPart{Type: ct, Text: v.Text})

		case *message.Blob:
			part, err := transcodeBlob(v)
			if err != nil {
				return nil, err
			}
			if pending == nil {
				pending = &apiMessageItem{Type: "message", Role: role}
			}
			pending.Content = append(pending.Content, part)

		case *message.ToolCall:
			if err := flush(); err != nil {
				return nil, err
			}
			args := string(v.Arguments)
			if args == "" {
				args = "{}"
			}
			raw, err := json.Marshal(apiFunctionCall{
				Type:      "function_call",
				CallID:    wireCallID(v.CallID),
				Name:      v.Name,
				Arguments: args,
			})
			if err != nil {
				return nil, err
			}
			items = append(items, raw)

		case *message.ToolResult:
			if err := flush(); err != nil {
				return nil, err
			}
			raw, err := json.Marshal(apiFunctionCallOutput{
				Type:   "function_call_output",
				CallID: wireCallID(v.CallID),
				Output: toolResultOutput(v),
			})
			if err != nil {
				return nil, err
			}
			items = append(items, raw)

		case *message.Reasoning:
			if err := flush(); err != nil {
				return nil, err
			}
			raw, ok := v.ProviderData[Family]
			if !ok {
				// Another provider's reasoning: dropped, per the canonical
				// format's crossing rule.
				continue
			}
			// Replay the stored raw reasoning item verbatim.
			items = append(items, append(json.RawMessage(nil), raw...))

		default:
			return nil, fmt.Errorf("unsupported part type %T", p)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return items, nil
}

// toolResultOutput flattens a ToolResult into the string-valued output field
// of a function_call_output item, which has no boolean error field and (as
// far as this adapter assumes) no array content form. IsError is encoded as a
// marker prefix so the model can distinguish failed/denied calls, and Blob
// parts — which cannot be carried in the string — are surfaced with an
// explicit omission note rather than dropped silently.
func toolResultOutput(v *message.ToolResult) string {
	out := v.Content.Text()
	blobs := 0
	for _, p := range v.Content {
		if _, ok := p.(*message.Blob); ok {
			blobs++
		}
	}
	if blobs > 0 {
		note := fmt.Sprintf("[%d image attachment(s) omitted]", blobs)
		if out == "" {
			out = note
		} else {
			out += "\n" + note
		}
	}
	if v.IsError {
		out = "[tool error] " + out
	}
	return out
}

// transcodeBlob maps a Blob to a Responses content part by media type:
// image/* → input_image, application/pdf → input_file. Anything else is a
// loud error — silently mis-typing a document as an image is worse.
func transcodeBlob(b *message.Blob) (apiContentPart, error) {
	switch {
	case strings.HasPrefix(b.MediaType, "image/"):
		if b.URL != "" {
			return apiContentPart{Type: "input_image", ImageURL: b.URL}, nil
		}
		if len(b.Data) == 0 {
			return apiContentPart{}, fmt.Errorf("blob has neither data nor url")
		}
		return apiContentPart{Type: "input_image", ImageURL: dataURL(b)}, nil

	case b.MediaType == "application/pdf":
		if len(b.Data) == 0 {
			// The input_file part is only emitted with inline file_data; a
			// URL-referenced form is not verified against the API, so fail
			// loudly rather than guess at the wire shape.
			return apiContentPart{}, fmt.Errorf("application/pdf blob by URL is not supported; provide inline data")
		}
		return apiContentPart{Type: "input_file", Filename: "file.pdf", FileData: dataURL(b)}, nil

	default:
		return apiContentPart{}, fmt.Errorf("unsupported blob media type %q", b.MediaType)
	}
}

func dataURL(b *message.Blob) string {
	return "data:" + b.MediaType + ";base64," + base64.StdEncoding.EncodeToString(b.Data)
}
