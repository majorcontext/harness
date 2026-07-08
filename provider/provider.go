// Package provider defines the interface between the engine and model APIs.
//
// Each adapter transcodes canonical history (package message) to its wire
// format from scratch on every request — transcoding is stateless, which is
// what makes mid-session model swaps free. Adapters produce the final
// canonical assistant message themselves, since only they know how to fold
// provider-specific state (thinking signatures, encrypted reasoning) into
// ProviderData attachments.
package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/andybons/harness/message"
)

// ToolDef describes a tool offered to the model.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema
}

// Request is one model call. System and Messages are canonical; the adapter
// owns all wire-format concerns, including prompt-cache markers (injected at
// transcode time, never stored).
type Request struct {
	Model       message.ModelRef
	System      []string // system prompt segments, in order
	Messages    []message.Message
	Tools       []ToolDef
	Temperature *float64
	TopP        *float64
	MaxTokens   int
}

// StopReason is why the model stopped generating.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopOther     StopReason = "other"
)

// Usage is token accounting for one request.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// EventType discriminates streaming events.
type EventType string

const (
	// EventTextDelta carries a chunk of assistant text in Text.
	EventTextDelta EventType = "text_delta"
	// EventReasoningDelta carries a chunk of reasoning summary in Text.
	EventReasoningDelta EventType = "reasoning_delta"
	// EventToolCall carries a complete tool call (arguments fully buffered).
	EventToolCall EventType = "tool_call"
	// EventDone carries the fully assembled canonical assistant message,
	// stop reason, and usage. It is always the final event of a stream.
	EventDone EventType = "done"
)

// Event is one streaming event from a model call.
type Event struct {
	Type       EventType
	Text       string
	ToolCall   *message.ToolCall
	Message    *message.Message
	StopReason StopReason
	Usage      Usage
}

// Stream yields events for one model call. Next returns io.EOF after the
// EventDone event has been consumed.
type Stream interface {
	Next() (Event, error)
	Close() error
}

// Provider is one model API family.
type Provider interface {
	// Name is the provider family key: it matches ModelRef.Provider and the
	// ProviderData tag this adapter reads and writes.
	Name() string
	Stream(ctx context.Context, req *Request) (Stream, error)
}

// Registry maps provider family names to adapters.
type Registry map[string]Provider

// For returns the adapter for a model ref.
func (r Registry) For(ref message.ModelRef) (Provider, error) {
	p, ok := r[ref.Provider]
	if !ok {
		return nil, fmt.Errorf("provider: no adapter for %q (model %s)", ref.Provider, ref)
	}
	return p, nil
}
