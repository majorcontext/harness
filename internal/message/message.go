// Package message defines canonical session messages.
//
// Provider adapters transcode them for each request. Provider data replays only with its tagged provider family.

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
	// Origin marks who or what produced this message beyond Role. Empty
	// means an ordinary source. It is presentation metadata: no transcoder
	// reads it and it never reaches a provider.
	Origin string `json:"origin,omitempty"`
	// ParentToolUseID identifies the tool_use call that spawned the subagent
	// turn which produced this message. Empty for a top-level message.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
}

// Normalize scrubs known encoding/json footguns from m's parts in place. It
// is the ingest-time counterpart to the marshal-time guards on ToolCall
// (safeArguments/MarshalJSON) and ProviderData (Get/MarshalJSON): those
// guards make every marshal of a poisoned value safe, but a
// present-but-zero-length ProviderData entry left sitting in the Go value
// itself still causes an in-memory Message to remarshal differently than
// the same message reloaded from its own persisted JSON. That is because
// Reasoning.ProviderData's field tag is "provider_data,omitempty":
// encoding/json's omitempty decides purely from the map's own length,
// before MarshalJSON ever runs, so a map holding one zero-length entry
// (len == 1) is "non-empty" and the field is emitted (as "{}", after
// MarshalJSON drops the useless entry) — while the same map reloaded from
// that exact "{}" comes back as a zero-length map (len == 0) and
// omitempty correctly drops the field entirely on the next marshal. Both
// shapes are safe (neither panics, neither carries real data — see
// ProviderData.Get) but they are not byte-identical, which breaks the
// "retranscoding an unchanged history produces identical wire requests"
// invariant ProviderCallID's doc comment promises elsewhere in this
// package. Normalize closes that gap by deleting zero-length entries
// in place, so a Message's in-memory shape always matches what
// LoadSession would hand back for it.
//
// # A salvaged tool call must never carry invalid Arguments
//
// A worker turn can die at its start with "json: error calling MarshalJSON
// for type json.RawMessage: unexpected end of JSON input", and
// GET /session/{id}/message on that session then 500s with the
// message.Parts wrapper of the same error, while the on-disk log stays
// clean (the poisoned message fails to persist and is never journaled).
// The len(Arguments) == 0 guard safeArguments already carries does not
// catch it: a provider stream that dies mid tool_use block — a connection
// drop during input_json_delta
// accumulation, or, as provider/anthropic/anthropic.go's protocol shows, a
// max_tokens cutoff mid tool-call, which the API still closes out with a
// normal content_block_stop/message_delta/message_stop sequence rather than
// an error — can leave ToolCall.Arguments holding non-empty but
// syntactically invalid (truncated) JSON. That value is neither absent nor
// usable, and json.RawMessage.MarshalJSON does not validate its bytes, so
// it sails through every len==0 check and only fails once embedded in a
// larger document forces encoding/json to compact (and so validate) it.
//
// Normalize is the single place a salvaged, truncated tool call enters
// history, so it is the single place this is fixed: an Arguments value that
// is non-empty but not valid JSON is replaced with nil, the same "no usable
// arguments" value Normalize already treats a zero-length ProviderData entry
// as equivalent to. Only Arguments is cleared, never the whole ToolCall —
// CallID and Name are plain provider-set strings, never json.RawMessage, so
// they carry no marshal risk and are worth keeping: knowing which tool the
// model was in the middle of calling remains useful even once its arguments
// are unrecoverable. safeArguments (below) already coerces a nil/empty
// Arguments to "{}" at marshal time, and every transcoder already does the
// same on the wire, so nil here introduces no new shape for a downstream
// consumer to learn.
//
// Every message passes through this call before it enters a session's
// history — user, assistant, and tool messages alike, regardless of source
// (a shipped provider adapter, a plugin's generate call, or a test's
// scripted provider) — which makes it the one ingest choke point.
//
// # A ProviderData entry has the exact same invalid-but-non-empty footgun
//
// The reasoning above ("A salvaged tool call must never carry invalid
// Arguments") fixed ToolCall.Arguments for a non-empty-but-syntactically-
// invalid value by clearing it here AND, as defense in depth, by having
// safeArguments itself refuse to marshal one. ProviderData.MarshalJSON
// already had the defense-in-depth half for its own zero-length footgun
// but, discovered by this package's own
// round-trip property test (message/properties_test.go,
// TestNormalizeIdempotent), never got the "non-empty but invalid" half
// either guard applies to: a Reasoning.ProviderData entry holding
// non-empty, non-JSON bytes — the same shape a dropped connection or
// malformed hand-rolled producer can leave behind — sailed through both
// Normalize's old len==0-only check and MarshalJSON's matching check, and
// only failed once nested inside a larger document forced encoding/json to
// validate it, reproducing the exact "json: error calling MarshalJSON for
// type json.RawMessage: ..." failure the ToolCall.Arguments guard above
// exists to prevent. Both guards below now check json.Valid, exactly
// mirroring the ToolCall.Arguments fix.
//
// # An empty ToolResult.Content is the same footgun, in reverse
//
// See SafeContent's doc comment for the full mechanism. A ToolResult
// with empty Content transcodes to a tool_result
// block every provider adapter in this package either rejects or drops.
// Content counts as empty when it is nil, or when it carries only a blank
// Text part — the exact shape bash.go leaves behind for a command with no
// output. Either shape wedges a session with no crash at all.
//
// The case below is this function's primary fix. It replaces an empty
// Content with NoToolOutputText in place. Every LIVE message passes
// through Normalize on append, and a replay of a session log calls
// Normalize on every message it reads, so a poisoned message is already
// repaired by the time anything downstream sees it. SafeContent's own check is the marshal/transcode-time backstop
// for a producer that bypasses Normalize entirely.
func (m *Message) Normalize() {
	for _, p := range m.Parts {
		switch v := p.(type) {
		case *Reasoning:
			for family, raw := range v.ProviderData {
				if len(raw) == 0 || !json.Valid(raw) {
					delete(v.ProviderData, family)
				}
			}
		case *ToolCall:
			if len(v.Arguments) > 0 && !json.Valid(v.Arguments) {
				v.Arguments = nil
			}
		case *ToolResult:
			if v.isEmpty() {
				v.Content = Parts{&Text{Text: NoToolOutputText}}
			}
		}
	}
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

// safeArguments normalizes Arguments for marshaling. This guards a genuine
// encoding/json footgun: json.RawMessage.MarshalJSON does not validate its
// bytes — a nil RawMessage is special-cased to marshal as "null", but any
// other empty (zero-length, non-nil) RawMessage is handed to the encoder
// as-is and fails with "json: error calling MarshalJSON for type
// json.RawMessage: unexpected end of JSON input" (zero bytes is not valid
// JSON). `omitempty` does not help either: it is defined in terms of the Go
// zero value (nil), not "len == 0", so an empty-but-non-nil RawMessage is
// never omitted. Every code path that marshals a ToolCall — directly (a
// plain struct field, e.g. an event's ToolCall pointer) or as a Parts
// element (marshalPart below) — must call this instead of encoding
// Arguments directly.
//
// Empty Arguments normalize to "{}", not "null": every transcoder treats a
// zero-length Arguments as "no arguments" and coerces it to an empty JSON
// object on the wire (see provider/anthropic/transcode.go and
// provider/openai/transcode.go, both of which substitute "{}" for a
// zero-length Arguments before sending to the provider). Normalizing to
// "null" here instead would diverge from that convention: a resumed session
// round-tripped through canonical JSON would carry Arguments: null, which is
// not a valid tool-call arguments object and does not match what was
// actually sent on the wire.
//
// A non-empty but syntactically invalid Arguments — the truncated-JSON
// shape a stream that dies mid tool_use block can leave behind (see
// Message.Normalize's doc comment for the full mechanism) — is normalized
// the same way as empty: json.RawMessage.MarshalJSON does not validate its
// bytes either, so an invalid value "succeeds" in isolation and only fails
// once nested inside a larger document that encoding/json must compact to
// validate. Normalize is the primary fix (it sanitizes at the one ingest
// choke point every message passes through, replacing invalid Arguments
// with nil so this branch never even fires for a message that went through
// it), but safeArguments checks json.Valid here too as defense in depth: a
// producer that bypasses Normalize entirely — a plugin's chat.message hook
// building a Message by hand, a hand-rolled provider adapter, a test's
// scripted provider — must still never be able to make a marshal fail.
func (tc ToolCall) safeArguments() json.RawMessage {
	if len(tc.Arguments) == 0 || !json.Valid(tc.Arguments) {
		return json.RawMessage("{}")
	}
	return tc.Arguments
}

// MarshalJSON implements json.Marshaler so any direct encoding of a ToolCall
// (or *ToolCall) — e.g. an Event's ToolCall field elsewhere in this
// module's consumers — goes through safeArguments automatically. It must
// NOT be relied on from marshalPart's tagged-union wrapper below: embedding
// *ToolCall anonymously in another struct promotes this method onto the
// wrapper, which would marshal using ToolCall's fields alone and silently
// drop the wrapper's own "type" discriminator. marshalPart therefore
// reconstructs ToolCall's fields explicitly instead of embedding.
func (tc ToolCall) MarshalJSON() ([]byte, error) {
	type alias ToolCall
	a := alias(tc)
	a.Arguments = tc.safeArguments()
	return json.Marshal(a)
}

// ToolResult is the outcome of a ToolCall. Content may hold Text and Blob
// parts only.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content Parts  `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

func (*ToolResult) partType() PartType { return PartToolResult }

// NoToolOutputText is the Content text substituted, via SafeContent below
// and Message.Normalize, for a ToolResult whose real Content is empty in
// every sense that matters — see SafeContent's doc comment for the full
// mechanism. A marker string, rather than an empty Text part, is chosen
// deliberately: an agent (or an operator) reading its own transcript
// benefits from seeing "(no output)" in place of a blank line, the same
// way a shell prompt distinguishes "ran, produced nothing" from "never
// ran".
const NoToolOutputText = "(no output)"

// isEmpty reports whether tr's Content carries nothing a reader (model,
// transcript, or wire protocol) would recognize as actual output: no parts
// at all, or parts that are exclusively blank Text (the exact shape
// bash.go's captured-output path leaves behind for a command with no
// stdout/stderr, e.g. a grep that matches nothing). Any other part type —
// Blob, a Text with real content — counts as content and is left alone.
func (tr ToolResult) isEmpty() bool {
	for _, p := range tr.Content {
		t, ok := p.(*Text)
		if !ok || t.Text != "" {
			return false
		}
	}
	return true
}

// SafeContent normalizes Content for marshaling and transcoding, mirroring
// ToolCall.safeArguments's role for Arguments.
//
// # A null/absent tool_result content wedges a session with no crash at
// all
//
// This is a distinct root cause from the stop-reason orphan (see
// engine.unexecutedToolCallStopReasonTextFmt's doc comment): a request can
// be internally balanced — every tool_use paired with a tool_result,
// every pair adjacent — and still 400 with the identical "tool_use ids
// were found without tool_result blocks immediately after" whenever one of
// those tool_result blocks carries null or absent content. A
// `grep ... | head -20` that matches nothing is enough: empty stdout makes
// bash.go's captured-output path return a ToolResult whose Content is a
// single blank Text part.
//
// A minimal 3-message reproduction isolates the exact shape. Two wire
// shapes trigger the rejection: an explicit null,
// and an omitted content field. The gateway ACCEPTS an empty array, an
// empty string, and a single blank text block — only the absent forms
// fail. That distinction matters here, because omitempty on
// provider/anthropic/transcode.go's apiBlock.Content turns an empty array
// into an omitted field on the wire, which is how a blank tool result
// reached the failing shape.
//
// Unlike the stop-reason orphan, this shape needs no crash, no stream
// truncation, and no sandbox death. An ordinary, successful tool call with
// empty output is enough.
//
// # Two enforcement points, not one
//
// The primary fix is a canonical-layer guarantee: Message.Normalize applies
// it in place at the one ingest choke point every LIVE-appended message
// passes through, and a replay of a session log applies the same Normalize
// call to every message it reads, so a resumed session repairs an old,
// unpatched empty ToolResult exactly like a live one. SafeContent is the second enforcement point: every transcoder
// (anthropic, openaicompat, openai) calls it directly when building a
// tool_result wire block, rather than reading Content unchecked. This is
// deliberate belt-and-suspenders, not redundancy — Normalize cannot reach a
// ToolResult built by a producer that bypasses it entirely: a plugin's
// chat.message hook, a hand-rolled provider adapter, or a test's scripted
// provider. SafeContent is also what ToolResult.MarshalJSON calls, so any
// direct JSON encoding of a ToolResult gets the same guarantee for free.
func (tr ToolResult) SafeContent() Parts {
	if tr.isEmpty() {
		return Parts{&Text{Text: NoToolOutputText}}
	}
	return tr.Content
}

// MarshalJSON implements json.Marshaler so any direct encoding of a
// ToolResult (or *ToolResult) goes through SafeContent automatically,
// exactly mirroring ToolCall.MarshalJSON's role for Arguments. It must NOT
// be relied on from marshalPart's tagged-union wrapper below, for the same
// reason ToolCall.MarshalJSON's own doc comment gives: embedding a type
// that implements json.Marshaler promotes the method onto the wrapper,
// silently dropping the "type" discriminator.
func (tr ToolResult) MarshalJSON() ([]byte, error) {
	type alias ToolResult
	a := alias(tr)
	a.Content = tr.SafeContent()
	return json.Marshal(a)
}

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
//
// # Unbounded replay is a request-size/time bomb
//
// A thinking-block signature or a redacted_thinking payload (see
// provider/anthropic/transcode.go's anthropicReasoningData) is opaque to
// this package and, in the ordinary case, small — a few hundred bytes. It
// is not, however, bounded by anything: a provider can hand back an entry
// orders of magnitude larger — a single thinking signature has been
// observed at roughly 30KB alongside sibling entries of a few hundred bytes
// in the same run — and every entry that makes it into history is replayed
// VERBATIM on every subsequent request for the rest of the session —
// history only grows, it is never pruned. An oversized entry is therefore
// not a one-time cost: it is carried on every request from the turn it
// appears in onward, compounding with whatever the next turn adds. That is
// a request-size (and, on some providers, request-time) bomb hiding in
// something this package treats as a small opaque blob.
//
// maxProviderDataEntry bounds this the same way a zero-length entry is
// already bounded (both are "Get, below, treats this as absent"): reasoning
// replay is a context-quality optimization, not a correctness requirement
// (a Reasoning part crossing to a different provider family is already
// dropped), so refusing to replay an oversized entry costs a turn's worth
// of thinking continuity/cache affinity and nothing else. The cap is
// generous — 256KiB, several hundred times the entry sizes typically
// observed in practice — specifically so it never fires on a legitimate
// large redacted_thinking payload from a long extended-thinking turn; it
// exists to catch the pathological case, not to budget the common one.
//
// # The map-shaped twin of the ToolCall.Arguments footgun
//
// ToolCall.Arguments is a single json.RawMessage field, and safeArguments
// (above) guards the one encoding/json footgun that matters for it: a
// zero-length but non-nil json.RawMessage fails to marshal with "json:
// error calling MarshalJSON for type json.RawMessage: unexpected end of
// JSON input" (nil is special-cased by the encoder to marshal as "null";
// zero-length-non-nil is not special-cased at all and is handed to the
// encoder as-is). ProviderData is a map of the same underlying type, and it
// has exactly the same failure mode PLUS an extra one: a caller that reads
// an entry straight out of the map (v.ProviderData[Family]) and reuses those
// bytes downstream — as every current transcoder does — bypasses any
// guard defined on the map type itself, because indexing a map is not a
// call to any method. A guard that covers only ToolCall.Arguments does not
// close this: Reasoning.ProviderData carries the exact same json.RawMessage
// under the exact same footgun, one layer of map indirection away, so the
// fix must cover both types, not ToolCall alone.
//
// Get and MarshalJSON below are ProviderData's equivalent of
// ToolCall.safeArguments/MarshalJSON: Get is the single choke point every
// transcoder must use to read an entry (never map indexing directly), so a
// zero-length entry is treated as "absent" at the one place all consumers
// go through, instead of being trusted as real data and carried into a
// provider request or an unmarshal call. MarshalJSON guards the direct-marshal
// path (a Reasoning part marshaled as-is — the session log, the server
// journal, a chat.message plugin hook payload) by dropping zero-length
// entries from the encoded object entirely: they carry no information
// (Get already treats them as absent), so omitting them is lossless and
// keeps every marshal of a ProviderData value — via any encoder, present or
// future — safe without that encoder having to know about this footgun.
type ProviderData map[string]json.RawMessage

// maxProviderDataEntry bounds a single ProviderData entry's replayed size —
// 256KiB is chosen to sit far above any signature or redacted_thinking
// payload size observed in practice while still being a hard, structural
// bound: bytes, not tokens or entries, because the whole point is bounding
// the wire size actually replayed.
const maxProviderDataEntry = 256 * 1024

// Get returns the ProviderData entry for family, treating a present-but
// zero-length entry as absent — the same normalization ToolCall.safeArguments
// applies to Arguments, but at the point of read rather than of marshal,
// since a raw value extracted here commonly gets reused downstream (appended
// into a provider request's own RawMessage list, e.g.) outside of any
// marshaling this map itself might guard. Every transcoder must call this
// instead of indexing the map directly.
//
// An entry larger than maxProviderDataEntry is also treated as absent: see
// "Unbounded replay is a request-size/time bomb" above. This is the single
// choke point every transcoder already goes through for the zero-length
// case, so it is also the single choke point that bounds size — no
// transcoder needs its own cap, and none can accidentally bypass it.
func (pd ProviderData) Get(family string) (json.RawMessage, bool) {
	raw, ok := pd[family]
	if !ok || len(raw) == 0 || len(raw) > maxProviderDataEntry {
		return nil, false
	}
	return raw, true
}

// MarshalJSON implements json.Marshaler so any direct encoding of a
// ProviderData value — e.g. a Reasoning part marshaled as-is by
// marshalPart's embedded-struct case below, in the session log, the server
// journal, or a plugin hook payload — cannot trip over a zero-length (but
// non-nil) entry's own MarshalJSON failure. Entries with zero-length data
// carry no information (Get, above, already treats them as absent) so they
// are dropped from the encoded object rather than encoded as "null":
// omitting an entry and normalizing it to null are equally "absent" to
// every reader in this codebase (both go through Get), and omitting keeps
// the wire shape exactly what it would have been had the entry never been
// set, rather than introducing a new null-valued shape for the format to
// support.
//
// A non-empty but syntactically invalid entry is dropped the same way, for
// the same reason safeArguments (ToolCall's equivalent guard) treats
// invalid Arguments as absent rather than encoding them: json.RawMessage's
// own MarshalJSON does not validate its bytes, so an invalid value
// "succeeds" here in isolation and only fails once nested inside a larger
// document that encoding/json must compact to validate — see Normalize's
// doc comment ("A ProviderData entry has the exact same invalid-but-non-
// empty footgun") for the failure shape this closes.
func (pd ProviderData) MarshalJSON() ([]byte, error) {
	if pd == nil {
		return []byte("null"), nil
	}
	out := make(map[string]json.RawMessage, len(pd))
	for family, raw := range pd {
		if len(raw) == 0 || !json.Valid(raw) {
			continue
		}
		out[family] = raw
	}
	return json.Marshal(out)
}

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
		// Deliberately not embedding *ToolCall here (unlike the other
		// cases below): ToolCall.MarshalJSON must be defined for direct
		// encoding of a bare ToolCall elsewhere, but embedding a type that
		// implements json.Marshaler promotes the method onto this wrapper,
		// which would then marshal using only ToolCall's own fields and
		// silently drop the "type" discriminator. Reconstructing the
		// fields explicitly sidesteps that and applies the same
		// empty-Arguments normalization inline.
		return json.Marshal(struct {
			Type      PartType        `json:"type"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}{PartToolCall, v.CallID, v.Name, v.safeArguments()})
	case *ToolResult:
		// Deliberately not embedding *ToolResult (mirroring the ToolCall
		// case above, for the exact same reason): now that ToolResult
		// defines its own MarshalJSON (see SafeContent's doc comment),
		// embedding it here would promote that method onto this wrapper
		// and silently drop the "type" discriminator. Reconstructing the
		// fields explicitly sidesteps that and applies the same
		// empty-Content normalization inline.
		return json.Marshal(struct {
			Type    PartType `json:"type"`
			CallID  string   `json:"call_id"`
			Content Parts    `json:"content"`
			IsError bool     `json:"is_error,omitempty"`
		}{PartToolResult, v.CallID, v.SafeContent(), v.IsError})
	case *Reasoning:
		return json.Marshal(struct {
			Type PartType `json:"type"`
			*Reasoning
		}{PartReasoning, v})
	case *EngineContext:
		return json.Marshal(struct {
			Type PartType `json:"type"`
			*EngineContext
		}{PartEngineContext, v})
	default:
		return nil, fmt.Errorf("message: cannot marshal part type %T", p)
	}
}

// partVariants pairs each wire discriminator with a zero-value constructor.
var partVariants = []struct {
	partType PartType
	new      func() Part
}{
	{PartText, func() Part { return new(Text) }},
	{PartBlob, func() Part { return new(Blob) }},
	{PartToolCall, func() Part { return new(ToolCall) }},
	{PartToolResult, func() Part { return new(ToolResult) }},
	{PartReasoning, func() Part { return new(Reasoning) }},
	{PartEngineContext, func() Part { return new(EngineContext) }},
}

func unmarshalPart(raw json.RawMessage) (Part, error) {
	var head struct {
		Type PartType `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	for _, v := range partVariants {
		if v.partType != head.Type {
			continue
		}
		p := v.new()
		if err := json.Unmarshal(raw, p); err != nil {
			return nil, err
		}
		return p, nil
	}
	return nil, fmt.Errorf("message: unknown part type %q", head.Type)
}

// SyntheticOrphanResultText is the Content text of a tool_result that
// NormalizeForWire synthesizes for a tool_use/tool_call that has no matching
// result where a provider's wire protocol requires one. The text always says
// "synthesized" so it is visibly distinguishable from a result a tool
// actually produced.
const SyntheticOrphanResultText = "synthesized: no tool_result was found in history for this tool_use; injected to keep the request protocol-valid"

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
