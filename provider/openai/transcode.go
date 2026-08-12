package openai

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
const Family = "openai"

// imageLimits are the image caps imageclamp enforces for OpenAI. OpenAI does
// NOT hard-reject on dimension (it auto-resizes; tile models to ~2048px), so
// the 8000px cap and 2576px target are defensive — they keep a pathological
// screenshot from bloating the request, matching the other adapters, while
// costing nothing in fidelity (OpenAI resizes below 2576 anyway). OpenAI has no
// per-image byte limit (only a 512MB total-request cap) and no >20-image
// stricter rule, so both are disabled.
var imageLimits = imageclamp.Limits{
	MaxDim:    8000,
	TargetDim: 2576,
	// ManyImageThreshold / MaxImageBytes: 0 (not enforced by OpenAI).
	RecurseToolResults: false, // tool-result images are omitted on the wire
}

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
	// Reasoning carries the reasoning-effort control. Nil sends no control,
	// so the model runs its own default.
	Reasoning *apiReasoning `json:"reasoning,omitempty"`
}

// apiReasoning is the OpenAI Responses reasoning control. Effort is one of
// minimal, low, medium, high.
type apiReasoning struct {
	Effort string `json:"effort,omitempty"`
}

// reasoningEffort maps a unified effort level to the OpenAI Responses
// reasoning.effort string. It returns ("", false) for a level that requests no
// reasoning (EffortUnset, EffortOff), so the caller sends no reasoning control
// and the model uses its own default.
func reasoningEffort(e message.Effort) (string, bool) {
	if !e.Reasoning() {
		return "", false
	}
	return string(e), true
}

// reasoningOutputFloor is the minimum max_output_tokens transcodeRequest
// raises to when reasoning is enabled at a given level. Reasoning tokens count
// against max_output_tokens, so a small cap (the 8192 engine default) can be
// consumed entirely by reasoning and leave a truncated or empty answer. The
// floor scales with the level — a low level should not silently triple a
// caller's deliberately-small cap the way a flat floor would — and a caller
// that set a LARGER cap always keeps it (the floor only raises, never lowers).
func reasoningOutputFloor(e message.Effort) int {
	switch e {
	case message.EffortMinimal:
		// Above the 8192 engine default so even minimal reasoning leaves answer
		// headroom (a flat-at-default floor would add none).
		return 10000
	case message.EffortLow:
		return 12000
	case message.EffortMedium:
		return 18000
	default: // high
		return 25000
	}
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
	effort, reasoningEnabled := reasoningEffort(req.Effort)
	// stripReasoning is DELIBERATELY asymmetric with reasoningEnabled and with
	// the anthropic adapter. reasoningEnabled is false for BOTH EffortUnset
	// (the default of every `harness run`/`serve` session — nothing sets
	// Config.Effort) and EffortOff, because neither sends a `reasoning`
	// control. But stored encrypted reasoning items must be STRIPPED only on
	// an EXPLICIT EffortOff (a genuine "reasoning disabled" intent). An unset
	// (default) session MUST replay them, exactly as every pre-effort-control
	// build did: OpenAI reasoning models (gpt-5) reason BY DEFAULT, and the
	// stored items are REQUIRED for stateless (Store:false) multi-turn tool
	// use — a stripped item wedges every turn-2+ tool continuation. So
	// unset != off here. Anthropic's sibling strip can safely key on
	// !Reasoning() because Claude emits no thinking block without the control;
	// an OpenAI default turn always carries one. Do NOT re-fold this back into
	// reasoningEnabled. (Regression: NEP-5272 review of PR #117.)
	stripReasoning := req.Effort == message.EffortOff
	if reasoningEnabled {
		out.Reasoning = &apiReasoning{Effort: effort}
		// Reasoning models reject an explicit temperature or top_p, and reasoning
		// tokens count against max_output_tokens — mirror the anthropic adapter:
		// drop both sampling controls and raise the output cap above a floor.
		out.Temperature = nil
		out.TopP = nil
		if floor := reasoningOutputFloor(req.Effort); out.MaxOutputTokens < floor {
			out.MaxOutputTokens = floor
		}
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, apiToolDef{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}

	// Orphaned-tool_use repair, same as the anthropic and openaicompat
	// transcoders: a ToolCall with no matching ToolResult in the
	// immediately-following wire turn would transcode to a dangling
	// function_call item the API rejects on every retry.
	// message.NormalizeForWire (NEP-5293 part 2) is the transcode-only
	// repair — this call site builds one throwaway request and never
	// touches the durable record, so its destructive/relocating repairs
	// are safe here; see its doc comment for the incident history and the
	// additive (message.ResolveOrphanToolCalls, used only against LIVE
	// history) / transcode-only split. Composed with image clamping
	// exactly as the anthropic transcoder does; imageLimits.RecurseToolResults
	// is false because tool-result images are omitted on the wire (see
	// toolResultOutput), so clamping them would be wasted work.
	messages := imageclamp.Clamp(message.NormalizeForWire(req.Messages), imageLimits)
	for i := range messages {
		m := &messages[i]
		items, err := transcodeMessage(m, stripReasoning)
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
// stripReasoning reports whether stored reasoning items must be dropped (see
// the Reasoning case). It is true ONLY for an explicit EffortOff, never for
// the default EffortUnset — an unset session replays stored reasoning items,
// which gpt-5 stateless multi-turn tool use requires.
func transcodeMessage(m *message.Message, stripReasoning bool) ([]json.RawMessage, error) {
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
			// NeutralizeEngineContextSentinel: a user- or paste-authored Text
			// part must never forge the engine-context sentinel on the wire
			// (see message.EngineContext). Only the *message.EngineContext
			// case below emits it.
			pending.Content = append(pending.Content, apiContentPart{Type: ct, Text: message.NeutralizeEngineContextSentinel(v.Text)})

		case *message.EngineContext:
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
			// A genuine engine block: emit it sentinel-wrapped as the base
			// system prompt (cmd/harness) describes. An ordinary text content
			// part on the wire — no new provider feature.
			pending.Content = append(pending.Content, apiContentPart{Type: ct, Text: message.RenderEngineContext(v.Text)})

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
			if stripReasoning {
				// Reasoning is EXPLICITLY OFF (EffortOff) for this request. A
				// stored reasoning item shipped while the request omits
				// `reasoning` can be rejected, so strip it — symmetric with the
				// anthropic thinking-block strip. This is NOT reached on the
				// default EffortUnset: gpt-5 reasons by default and the stored
				// items are required for stateless multi-turn tool use, so an
				// unset session replays them (see stripReasoning in
				// transcodeRequest). A transcode-time throwaway request may drop
				// a part destructively; a later reasoning-ON turn replays it
				// from the intact history.
				continue
			}
			raw, ok := v.ProviderData.Get(Family)
			if !ok {
				// Another provider's reasoning, or a present-but-empty
				// entry (see message.ProviderData.Get — this is the
				// exact shape that used to reach json.Marshal below as a
				// zero-length, non-nil json.RawMessage and fail with
				// "unexpected end of JSON input"): dropped, per the
				// canonical format's crossing rule.
				continue
			}
			// Replay the stored raw reasoning item verbatim. Deliberately NOT
			// run through NeutralizeEngineContextSentinel (unlike every Text
			// path): this is an opaque, provider-native reasoning item
			// (typically encrypted) whose bytes must round-trip unchanged;
			// rewriting them would corrupt the payload. Reasoning is
			// model-authored, not attacker-pasted, so it is not a viable
			// engine-context forge vector.
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
//
// SafeContent, not Content: this adapter's own wire shape (a plain JSON
// string field, never an omittable array) never reproduced the
// empty-tool_result wedge SafeContent's doc comment describes, but reading
// through SafeContent anyway keeps this adapter covered by the same
// canonical-layer guarantee the others rely on, rather than needing its
// own separate argument for why it happens to be safe.
func toolResultOutput(v *message.ToolResult) string {
	content := v.SafeContent()
	// NeutralizeEngineContextSentinel: tool output (a hostile file read,
	// web fetch, or subprocess stdout) must never forge the engine-context
	// sentinel on the wire (see message.EngineContext). This flattening path
	// bypasses the *message.Text case's neutralization, so it neutralizes
	// here — the same bar user/assistant text already meets.
	//
	// Both *Text and *EngineContext contribute their text: message.Parts.Text
	// (content.Text()) keeps only *Text, so a plugin-built *EngineContext
	// nested in a tool result would vanish silently. A tool result is never a
	// genuine top-level ambient position, so such a block renders INERT
	// (neutralized text) and stays visible — matching provider/anthropic's
	// transcodeParts tool-result recursion (topLevel=false), so all three
	// transcoders agree on identical input. No engine path places an
	// EngineContext in a tool result today.
	var b strings.Builder
	blobs := 0
	for _, p := range content {
		var text string
		switch pt := p.(type) {
		case *message.Text:
			text = pt.Text
		case *message.EngineContext:
			text = pt.Text
		case *message.Blob:
			blobs++
			continue
		default:
			continue
		}
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	out := message.NeutralizeEngineContextSentinel(b.String())
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
