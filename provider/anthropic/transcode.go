package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/majorcontext/harness/imageclamp"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// Family is the provider family key: ModelRef.Provider values and
// ProviderData tags this adapter reads and writes.
const Family = "anthropic"

// imageLimits are the Anthropic-family image caps imageclamp enforces at
// transcode time so a poisoned transcript heals on its next request build.
// The Anthropic API and Bedrock reject a side over 8000px ("image dimensions
// exceed max allowed size: 8000 pixels") and, with >20 image/document blocks,
// over 2000px; the direct API also rejects a single image over 10MB base64
// (Bedrock over 5MB — the stricter openaicompat/OpenRouter path carries that
// tighter budget). TargetDim is Claude's ~2576px processing edge: the most any
// model actually consumes.
var imageLimits = imageclamp.Limits{
	MaxDim:             8000,
	TargetDim:          2576,
	ManyImageThreshold: 20,
	ManyImageDim:       2000,
	MaxImageBytes:      10_000_000,
	RecurseToolResults: true,
}

type apiRequest struct {
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	System      []apiBlock   `json:"system,omitempty"`
	Messages    []apiMessage `json:"messages"`
	Tools       []apiToolDef `json:"tools,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	TopP        *float64     `json:"top_p,omitempty"`
	Stream      bool         `json:"stream"`
	Thinking    *apiThinking `json:"thinking,omitempty"`
}

// apiThinking enables Anthropic extended thinking with a token budget. The API
// requires max_tokens strictly greater than budget_tokens, and rejects an
// explicit temperature or top_p while thinking is enabled — transcodeRequest
// enforces both.
type apiThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

// thinkingBudget maps a unified reasoning-effort level to an Anthropic thinking
// budget in tokens. It returns (0, false) for a level that requests no
// reasoning (EffortUnset, EffortOff), so the caller emits no thinking block.
// 1024 is the Anthropic minimum budget.
func thinkingBudget(e message.Effort) (int, bool) {
	switch e {
	case message.EffortMinimal:
		return 1024, true
	case message.EffortLow:
		return 4096, true
	case message.EffortMedium:
		return 8192, true
	case message.EffortHigh:
		return 16384, true
	default:
		return 0, false
	}
}

// thinkingCompletionMargin is the minimum room left for the visible answer
// above the thinking budget, since the API requires max_tokens > budget_tokens.
const thinkingCompletionMargin = 4096

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

	// Extended thinking: a non-off effort level enables a thinking block with a
	// token budget. The API requires max_tokens > budget_tokens and rejects an
	// explicit temperature or top_p while thinking is on, so drop both and bump
	// max_tokens above the budget when needed.
	//
	// KNOWN LIMITATION (live-gated): enabling thinking mid-session does not
	// synthesize a leading thinking block for a prior assistant turn that made
	// tool calls without one. The API can reject a request that continues such a
	// turn ("thinking blocks expected before tool_use"). Normally the engine's
	// agentic loop finishes a tool cycle within one turn, so a caller toggling
	// effort between turns faces a final assistant TEXT turn, not a dangling
	// tool_use. The one supported path that DOES leave the risky shape is POST
	// /session/{id}/abort mid-tool-call: it leaves a partial tool_use plus a
	// synthetic tool-result and no thinking block, and a following effort-enabled
	// prompt then continues it. A robust fix (synthesize or relocate at transcode
	// time) is deferred pending live confirmation of the exact reject shape; the
	// //go:build live suite should exercise abort-then-enable. See the
	// reasoning-effort PR.
	if budget, ok := thinkingBudget(req.Effort); ok {
		out.Thinking = &apiThinking{Type: "enabled", BudgetTokens: budget}
		if out.MaxTokens < budget+thinkingCompletionMargin {
			out.MaxTokens = budget + thinkingCompletionMargin
		}
		out.Temperature = nil
		out.TopP = nil
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

	// Defense-in-depth against a poisoned history (incident
	// ses_01kx48z4rqfkpbwmzfdv1jzeg6): a ToolCall with no matching
	// ToolResult in the immediately-following wire turn would otherwise
	// transcode to a dangling tool_use block, which the Anthropic API
	// rejects wholesale with HTTP 400 "tool_use ids were found without
	// tool_result blocks immediately after". engine.Session's turn loop is
	// the primary fix and keeps its own ingest self-consistent (see
	// engine/engine.go), but this backstops any OTHER producer of history
	// — a plugin hook, a hand-rolled adapter, a replayed log from an
	// older binary — so a request never ships an orphaned tool_use.
	// NormalizeForWire (NEP-5293 part 2), not ResolveOrphanToolCalls,
	// belongs here: this call site builds one throwaway request and never
	// touches the durable record, so the destructive/relocating repairs
	// only NormalizeForWire performs are safe here specifically. See its
	// doc comment for the full incident and the additive/transcode-only
	// split.
	messages := imageclamp.Clamp(message.NormalizeForWire(req.Messages), imageLimits)

	for i := range messages {
		m := &messages[i]
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
			// SafeContent, not Content: the canonical-layer guarantee
			// (message.Message.Normalize, at both live append and
			// LoadSession) is the primary fix, but this is the last stop
			// before the wire, for a ToolResult built by a producer that
			// bypasses Normalize entirely — see SafeContent's doc comment.
			content, err := transcodeParts(v.SafeContent())
			if err != nil {
				return nil, err
			}
			if content == nil {
				// Should not happen once SafeContent has run — kept as a
				// defensive fallback so a non-empty Content whose parts
				// all happen to transcode to nothing still ships a
				// present, non-empty content array rather than an omitted
				// key (see the incident in SafeContent's own doc comment).
				content = []apiBlock{{Type: "text", Text: message.NoToolOutputText}}
			}
			blocks = append(blocks, apiBlock{
				Type:      "tool_result",
				ToolUseID: wireCallID(v.CallID),
				Content:   content,
				IsError:   v.IsError,
			})

		case *message.Reasoning:
			raw, ok := v.ProviderData.Get(Family)
			if !ok {
				// Another provider's reasoning, or a present-but-empty
				// entry (see message.ProviderData.Get): dropped, per the
				// canonical format's crossing rule.
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
