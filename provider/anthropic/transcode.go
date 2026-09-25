package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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
	// Type names a SERVER-side tool (e.g. the tool search tool); it is
	// omitted for an ordinary client tool, which the API identifies by
	// name plus schema. A server tool entry carries Type and Name only --
	// Description and InputSchema stay empty, which is why both are
	// omitempty here.
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// DeferLoading keeps this tool's definition out of the model's context
	// until it discovers the tool through tool search. The definition is
	// still sent: the API needs it server-side to run the search and to
	// expand the tool_reference block it returns.
	//
	// Never set on the tool search tool itself, and never on every tool --
	// the API rejects a request whose tools are all deferred ("At least one
	// tool must have defer_loading=false").
	DeferLoading bool `json:"defer_loading,omitempty"`
}

// Tool search tool identifiers, from
// platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool.
//
// harness sends the BM25 variant. Both variants search the same fields
// (tool names, descriptions, argument names, argument descriptions) and
// ship on the same models, so the choice is only about how the model
// expresses a query: regex makes it construct a pattern, BM25 lets it write
// a natural-language query. Two reasons BM25 wins here. A malformed pattern
// is a real failure mode the regex variant owns -- the doc's own
// invalid_tool_input example is "missing ) at position 1", and a pattern is
// also capped at 200 characters -- while a natural-language query cannot be
// syntactically invalid. And harness's own client-side search, which stays
// the mechanism on every other provider, already ranks a natural-language
// query over exactly those fields (see engine/mcp_search.go), so BM25 keeps
// one mental model for the same task across routes rather than making the
// query language depend on which provider a session happens to run.
const (
	toolSearchToolType = "tool_search_tool_bm25_20251119"
	toolSearchToolName = "tool_search_tool_bm25"
)

// apiCacheControl marks a prompt-cache breakpoint. TTL is the cache lifetime:
// empty means the API default (5 minutes) and omits the field; "1h" selects the
// extended TTL, which requires the extendedCacheTTLBeta header on the request.
type apiCacheControl struct {
	Type string `json:"type"`          // ephemeral
	TTL  string `json:"ttl,omitempty"` // "1h" — empty means the 5m default
}

// anthropicReasoningData is the shape stored under ProviderData[Family] on
// Reasoning parts: the signature for thinking blocks, or the opaque payload
// for redacted_thinking blocks.
type anthropicReasoningData struct {
	Signature string `json:"signature,omitempty"`
	Redacted  string `json:"redacted,omitempty"`
}

// Cache TTL values for Client.CacheTTL. CacheTTL5m is the Anthropic API
// default; CacheTTL1h is the extended TTL (beta extendedCacheTTLBeta).
const (
	CacheTTL5m = "5m"
	CacheTTL1h = "1h"
)

// CacheTTLValues returns every non-empty value resolveCacheTTL accepts, in a
// fresh slice. It is the adapter's own accepted set, not a copy maintained
// beside it: resolveCacheTTL iterates this same list, so a TTL added here is
// accepted, and one added anywhere else is unreachable. cmd/harness's parity
// test compares this set against config's for equality — the seam that turns
// one-sided drift into a test failure instead of a load-time/first-Stream
// split.
func CacheTTLValues() []string {
	return []string{CacheTTL5m, CacheTTL1h}
}

// DefaultCacheTTL is what an empty Client.CacheTTL resolves to.
//
// The default is the EXTENDED 1-hour TTL, not the API's own 5-minute default,
// because harness sessions are agentic: a single tool call (a long build, a
// live probe, a subagent) routinely runs longer than 5 minutes, and a user
// reads the answer before the next turn. Cache READS price the same at both
// TTLs. A 1h WRITE costs 2x base input instead of 1.25x, and only on the
// incremental tokens each turn adds. A 5m expiry on a mature session
// rewrites the WHOLE prefix — the entire history, at full input price. One
// such miss costs more than the 1h write premium over hundreds of turns.
const DefaultCacheTTL = CacheTTL1h

// ResolveCacheTTL reports whether ttl is a value this adapter accepts for
// Client.CacheTTL, returning the resolved wire TTL. It is the exported seam
// cmd/harness's parity test uses to prove this adapter's accepted list still
// agrees with package config's duplicated copy. Stream calls the unexported
// resolveCacheTTL directly.
func ResolveCacheTTL(ttl string) (string, error) { return resolveCacheTTL(ttl) }

// resolveCacheTTL maps a configured Client.CacheTTL to a wire TTL. It fails on
// an unknown value instead of falling back: a typo must never silently ship
// different cache economics than the operator asked for.
func resolveCacheTTL(v string) (string, error) {
	if v == "" {
		return DefaultCacheTTL, nil
	}
	if slices.Contains(CacheTTLValues(), v) {
		return v, nil
	}
	return "", fmt.Errorf("anthropic: invalid cache_ttl %q (want %q or %q)", v, CacheTTL5m, CacheTTL1h)
}

// cacheControl builds the breakpoint marker for a resolved TTL. The 5m TTL is
// the API default, so it omits the ttl field and keeps the request byte-
// identical to a build that had no TTL support at all.
func cacheControl(ttl string) *apiCacheControl {
	if ttl == CacheTTL1h {
		return &apiCacheControl{Type: "ephemeral", TTL: CacheTTL1h}
	}
	return &apiCacheControl{Type: "ephemeral"}
}

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
// Cache markers are injected here — on the last system block, the ambient
// boundary (see markAmbientBoundary), and the last content block of the
// final message — and never stored in the session log.
// ttl is the resolved cache lifetime (see resolveCacheTTL); the caller sends
// the extendedCacheTTLBeta header when it is CacheTTL1h.
func transcodeRequest(req *provider.Request, ttl string) (*apiRequest, error) {
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
	// KNOWN LIMITATION (live-gated), the ENABLE direction: turning thinking ON
	// does not synthesize a leading thinking block for a prior assistant turn
	// that made tool calls without one. The API can reject a request that
	// continues such a turn ("thinking blocks expected before tool_use"). (The
	// OFF direction — a stored thinking block replayed with thinking disabled —
	// IS fixed, by the strip in transcodeParts below.)
	//
	// The general trigger is any mid-turn enable, not only abort. runAgenticLoop
	// rebuilds the request with Effort: s.Effort() FRESH on every tool round
	// (engine.go), so a plain off->high via POST /session/{id}/thinking while a
	// turn is mid-tool-call makes the very NEXT round enable thinking over a
	// tool_use this turn already emitted without one — the identical
	// thinking-less shape, no abort needed. POST /session/{id}/abort mid-tool-
	// call is the second trigger (a partial tool_use plus a synthetic tool-
	// result, no thinking block, continued by a later effort-enabled prompt).
	// Between turns the risk is absent: the agentic loop finishes a tool cycle
	// within one turn, so a toggle between turns faces a final assistant TEXT
	// turn. STATUS: a live enable-mid-tool-round probe
	// (server/thinking_realmodel_live_test.go, TestEnableMidToolRoundLive) was
	// TOLERATED by the API on 2026-08-11 — the turn completed, no reject — so
	// this limitation is theoretical, not confirmed. No fix is warranted unless
	// that probe later reproduces a reject; the live suite keeps it as the guard.
	// (The OFF direction, by contrast, DID wedge and is fixed below.)
	budget, thinkingEnabled := thinkingBudget(req.Effort)
	if thinkingEnabled {
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
		out.System[n-1].CacheControl = cacheControl(ttl)
	}

	// Tool array. A deferred tool is sent in full, exactly like any other
	// -- defer_loading controls what enters the model's CONTEXT, not what
	// the request carries -- and its presence is what makes the tool search
	// tool useful, so the search tool is prepended whenever anything is
	// deferred.
	//
	// The search tool goes FIRST, and that position is a prompt-cache
	// decision as much as a readability one: it is a fixed two-field entry,
	// so the array's leading bytes stay identical across requests, and the
	// caller's own group order (built-ins, then MCP, then plugins -- see
	// engine.Session.toolDefs) is preserved behind it.
	var deferred int
	for _, t := range req.Tools {
		if t.DeferLoading {
			deferred++
		}
	}
	if deferred > 0 && deferred < len(req.Tools) {
		// The guard is deferred < len(req.Tools), not deferred > 0 alone:
		// the API rejects a request whose tools are ALL deferred, and the
		// search tool itself does not count as the non-deferred one for
		// that rule. A caller that defers everything gets its tools sent
		// eagerly rather than a 400 -- degrading to today's behaviour beats
		// failing the turn.
		out.Tools = append(out.Tools, apiToolDef{Type: toolSearchToolType, Name: toolSearchToolName})
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, apiToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			// Only marked when the search tool went out with it: a deferred
			// tool with no way to be discovered is an unreachable tool.
			DeferLoading: t.DeferLoading && deferred < len(req.Tools),
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
		blocks, err := transcodeParts(m.Parts, thinkingEnabled, true)
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
	last.Content[len(last.Content)-1].CacheControl = cacheControl(ttl)
	markAmbientBoundary(out, ttl)

	return out, nil
}

// markAmbientBoundary adds the CROSS-TURN breakpoint: one cache_control on
// the last block strictly before the FIRST ambient block in the request.
//
// THE PROBLEM it closes. withAmbientStatus (engine/process.go) appends an
// *EngineContext part to the newest user message on the throwaway request
// copy only — durable history never sees it — so the message that carried
// the block on turn N is re-rendered WITHOUT it on turn N+1. A cache entry
// is readable only when the bytes it was written from are a prefix of the
// new request, so the tail breakpoint above, which sits AFTER the ambient
// block, wrote an entry turn N+1 can never read: every user turn re-read
// the whole conversation at the cache-WRITE price. identityStatusSegment is
// present on every request once version and start time are configured
// (engine/identity_status.go), so in `serve` this was the normal path, not
// an edge case.
//
// The boundary block is the last one whose bytes are stable across the turn
// boundary, which makes the entry written here exactly the prefix turn N+1
// re-sends unchanged. The tail breakpoint stays: it is what lets the steps
// WITHIN one turn read each other, and a second entry costs nothing, since
// a write bills only the delta past the highest hit.
//
// An ambient block is identified by the engine-context sentinel, which
// message.NeutralizeEngineContextSentinel guarantees only a genuine
// *EngineContext part can emit (see message/engine_context.go). A request
// with no ambient block needs no boundary — the tail entry is already
// reusable — and one whose FIRST block is ambient has nothing before it to
// mark.
func markAmbientBoundary(out *apiRequest, ttl string) {
	var prev *apiBlock
	for i := range out.Messages {
		for j := range out.Messages[i].Content {
			b := &out.Messages[i].Content[j]
			if b.Type == "text" && strings.HasPrefix(b.Text, message.EngineContextOpenTag) {
				if prev != nil {
					prev.CacheControl = cacheControl(ttl)
				}
				return
			}
			prev = b
		}
	}
}

// transcodeParts renders parts to wire blocks. thinkingEnabled reports whether
// THIS request enables extended thinking; when false, stored Reasoning parts
// are stripped (see the *message.Reasoning case below). topLevel is true for a
// message's own parts and false when recursing into a ToolResult's content
// (see the ToolResult case below). The topLevel distinction matters ONLY for
// *message.EngineContext: the trusted engine-context sentinel is keyed on wire
// POSITION, not part type alone, so a genuine top-level ambient block is
// sentinel-wrapped while an EngineContext reached through tool-result recursion
// is rendered inert. Every other part type ignores topLevel.
func transcodeParts(parts message.Parts, thinkingEnabled, topLevel bool) ([]apiBlock, error) {
	var blocks []apiBlock
	for _, p := range parts {
		switch v := p.(type) {
		case *message.Text:
			if v.Text == "" {
				continue
			}
			// NeutralizeEngineContextSentinel: a user- or paste-authored Text
			// part must never be able to forge the engine-context sentinel on
			// the wire (see message.EngineContext). Only the *message.Engine-
			// Context case below emits it.
			blocks = append(blocks, apiBlock{Type: "text", Text: message.NeutralizeEngineContextSentinel(v.Text)})

		case *message.EngineContext:
			if v.Text == "" {
				continue
			}
			if !topLevel {
				// Reached through tool-result recursion (see the ToolResult
				// case): NOT a genuine top-level ambient position, so it must
				// never emit the trusted sentinel. A tool that could get an
				// EngineContext-shaped part into its result would otherwise
				// inherit the trusted wrapping — the exact forge this change
				// closes. Render it inert (neutralized text) instead. No
				// current path places an EngineContext in a tool result; this
				// is defense against a plugin-built one.
				blocks = append(blocks, apiBlock{Type: "text", Text: message.NeutralizeEngineContextSentinel(v.Text)})
				continue
			}
			// A genuine top-level engine block: emit it sentinel-wrapped so
			// the model can trust it as engine context, exactly as the base
			// system prompt (cmd/harness) describes. This is an ordinary text
			// block on the wire — no new provider feature.
			blocks = append(blocks, apiBlock{Type: "text", Text: message.RenderEngineContext(v.Text)})

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
			content, err := transcodeParts(v.SafeContent(), thinkingEnabled, false)
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
			if !thinkingEnabled {
				// Thinking is OFF for this request. A stored thinking/
				// redacted_thinking block sent with thinking disabled is
				// rejected by the API ("thinking blocks require thinking to be
				// enabled") — and, once in durable history, it 400s EVERY later
				// turn, a permanent wedge. This is the symmetric opposite of the
				// enable-direction limitation. Strip it here: a transcode-time
				// throwaway request may drop a part destructively (it never
				// touches the durable record), the same license NormalizeForWire
				// relies on. A later turn that re-enables thinking replays the
				// block again from the intact history.
				continue
			}
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
				// Deliberately NOT run through
				// NeutralizeEngineContextSentinel (unlike every Text path):
				// the thinking body is signature-bound (data.Signature is
				// computed by the API over this exact text), so altering a
				// byte makes the API reject the replay. Reasoning is
				// model-authored, not attacker-pasted, so it is not a viable
				// engine-context forge vector; the sentinel-wrapping trust
				// this change adds is for genuine EngineContext parts only.
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
