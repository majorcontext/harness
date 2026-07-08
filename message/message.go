// Package message defines the canonical message format stored in session
// logs.
//
// The session log stores this format and never a provider's wire format.
// Provider adapters transcode canonical history to and from each API's wire
// format from scratch on every request (stateless transcoding), which is what
// makes mid-session model swaps a no-op: the next request simply uses a
// different transcoder.
//
// Provider-specific state that cannot cross providers (signed thinking
// blocks, encrypted reasoning items) is carried as opaque, provider-tagged
// attachments (ProviderData): replayed verbatim to the same provider family,
// dropped when the history is transcoded for a different one.
package message

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Role identifies the author of a Message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool carries tool results back to the model. A RoleTool message
	// contains only ToolResult parts.
	RoleTool Role = "tool"
)

// Message is one entry in a session's history.
//
// The system prompt is deliberately not part of history: it is assembled per
// request from config and the system.transform hook chain, then injected by
// the transcoder.
type Message struct {
	ID    string `json:"id"`
	Role  Role   `json:"role"`
	Parts Parts  `json:"parts"`
	// Model records which model produced an assistant message. It is zero
	// for user and tool messages.
	Model     ModelRef  `json:"model,omitzero"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// PartType discriminates the concrete type of a Part in JSON.
type PartType string

const (
	PartText       PartType = "text"
	PartBlob       PartType = "blob"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
	PartReasoning  PartType = "reasoning"
)

// Part is one content block within a Message. Concrete part types are always
// used as pointers (*Text, *Blob, ...); value types do not implement Part.
type Part interface {
	partType() PartType
}

// Text is a plain text block.
type Text struct {
	Text string `json:"text"`
}

func (*Text) partType() PartType { return PartText }

// Blob is binary content (image, PDF, ...) either inline or by URL.
type Blob struct {
	MediaType string `json:"media_type"`
	// Data holds inline content (base64 in JSON). Mutually exclusive with URL.
	Data []byte `json:"data,omitempty"`
	URL  string `json:"url,omitempty"`
}

func (*Blob) partType() PartType { return PartBlob }

// ToolCall is a model-issued request to run a tool.
type ToolCall struct {
	// CallID is harness-internal. Transcoders derive provider-compliant IDs
	// from it deterministically (see ProviderCallID) so retranscoding a
	// history yields byte-identical wire requests.
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (*ToolCall) partType() PartType { return PartToolCall }

// ToolResult is the outcome of a ToolCall. Content may hold Text and Blob
// parts only.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content Parts  `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

func (*ToolResult) partType() PartType { return PartToolResult }

// Reasoning is a model reasoning block.
type Reasoning struct {
	// Text is the human-readable reasoning summary. It is safe to render and
	// to downgrade to plain text when crossing providers.
	Text string `json:"text,omitempty"`
	// ProviderData holds opaque provider-native reasoning state, keyed by
	// provider family (e.g. "anthropic", "openai-responses").
	ProviderData ProviderData `json:"provider_data,omitempty"`
}

func (*Reasoning) partType() PartType { return PartReasoning }

// ProviderData carries opaque provider-native state keyed by provider family.
// Transcoders replay the entry matching their own family verbatim and ignore
// the rest.
type ProviderData map[string]json.RawMessage

// Parts is a list of message parts with polymorphic JSON encoding: each part
// is an object carrying a "type" discriminator alongside its fields.
type Parts []Part

// Text returns the concatenation of all Text parts, joined with newlines.
func (ps Parts) Text() string {
	var b strings.Builder
	for _, p := range ps {
		if t, ok := p.(*Text); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func (ps Parts) MarshalJSON() ([]byte, error) {
	raws := make([]json.RawMessage, len(ps))
	for i, p := range ps {
		raw, err := marshalPart(p)
		if err != nil {
			return nil, err
		}
		raws[i] = raw
	}
	return json.Marshal(raws)
}

func (ps *Parts) UnmarshalJSON(b []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		return err
	}
	out := make(Parts, 0, len(raws))
	for _, raw := range raws {
		p, err := unmarshalPart(raw)
		if err != nil {
			return err
		}
		out = append(out, p)
	}
	*ps = out
	return nil
}

func marshalPart(p Part) ([]byte, error) {
	switch v := p.(type) {
	case *Text:
		return json.Marshal(struct {
			Type PartType `json:"type"`
			*Text
		}{PartText, v})
	case *Blob:
		return json.Marshal(struct {
			Type PartType `json:"type"`
			*Blob
		}{PartBlob, v})
	case *ToolCall:
		return json.Marshal(struct {
			Type PartType `json:"type"`
			*ToolCall
		}{PartToolCall, v})
	case *ToolResult:
		return json.Marshal(struct {
			Type PartType `json:"type"`
			*ToolResult
		}{PartToolResult, v})
	case *Reasoning:
		return json.Marshal(struct {
			Type PartType `json:"type"`
			*Reasoning
		}{PartReasoning, v})
	default:
		return nil, fmt.Errorf("message: cannot marshal part type %T", p)
	}
}

func unmarshalPart(raw json.RawMessage) (Part, error) {
	var head struct {
		Type PartType `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	var p Part
	switch head.Type {
	case PartText:
		p = new(Text)
	case PartBlob:
		p = new(Blob)
	case PartToolCall:
		p = new(ToolCall)
	case PartToolResult:
		p = new(ToolResult)
	case PartReasoning:
		p = new(Reasoning)
	default:
		return nil, fmt.Errorf("message: unknown part type %q", head.Type)
	}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	return p, nil
}

// ProviderCallID derives a deterministic, provider-safe tool-call ID from a
// canonical CallID. The same input always yields the same output, so
// retranscoding an unchanged history produces identical wire requests —
// which keeps provider prompt caches warm across turns.
//
// prefix is the provider's required ID prefix (e.g. "toolu_", "call_");
// maxLen truncates the final ID when > 0.
func ProviderCallID(prefix, callID string, maxLen int) string {
	sum := sha256.Sum256([]byte(callID))
	id := prefix + hex.EncodeToString(sum[:])
	if maxLen > 0 && len(id) > maxLen {
		id = id[:maxLen]
	}
	return id
}
