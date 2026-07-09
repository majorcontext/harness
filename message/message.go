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
// Two production goal sessions, ses_01kx453ewfedqrg7p3c64f8sca and
// ses_01kx453ev9ejattygpf7rbzptw, died at the start of a worker turn with
// "json: error calling MarshalJSON for type json.RawMessage: unexpected end
// of JSON input" — three identical attempts — and GET /session/{id}/message
// on them then 500'd with the message.Parts wrapper of the same error,
// while the on-disk log stayed clean (the poisoned message failed to
// persist and was never journaled). The len(Arguments) == 0 guard
// safeArguments already carries did not catch it: a provider stream that
// dies mid tool_use block — a connection drop during input_json_delta
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
// Session.append (engine/engine.go) calls this on every message before it
// enters a session's history — user, assistant, and tool messages alike,
// regardless of source (a shipped provider adapter, a plugin's generate
// call, or a test's scripted provider) — which is the one ingest choke
// point every message passes through.
func (m *Message) Normalize() {
	for _, p := range m.Parts {
		switch v := p.(type) {
		case *Reasoning:
			for family, raw := range v.ProviderData {
				if len(raw) == 0 {
					delete(v.ProviderData, family)
				}
			}
		case *ToolCall:
			if len(v.Arguments) > 0 && !json.Valid(v.Arguments) {
				v.Arguments = nil
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
// Message.Normalize's doc comment for the full incident,
// ses_01kx453ewfedqrg7p3c64f8sca / ses_01kx453ev9ejattygpf7rbzptw) — is
// normalized the same way as empty: json.RawMessage.MarshalJSON does not
// validate its bytes either, so an invalid value "succeeds" in isolation and
// only fails once nested inside a larger document that encoding/json must
// compact to validate, which is exactly the shape that error took in
// production. Normalize is the primary fix (it sanitizes at the one ingest
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
// is not, however, bounded by anything: a provider is free to hand back an
// entry orders of magnitude larger (a production session,
// ses_01kx3ts0pjfap950bmr9b2js0b.jsonl, carries one thinking signature of
// ~30KB against seven siblings of 350-600 bytes in the same run), and every
// entry that makes it into history is replayed VERBATIM on every
// subsequent request for the rest of the session — history only grows, it
// is never pruned. An oversized entry is therefore not a one-time cost:
// it is carried on every request from the turn it appears in onward,
// compounding with whatever the next turn adds. That is a request-size
// (and, on some providers, request-time) bomb hiding in something this
// package treats as a small opaque blob.
//
// maxProviderDataEntry bounds this the same way a zero-length entry is
// already bounded (both are "Get, below, treats this as absent"): reasoning
// replay is a context-quality optimization, not a correctness requirement
// (see the package doc — a Reasoning part crossing to a different provider
// family is already dropped the same way), so refusing to replay an
// oversized entry costs a turn's worth of thinking continuity/cache
// affinity and nothing else. The cap is generous — 256KiB, several hundred
// times the ordinary entry size seen in production — specifically so it
// never fires on a legitimate large redacted_thinking payload from a long
// extended-thinking turn; it exists to catch the pathological case, not to
// budget the common one.
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
// call to any method. #42 fixed the ToolCall case and, because it only
// looked at ToolCall, missed this one entirely: Reasoning.ProviderData
// carries the exact same json.RawMessage under the exact same footgun, one
// layer of map indirection away, and #42's fix does not reach it — which is
// why the error recurred on a binary that already had #42's fix.
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
// see the package doc above ("Unbounded replay is a request-size/time
// bomb"). 256KiB is chosen to sit far above any signature or
// redacted_thinking payload observed in production while still being a
// hard, structural bound: bytes, not tokens or entries, because the whole
// point is bounding the wire size actually replayed.
const maxProviderDataEntry = 256 * 1024

// Get returns the ProviderData entry for family, treating a present-but
// zero-length entry as absent — the same normalization ToolCall.safeArguments
// applies to Arguments, but at the point of read rather than of marshal,
// since a raw value extracted here commonly gets reused downstream (appended
// into a provider request's own RawMessage list, e.g.) outside of any
// marshaling this map itself might guard. Every transcoder must call this
// instead of indexing the map directly; see the package doc on ProviderData.
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
func (pd ProviderData) MarshalJSON() ([]byte, error) {
	if pd == nil {
		return []byte("null"), nil
	}
	out := make(map[string]json.RawMessage, len(pd))
	for family, raw := range pd {
		if len(raw) == 0 {
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

// SyntheticOrphanResultText is the Content text of a tool_result
// synthesized by ResolveOrphanToolCalls for a tool_use/tool_call that has
// no matching result anywhere a provider's wire protocol requires one.
// Callers (currently every transcoder) must never drop an orphaned
// tool_use silently — the text always says "synthesized" so it is visibly
// distinguishable, in a transcript or a debugging session, from a result
// the model's tool actually produced.
const SyntheticOrphanResultText = "synthesized: no tool_result was found in history for this tool_use; injected to keep the request protocol-valid"

// ResolveOrphanToolCalls returns messages with a synthetic, is_error
// tool_result injected for every ToolCall that has no matching ToolResult
// immediately after it — every provider wire protocol this package
// transcodes to (Anthropic's tool_use/tool_result, the OpenAI-compatible
// chat-completions tool_calls/tool message) requires a tool call to be
// followed immediately by its result, and rejects a request where one is
// missing (Anthropic: HTTP 400 "tool_use ids were found without
// tool_result blocks immediately after").
//
// # Incident ses_01kx48z4rqfkpbwmzfdv1jzeg6
//
// A goal worker turn died with exactly that 400 naming one tool_use id,
// and every subsequent goal-loop retry failed identically, killing the
// goal: once an assistant message carrying a ToolCall part enters a
// session's history without a following tool-role result — the provider
// stream died between emitting the tool_call and the engine executing it,
// or errored mid-turn — every later request replays that same orphaned
// tool_use and is rejected the same way. This is the sibling, at the wire
// protocol level, of the marshal-level poisoning fixed in the commit
// titled "fix(message,engine): truncated ToolCall.Arguments must never
// poison history" (see message.Normalize and
// engine/tool_call_poison_test.go): that fix keeps a poisoned ToolCall
// marshalable; this one keeps a poisoned history transcodable.
//
// engine.Session's own turn loop is the primary fix (see engine/engine.go:
// a turn that ends abnormally after recording one or more tool calls now
// synthesizes their results immediately, before the poisoned history could
// ever be replayed), which keeps ingest self-consistent for every message
// that actually passes through Session.append. ResolveOrphanToolCalls is
// the defense-in-depth counterpart every transcoder calls at request-build
// time, exactly as ToolCall.safeArguments backstops message.Normalize: a
// history built or mutated by any OTHER producer — a plugin's
// chat.message hook, a hand-rolled provider adapter, a test's scripted
// provider, a session log edited or replayed from an older, unpatched
// binary — must still transcode to a protocol-valid request rather than
// silently dropping the orphaned tool_use or shipping a request the
// provider will reject.
//
// "Immediately after" mirrors the wire requirement: a ToolCall in
// messages[i] is satisfied only by a ToolResult carrying its CallID in
// messages[i+1] when that message has RoleTool — a ToolResult anywhere
// else in history (an earlier or later, non-adjacent tool message) does
// not count, because no transcoder looks for one there either. When
// messages[i+1] is already a RoleTool message, missing results are merged
// into it; otherwise (end of history, or the next message is not
// RoleTool) a new RoleTool message carrying only the synthetic results is
// inserted immediately after messages[i]. Every synthetic ToolResult is
// IsError true with Content set to SyntheticOrphanResultText.
//
// messages is never mutated in place; the input slice and its Message
// values are safe to reuse after this call. When no orphan exists the
// input slice itself is returned unchanged (no allocation).
func ResolveOrphanToolCalls(messages []Message) []Message {
	type insertion struct {
		afterIndex int
		parts      Parts
	}
	pendingMerge := make(map[int]Parts)
	var insertions []insertion

	for i := range messages {
		m := &messages[i]
		if m.Role != RoleAssistant {
			continue
		}
		var callIDs []string
		for _, p := range m.Parts {
			if tc, ok := p.(*ToolCall); ok {
				callIDs = append(callIDs, tc.CallID)
			}
		}
		if len(callIDs) == 0 {
			continue
		}
		followingIsTool := i+1 < len(messages) && messages[i+1].Role == RoleTool
		present := make(map[string]bool)
		if followingIsTool {
			for _, p := range messages[i+1].Parts {
				if tr, ok := p.(*ToolResult); ok {
					present[tr.CallID] = true
				}
			}
		}
		var missing []string
		for _, id := range callIDs {
			if !present[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}
		synth := make(Parts, 0, len(missing))
		for _, id := range missing {
			synth = append(synth, &ToolResult{
				CallID:  id,
				Content: Parts{&Text{Text: SyntheticOrphanResultText}},
				IsError: true,
			})
		}
		if followingIsTool {
			pendingMerge[i+1] = append(pendingMerge[i+1], synth...)
		} else {
			insertions = append(insertions, insertion{afterIndex: i, parts: synth})
		}
	}

	if len(pendingMerge) == 0 && len(insertions) == 0 {
		return messages
	}

	out := make([]Message, 0, len(messages)+len(insertions))
	for i := range messages {
		m := messages[i]
		if extra, ok := pendingMerge[i]; ok {
			merged := m
			merged.Parts = append(append(Parts(nil), m.Parts...), extra...)
			m = merged
		}
		out = append(out, m)
		for _, ins := range insertions {
			if ins.afterIndex == i {
				out = append(out, Message{
					ID:    "synthetic-orphan-tool-result-" + strings.Join(callIDsOf(ins.parts), "-"),
					Role:  RoleTool,
					Parts: ins.parts,
				})
			}
		}
	}
	return out
}

// callIDsOf extracts the CallID of every ToolResult in parts, used only to
// build a stable, debuggable ID for a synthesized RoleTool message.
func callIDsOf(parts Parts) []string {
	ids := make([]string, 0, len(parts))
	for _, p := range parts {
		if tr, ok := p.(*ToolResult); ok {
			ids = append(ids, tr.CallID)
		}
	}
	return ids
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
