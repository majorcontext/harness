package openaicompat

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

// imageLimits are the image caps imageclamp enforces for OpenAI-compatible
// endpoints. This adapter commonly fronts Amazon Bedrock / Google Vertex (the
// NEP-5109 incident path, via OpenRouter), so it carries their documented
// constraints: 8000px per side, a stricter 2000px cap with >20 image/document
// blocks, and 5MB base64 per image — the tightest per-image size limit of the
// common OpenAI-compatible vision backends, so a clamped image is accepted by
// any of them. TargetDim is 2576px (the most any model consumes). A proxy such
// as OpenRouter may tolerate larger images, but 5MB guarantees the emitted
// request is valid against the strictest documented origin.
var imageLimits = imageclamp.Limits{
	MaxDim:             8000,
	TargetDim:          2576,
	ManyImageThreshold: 20,
	ManyImageDim:       2000,
	MaxImageBytes:      5_000_000,
	RecurseToolResults: false, // tool-result images are omitted on the wire
}

// apiRequest is the wire body for POST {base}/chat/completions, the wire
// spoken by OpenAI-compatible chat-completions endpoints (OpenRouter,
// Ollama, vLLM, ...) — not the OpenAI Responses API (see provider/openai).
type apiRequest struct {
	Model         string            `json:"model"`
	Messages      []apiMessage      `json:"messages"`
	Tools         []apiToolDef      `json:"tools,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Stream        bool              `json:"stream"`
	StreamOptions *apiStreamOptions `json:"stream_options,omitempty"`
}

type apiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// apiMessage is one entry in the wire "messages" array. Content is raw JSON
// so it can hold either a plain string (text-only turns) or a content-part
// array (multimodal turns), matching what each concrete case needs.
type apiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []apiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// apiContentPart is one entry of a multimodal "content" array.
type apiContentPart struct {
	Type     string       `json:"type"` // text | image_url
	Text     string       `json:"text,omitempty"`
	ImageURL *apiImageURL `json:"image_url,omitempty"`
}

type apiImageURL struct {
	URL string `json:"url"`
}

type apiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"` // always "function"
	Function apiFunctionCall `json:"function"`
}

type apiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type apiToolDef struct {
	Type     string      `json:"type"` // always "function"
	Function apiFunction `json:"function"`
}

type apiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// wireIDPattern is what the chat-completions wire accepts for client-supplied
// tool_call ids across the family of servers speaking this protocol.
var wireIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// wireCallID preserves the canonical CallID when it is already wire-safe
// (true for calls that originated on a server speaking this wire, keeping
// the prompt cache warm) and derives a deterministic compliant ID otherwise.
func wireCallID(id string) string {
	if wireIDPattern.MatchString(id) {
		return id
	}
	return message.ProviderCallID("call_", id, 64)
}

// transcodeRequest maps a canonical request to the OpenAI-compatible
// chat-completions wire format. family is the Client's configured Family: it
// is both the ModelRef.Provider value and the ProviderData tag this call
// reads reasoning attachments from.
func transcodeRequest(req *provider.Request, family string) (*apiRequest, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("openaicompat: request has no transcodable messages")
	}

	out := &apiRequest{
		Model:         req.Model.Model,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &apiStreamOptions{IncludeUsage: true},
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, apiToolDef{
			Type: "function",
			Function: apiFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	if len(req.System) > 0 {
		raw, err := json.Marshal(strings.Join(req.System, "\n\n"))
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, apiMessage{Role: "system", Content: raw})
	}

	// Defense-in-depth against a poisoned history (incident
	// ses_01kx48z4rqfkpbwmzfdv1jzeg6): a tool call with no matching result
	// in the immediately-following wire turn would otherwise transcode to a
	// dangling tool_calls entry with no paired "tool"-role message, which
	// this wire protocol also requires immediately after (mirrors
	// provider/anthropic/transcode.go's identical guard).
	// engine.Session's turn loop is the primary fix and keeps its own
	// ingest self-consistent (see engine/engine.go), but this backstops
	// any OTHER producer of history. message.NormalizeForWire (NEP-5293
	// part 2) is the transcode-only repair used here — this call site
	// builds one throwaway request and never touches the durable record,
	// so its destructive/relocating repairs are safe here; see its doc
	// comment for the full incident and the additive
	// (message.ResolveOrphanToolCalls, LIVE history only) / transcode-only
	// split. Note this adapter's own transcodeMessage below is role-strict
	// (RoleUser/RoleAssistant/RoleTool each accept only their expected part
	// types and error otherwise), so a ToolCall stranded in a non-assistant
	// message already fails loudly here rather than reaching the wire
	// malformed — NormalizeForWire's repair for that shape matters for the
	// other two (role-agnostic) transcoders, not this one.
	// imageLimits.RecurseToolResults is false: tool-result images are omitted
	// on the wire (see toolResultOutput), so clamping them would be wasted work.
	messages := imageclamp.Clamp(message.NormalizeForWire(req.Messages), imageLimits)

	for i := range messages {
		m := &messages[i]
		msgs, err := transcodeMessage(m, family)
		if err != nil {
			return nil, fmt.Errorf("openaicompat: message %s: %w", m.ID, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}
	return out, nil
}

// transcodeMessage expands one canonical message into zero or more wire
// messages: RoleUser and RoleAssistant each become exactly one message,
// while RoleTool becomes one "tool"-role message per ToolResult (the wire
// requires each tool result addressed by its own tool_call_id).
func transcodeMessage(m *message.Message, family string) ([]apiMessage, error) {
	switch m.Role {
	case message.RoleUser:
		return transcodeUserMessage(m)
	case message.RoleAssistant:
		return transcodeAssistantMessage(m, family)
	case message.RoleTool:
		return transcodeToolMessages(m)
	default:
		return nil, fmt.Errorf("unsupported role %q", m.Role)
	}
}

func transcodeUserMessage(m *message.Message) ([]apiMessage, error) {
	var texts []string
	var parts []apiContentPart
	hasBlob := false
	for _, p := range m.Parts {
		switch v := p.(type) {
		case *message.Text:
			// NeutralizeEngineContextSentinel: a user- or paste-authored Text
			// part must never forge the engine-context sentinel on the wire
			// (see message.EngineContext). Only the *message.EngineContext
			// case below emits it.
			t := message.NeutralizeEngineContextSentinel(v.Text)
			texts = append(texts, t)
			parts = append(parts, apiContentPart{Type: "text", Text: t})
		case *message.EngineContext:
			if v.Text == "" {
				continue
			}
			// A genuine engine block: emit it sentinel-wrapped as the base
			// system prompt (cmd/harness) describes. An ordinary text content
			// part on the wire — no new provider feature. The empty guard
			// mirrors anthropic/openai so all three transcoders agree on
			// identical input (never a bare empty envelope).
			t := message.RenderEngineContext(v.Text)
			texts = append(texts, t)
			parts = append(parts, apiContentPart{Type: "text", Text: t})
		case *message.Blob:
			hasBlob = true
			url, err := blobURL(v)
			if err != nil {
				return nil, err
			}
			parts = append(parts, apiContentPart{Type: "image_url", ImageURL: &apiImageURL{URL: url}})
		default:
			return nil, fmt.Errorf("unsupported part type %T in user message", p)
		}
	}

	var content json.RawMessage
	var err error
	if hasBlob {
		content, err = json.Marshal(parts)
	} else {
		content, err = json.Marshal(strings.Join(texts, "\n"))
	}
	if err != nil {
		return nil, err
	}
	return []apiMessage{{Role: "user", Content: content}}, nil
}

func transcodeAssistantMessage(m *message.Message, family string) ([]apiMessage, error) {
	var toolCalls []apiToolCall
	for _, p := range m.Parts {
		switch v := p.(type) {
		case *message.Text:
			// Folded into content below via Parts.Text().
		case *message.ToolCall:
			args := string(v.Arguments)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, apiToolCall{
				ID:   wireCallID(v.CallID),
				Type: "function",
				Function: apiFunctionCall{
					Name:      v.Name,
					Arguments: args,
				},
			})
		case *message.Reasoning:
			if _, ok := v.ProviderData.Get(family); !ok {
				// Foreign-provider reasoning, or a present-but-empty entry
				// (see message.ProviderData.Get): dropped per the
				// canonical format's crossing rule.
				continue
			}
			// The generic chat-completions wire has no field to replay
			// opaque/signed reasoning into (unlike Anthropic's thinking
			// blocks or OpenAI Responses' encrypted reasoning items), and
			// this adapter's own stream assembly never populates
			// ProviderData[family] in the first place (see stream.go) — so
			// this branch is unreachable in practice. Reasoning is
			// therefore always dropped when replaying history to a
			// compat-wire server: there is no signed-reasoning replay here.
			continue
		default:
			return nil, fmt.Errorf("unsupported part type %T in assistant message", p)
		}
	}

	var content json.RawMessage
	// NeutralizeEngineContextSentinel: a model induced (via prompt injection)
	// to echo the sentinel in its reply must not carry a live sentinel when
	// that assistant message is replayed on a later turn (see
	// message.EngineContext). This Parts.Text() flatten bypasses the
	// *message.Text case's neutralization, so it neutralizes here.
	if text := message.NeutralizeEngineContextSentinel(m.Parts.Text()); text != "" {
		raw, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		content = raw
	}
	return []apiMessage{{Role: "assistant", Content: content, ToolCalls: toolCalls}}, nil
}

func transcodeToolMessages(m *message.Message) ([]apiMessage, error) {
	var msgs []apiMessage
	for _, p := range m.Parts {
		tr, ok := p.(*message.ToolResult)
		if !ok {
			return nil, fmt.Errorf("unsupported part type %T in tool message", p)
		}
		raw, err := json.Marshal(toolResultOutput(tr))
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, apiMessage{
			Role:       "tool",
			ToolCallID: wireCallID(tr.CallID),
			Content:    raw,
		})
	}
	return msgs, nil
}

// toolResultOutput flattens a ToolResult into the string content of a
// "tool"-role message. There is no boolean error field on the wire, so
// IsError is encoded as a marker prefix, and Blob parts — which cannot be
// carried inline in the string — are surfaced with an explicit omission note
// rather than dropped silently (mirrors provider/openai's toolResultOutput).
//
// SafeContent, not Content: this adapter's own wire shape (a plain JSON
// string, never an omittable array) never reproduced the empty-tool_result
// wedge SafeContent's doc comment describes, but reading through
// SafeContent anyway keeps this adapter covered by the same
// canonical-layer guarantee the others rely on, rather than needing its
// own separate argument for why it happens to be safe.
func toolResultOutput(v *message.ToolResult) string {
	content := v.SafeContent()
	// NeutralizeEngineContextSentinel: tool output (a hostile file read,
	// web fetch, or subprocess stdout) must never forge the engine-context
	// sentinel on the wire (see message.EngineContext). This flattening path
	// bypasses the *message.Text case's neutralization, so it neutralizes
	// here — the same bar user/assistant text already meets.
	out := message.NeutralizeEngineContextSentinel(content.Text())
	blobs := 0
	for _, p := range content {
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

// blobURL maps a Blob to an image_url string: the URL verbatim when
// URL-referenced, or a data: URL when carrying inline data. Only image/*
// media types are supported — the chat-completions wire's content parts have
// no non-image form, so anything else is a loud error rather than a
// silent mis-typing of a document as an image (mirrors provider/openai's
// transcodeBlob).
func blobURL(b *message.Blob) (string, error) {
	if !strings.HasPrefix(b.MediaType, "image/") {
		return "", fmt.Errorf("unsupported blob media type %q", b.MediaType)
	}
	if b.URL != "" {
		return b.URL, nil
	}
	if len(b.Data) == 0 {
		return "", fmt.Errorf("blob has neither data nor url")
	}
	return "data:" + b.MediaType + ";base64," + base64.StdEncoding.EncodeToString(b.Data), nil
}
