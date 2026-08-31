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

	"github.com/majorcontext/harness/message"
)

// ToolDef describes a tool offered to the model.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema

	// DeferLoading asks the PROVIDER to keep this tool's schema out of the
	// model's context until the model discovers it, instead of loading it
	// up front. The definition is still sent on every request: deferral
	// controls what enters the context window, not what the wire carries.
	//
	// Only an adapter whose API has a native deferral mechanism acts on
	// this; provider/anthropic emits defer_loading plus a server-side tool
	// search tool (see its transcoder). Every other adapter IGNORES the
	// field, which is the safe default -- a tool with no way to be
	// discovered would otherwise be unreachable -- so the engine sets it
	// only for a route it knows can honor it, and keeps its own
	// client-side deferral everywhere else.
	DeferLoading bool
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
	// Effort is the unified reasoning-effort level (message.Effort). The
	// zero value (EffortUnset) sends no reasoning control. Each adapter maps
	// a non-unset level to its own wire shape; a level the target model
	// rejects surfaces as a provider error, since the adapter cannot know
	// per-model support from the ref alone.
	Effort message.Effort
	// SessionKey is a stable, opaque identifier for the session this
	// request belongs to. An adapter MAY forward it to the provider as a
	// routing or cache-affinity hint. It is not a secret and is not
	// persisted.
	SessionKey string
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
// Usage reports token accounting for one provider call.
//
// CONTRACT: the three input components are DISJOINT — InputTokens is the
// uncached portion only, and the true prompt size is InputTokens +
// CacheReadTokens + CacheWriteTokens. Adapters whose upstream reports a
// cache-inclusive total (OpenAI Responses: input_tokens includes
// input_tokens_details.cached_tokens) must subtract before populating.
// Consumers (auto-compaction thresholds, cost accounting) rely on the sum
// being the prompt size on every provider.
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
	// EventActivity carries no content at all: it reports that a wire
	// event arrived and was handled but produced nothing consumer-visible
	// — a keep-alive ping, a tool call's arguments still streaming
	// (input_json_delta), a message_start. Adapters surface one per such
	// wire event instead of looping silently, so a consumer timing the
	// gaps between Next returns (the engine's idle-stream watchdog)
	// measures real wire activity rather than content cadence — a large
	// tool-argument block can stream for minutes with zero content
	// events. Consumers that only want content simply skip it.
	EventActivity EventType = "activity"
)

// Event is one streaming event from a model call.
type Event struct {
	Type       EventType
	Text       string
	ToolCall   *message.ToolCall
	Message    *message.Message
	StopReason StopReason
	Usage      Usage
	// SubscriptionUsage carries a subscription lane's captured rate-limit/
	// quota snapshot (see message.SubscriptionUsage's own doc comment),
	// set only on EventDone by an adapter that captured one on THIS call —
	// nil for every other event, and nil on EventDone itself unless the
	// adapter is a subscription lane that found the signal on this
	// response (provider/openai's codex family reads it from x-codex-*
	// response headers; see engine.streamTurn's EventDone case for where
	// this rides onto Session.SubscriptionUsage).
	SubscriptionUsage *message.SubscriptionUsage
}

// Stream yields events for one model call. Next returns io.EOF after the
// EventDone event has been consumed.
//
// That io.EOF MUST be the literal, unwrapped sentinel value on clean
// termination — never wrapped or replaced by an adapter-specific error type.
// Two engine consumers (engine/goal.go's evaluateGoal loop and
// engine/compact.go's compaction summarizer) tell a clean end apart from a
// truncated-stream failure via identity comparison (err == io.EOF),
// deliberately not errors.Is: a stream cut before its terminal event is
// wrapped by provider.MarkStreamTruncated (see provider/retryable.go) around
// the underlying transport error — typically io.EOF itself, when the
// connection simply closes early — so errors.Is(err, io.EOF) would report
// true for BOTH a clean end and a truncation once that wrapping is in play,
// and a consumer relying on it would silently fold an unfinished stream into
// a "done" result. An adapter returning anything other than the literal
// io.EOF on clean termination is a contract violation.
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
