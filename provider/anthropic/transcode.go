package anthropic

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
const Family = "anthropic"

type apiRequest struct {
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	System      []apiBlock   `json:"system,omitempty"`
	Messages    []apiMessage `json:"messages"`
	Tools       []apiToolDef `json:"tools,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	TopP        *float64     `json:"top_p,omitempty"`
	Stream      bool         `json:"stream"`
}

type apiMessage struct {
	Role    string     `json:"role"`
	Content []apiBlock `json:"content"`
}

// apiBlock is a union of Anthropic content block shapes, discriminated by
// Type: text, image, document, tool_use, tool_result, thinking,
// redacted_thinking.
type apiBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	Source *apiSource `json:"source,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string     `json:"tool_use_id,omitempty"`
	Content   []apiBlock `json:"content,omitempty"`
	IsError   bool       `json:"is_error,omitempty"`

	// Thinking is a pointer because the API requires the field on thinking
	// blocks even when empty — omitempty on a plain string drops it.
	Thinking  *string `json:"thinking,omitempty"`
	Signature string  `json:"signature,omitempty"`

	Data string `json:"data,omitempty"` // redacted_thinking

	CacheControl *apiCacheControl `json:"cache_control,omitempty"`
}

type apiSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type apiToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type apiCacheControl struct {
	Type string `json:"type"` // ephemeral
}

// anthropicReasoningData is the shape stored under ProviderData[Family] on
// Reasoning parts: the signature for thinking blocks, or the opaque payload
// for redacted_thinking blocks.
type anthropicReasoningData struct {
	Signature string `json:"signature,omitempty"`
	Redacted  string `json:"redacted,omitempty"`
}

var ephemeral = &apiCacheControl{Type: "ephemeral"}

// wireIDPattern is what the API accepts for client-supplied tool_use IDs.
var wireIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// wireCallID preserves the canonical CallID when it is already wire-safe
// (true for calls that originated on Anthropic, keeping the prompt cache
// warm) and derives a deterministic compliant ID otherwise.
func wireCallID(id string) string {
	if wireIDPattern.MatchString(id) {
		return id
	}
	return message.ProviderCallID("toolu_", id, 64)
}

// transcodeRequest maps a canonical request to the Anthropic Messages API.
// Cache markers are injected here — on the last system block and the last
// content block of the final message — and never stored in the session log.
func transcodeRequest(req *provider.Request) (*apiRequest, error) {
	out := &apiRequest{
		Model:       req.Model.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      true,
	}

	for _, seg := range req.System {
		out.System = append(out.System, apiBlock{Type: "text", Text: seg})
	}
	if n := len(out.System); n > 0 {
		out.System[n-1].CacheControl = ephemeral
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, apiToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	for i := range req.Messages {
		m := &req.Messages[i]
		role := "user"
		if m.Role == message.RoleAssistant {
			role = "assistant"
		}
		blocks, err := transcodeParts(m.Parts)
		if err != nil {
			return nil, fmt.Errorf("anthropic: message %s: %w", m.ID, err)
		}
		if len(blocks) == 0 {
			// A message can transcode to nothing — e.g. an assistant turn
			// whose only content was another provider's reasoning.
			continue
		}
		// The API requires strict user/assistant alternation; merge
		// adjacent same-role messages.
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
		} else {
			out.Messages = append(out.Messages, apiMessage{Role: role, Content: blocks})
		}
	}
	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("anthropic: request has no transcodable messages")
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content[len(last.Content)-1].CacheControl = ephemeral

	return out, nil
}

func transcodeParts(parts message.Parts) ([]apiBlock, error) {
	var blocks []apiBlock
	for _, p := range parts {
		switch v := p.(type) {
		case *message.Text:
			if v.Text == "" {
				continue
			}
			blocks = append(blocks, apiBlock{Type: "text", Text: v.Text})

		case *message.Blob:
			b, err := transcodeBlob(v)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, b)

		case *message.ToolCall:
			input := v.Arguments
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			blocks = append(blocks, apiBlock{
				Type:  "tool_use",
				ID:    wireCallID(v.CallID),
				Name:  v.Name,
				Input: input,
			})

		case *message.ToolResult:
			content, err := transcodeParts(v.Content)
			if err != nil {
				return nil, err
			}
			if content == nil {
				content = []apiBlock{}
			}
			blocks = append(blocks, apiBlock{
				Type:      "tool_result",
				ToolUseID: wireCallID(v.CallID),
				Content:   content,
				IsError:   v.IsError,
			})

		case *message.Reasoning:
			raw, ok := v.ProviderData[Family]
			if !ok {
				// Another provider's reasoning: dropped, per the canonical
				// format's crossing rule.
				continue
			}
			var data anthropicReasoningData
			if err := json.Unmarshal(raw, &data); err != nil {
				return nil, fmt.Errorf("bad anthropic reasoning data: %w", err)
			}
			if data.Redacted != "" {
				blocks = append(blocks, apiBlock{Type: "redacted_thinking", Data: data.Redacted})
			} else {
				thinking := v.Text
				blocks = append(blocks, apiBlock{Type: "thinking", Thinking: &thinking, Signature: data.Signature})
			}

		default:
			return nil, fmt.Errorf("unsupported part type %T", p)
		}
	}
	return blocks, nil
}

func transcodeBlob(b *message.Blob) (apiBlock, error) {
	blockType := "document"
	if strings.HasPrefix(b.MediaType, "image/") {
		blockType = "image"
	}
	if b.URL != "" {
		return apiBlock{Type: blockType, Source: &apiSource{Type: "url", URL: b.URL}}, nil
	}
	if len(b.Data) == 0 {
		return apiBlock{}, fmt.Errorf("blob has neither data nor url")
	}
	return apiBlock{Type: blockType, Source: &apiSource{
		Type:      "base64",
		MediaType: b.MediaType,
		Data:      base64.StdEncoding.EncodeToString(b.Data),
	}}, nil
}
