package claudecode

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
)

// envelope is one stream-json frame of the Claude Code CLI.
type envelope struct {
	Type            string                `json:"type"`
	Subtype         string                `json:"subtype,omitempty"`
	SessionID       string                `json:"session_id,omitempty"`
	Message         json.RawMessage       `json:"message,omitempty"`
	IsError         bool                  `json:"is_error,omitempty"`
	NumTurns        *int                  `json:"num_turns,omitempty"`
	Result          string                `json:"result,omitempty"`
	LocalCommand    string                `json:"local_command,omitempty"`
	Usage           *usage                `json:"usage,omitempty"`
	ParentToolUseID string                `json:"parent_tool_use_id,omitempty"`
	CompactMetadata *compactMetadata      `json:"compact_metadata,omitempty"`
	Model           string                `json:"model,omitempty"`
	ModelUsage      map[string]modelUsage `json:"modelUsage,omitempty"`
	Tools           *[]string             `json:"tools,omitempty"`
	FilePath        string                `json:"filePath,omitempty"`
	Entries         []json.RawMessage     `json:"entries,omitempty"`
	RateLimitInfo   *rateLimitInfo        `json:"rate_limit_info,omitempty"`
	StopReason      string                `json:"stop_reason,omitempty"`
	RequestID       string                `json:"request_id,omitempty"`
	Request         *controlRequest       `json:"request,omitempty"`
}

type controlRequest struct {
	Subtype   string `json:"subtype"`
	ToolUseID string `json:"tool_use_id"`
}

// rateLimitInfo is the subscription limit signal of a rate_limit_event frame.
type rateLimitInfo struct {
	OverageStatus   string                     `json:"overageStatus,omitempty"`
	OverageResetsAt int64                      `json:"overageResetsAt,omitempty"`
	IsUsingOverage  bool                       `json:"isUsingOverage,omitempty"`
	UnifiedWindows  map[string]rateLimitWindow `json:"unifiedWindows,omitempty"`
}

// rateLimitWindow is one window of a rate_limit_event, as a share of 0 to 1.
type rateLimitWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

// windowLabels name the windows that the CLI sends. Another key is its own label.
var windowLabels = map[string]string{"five_hour": "5-hour", "seven_day": "Weekly"}

// subscription maps the event to a snapshot of the claude lane. The event
// has no plan, and the actor stamps the capture time. Windows are sorted by
// key, so a snapshot is the same from one turn to the next.
func (i *rateLimitInfo) subscription() *eventlog.SubscriptionUsage {
	if i == nil {
		return nil
	}
	u := &eventlog.SubscriptionUsage{Provider: "claude", Windows: []eventlog.SubscriptionUsageWindow{}}
	for _, k := range slices.Sorted(maps.Keys(i.UnifiedWindows)) {
		w := i.UnifiedWindows[k]
		u.Windows = append(u.Windows, eventlog.SubscriptionUsageWindow{Key: k, Label: cmp.Or(windowLabels[k], k), UsedPercent: w.Utilization * 100, ResetsAt: w.ResetsAt})
	}
	if i.IsUsingOverage || i.OverageStatus != "" {
		u.Overage = &eventlog.SubscriptionOverage{InUse: i.IsUsingOverage, Status: i.OverageStatus, ResetsAt: i.OverageResetsAt}
	}
	return u
}

type modelUsage struct {
	ContextWindow int64 `json:"contextWindow,omitempty"`
}

type compactMetadata struct {
	Trigger   string `json:"trigger,omitempty"`
	PreTokens int64  `json:"pre_tokens,omitempty"`
}

// usage is the usage of a result frame (the turn) or of an assistant frame
// (one API call).
type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (u *usage) usage() eventlog.Usage {
	if u == nil {
		return eventlog.Usage{}
	}
	return eventlog.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens}
}

// prompt is the size of the prompt that one API call sent.
func (u *usage) prompt() int64 {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// wireMessage is the message of an assistant or user frame. Content is a string
// or an array of blocks.
type wireMessage struct {
	ID      string          `json:"id,omitempty"`
	Content json.RawMessage `json:"content"`
	Usage   *usage          `json:"usage,omitempty"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

// reasoningFamily tags the signature of a thinking block, as the anthropic
// provider names its family.
const reasoningFamily = "anthropic"

func decodeMessage(raw json.RawMessage) wireMessage {
	var m wireMessage
	_ = json.Unmarshal(raw, &m)
	return m
}

func decodeBlocks(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && text != "" {
		return []block{{Type: "text", Text: text}}
	}
	return nil
}

// contentText flattens the content of a tool_result block. An unknown shape
// is kept as its raw JSON.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	if blocks := decodeBlocks(raw); blocks != nil {
		var texts []string
		for _, b := range blocks {
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

// assistantParts maps the blocks of an assistant message to parts in order.
func assistantParts(m wireMessage) []eventlog.Part {
	var parts []eventlog.Part
	for _, b := range decodeBlocks(m.Content) {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, eventlog.Part{Type: eventlog.PartText, Text: b.Text})
			}
		case "thinking":
			data, _ := json.Marshal(struct {
				Signature string `json:"signature,omitempty"`
			}{b.Signature})
			parts = append(parts, eventlog.Part{Type: eventlog.PartReasoning, Text: b.Thinking,
				ProviderData: map[string]json.RawMessage{reasoningFamily: data}})
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			parts = append(parts, eventlog.Part{Type: eventlog.PartToolCall, CallID: b.ID, Name: b.Name, Arguments: args})
		}
	}
	return parts
}

// toolResults maps the tool_result blocks of a user message to parts. A
// user frame with no tool_result block has none.
func toolResults(m wireMessage, names map[string]string) []eventlog.Part {
	var parts []eventlog.Part
	for _, b := range decodeBlocks(m.Content) {
		if b.Type == "tool_result" {
			parts = append(parts, eventlog.Part{Type: eventlog.PartToolResult, CallID: b.ToolUseID,
				Name: names[b.ToolUseID], Text: contentText(b.Content), IsError: b.IsError})
		}
	}
	return parts
}

// retryable reports whether a failed result is transient. A credential
// failure is refused, not busy, so it is not.
func retryable(subtype, result string) bool {
	hay := strings.ToLower(subtype + " " + result)
	switch {
	case strings.Contains(hay, "credential resolution failed"):
		return false
	case strings.Contains(hay, "rate_limit"), strings.Contains(hay, "rate limit"), strings.Contains(hay, "overloaded"):
		return true
	}
	return subtype == "error_during_execution"
}

type input struct {
	Type    string       `json:"type"`
	Message inputMessage `json:"message"`
}

type inputMessage struct {
	Role string `json:"role"`
	// Content is the text of a message with no attachment, else its content
	// blocks: the text, then one image or document block for each attachment.
	Content any `json:"content"`
}

type contentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *blockSource `json:"source,omitempty"`
}

// blockSource is the inline base64 payload of an attachment block.
type blockSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

// userLine is the stdin line of one user message. read returns the bytes of
// an attachment.
func userLine(m eventlog.Message, read func(key string) ([]byte, error)) (input, error) {
	var texts []string
	var attachments []contentBlock
	for _, p := range m.Parts {
		switch p.Type {
		case eventlog.PartText:
			texts = append(texts, p.Text)
		case eventlog.PartBlob:
			data, err := read(p.BlobKey)
			if err != nil {
				return input{}, fmt.Errorf("claudecode: attachment %s: %w", p.BlobKey, err)
			}
			kind := "document"
			if strings.HasPrefix(p.MediaType, "image/") {
				kind = "image"
			}
			attachments = append(attachments, contentBlock{Type: kind, Source: &blockSource{Type: "base64", MediaType: p.MediaType, Data: data}})
		}
	}
	text := strings.Join(texts, "\n\n")
	var content any = text
	switch {
	case len(attachments) > 0 && text == "":
		content = attachments
	case len(attachments) > 0:
		content = append([]contentBlock{{Type: "text", Text: text}}, attachments...)
	}
	return input{Type: "user", Message: inputMessage{Role: "user", Content: content}}, nil
}
