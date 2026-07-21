package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// This file's properties are derived from documented contracts only; each
// property below cites the doc comment it rests on. Two contracts the
// package's doc comments gesture at were deliberately NOT turned into a
// property, and are called out here rather than silently skipped:
//
//   - Message-level struct equality after a marshal/unmarshal round trip
//     (Unmarshal(Marshal(m)) == m) does not hold for arbitrary Messages, and
//     is not promised anywhere: Marshal is a canonicalizing function.
//     ToolCall.safeArguments coerces empty/invalid Arguments to "{}"
//     (ToolCall.MarshalJSON's doc), ProviderData.MarshalJSON drops
//     zero-length/invalid entries (ProviderData's doc), and — found while
//     building this file's generator — Parts.MarshalJSON/UnmarshalJSON is a
//     further, undocumented instance of the same shape: a nil Parts
//     marshals to "[]" (len(nil)==0) and unmarshals back to a non-nil,
//     zero-length Parts, so reflect.DeepEqual(Message{Parts: nil},
//     roundTripped) is false even though the two are semantically the same
//     empty part list. Per the task's own guidance for a canonicalizing
//     Marshal, the round-trip property here checks marshal STABILITY
//     instead (marshal ∘ unmarshal ∘ marshal == marshal) — see
//     TestMessageNormalizedMarshalStable.
//   - ModelRef's "both segments must be non-empty" rule (ParseModelRef's
//     doc) is enforced by ParseModelRef but not by the ModelRef struct
//     itself: a hand-built ModelRef{Provider: "x"} (Model empty) marshals
//     successfully (String() doesn't consult ParseModelRef) but then fails
//     to unmarshal, since UnmarshalJSON routes through ParseModelRef, which
//     rejects it. genModelRef below only ever generates the zero ModelRef or
//     one with both segments non-empty, matching the one contract the
//     package actually documents; a ModelRef with exactly one segment empty
//     is a state no documented constructor in this package produces, so
//     round-tripping it is out of scope here.
//
// # A finding, fixed before these properties could pass
//
// TestMessageNormalizedMarshalStable and TestNormalizeIdempotent originally
// shared one test (checking marshal-stability directly on a raw, non-
// Normalized generated Message) and immediately found a real bug:
// Reasoning.ProviderData carrying a non-empty-but-syntactically-invalid
// entry (e.g. a single 0x00 byte) made json.Marshal fail outright — the
// exact "json: error calling MarshalJSON for type json.RawMessage: ..."
// incident this package has already hit once for ToolCall.Arguments (see
// Normalize's doc comment) — because neither Normalize nor
// ProviderData.MarshalJSON checked json.Valid, only len(raw)==0. Both now
// do (message.go); see Normalize's doc comment, "A ProviderData entry has
// the exact same invalid-but-non-empty footgun", for the full fix. This is
// the fuzz-property workflow's second bug find (after PR #85's two), and it
// is fixed in a commit preceding this one.
//
// With that fixed, a second, narrower issue surfaced: a raw (never-
// Normalized) Message is NOT promised to marshal stably across a reload —
// Normalize's own doc says as much for the sibling zero-length-entry case
// ("Both shapes are safe ... but they are not byte-identical") — because
// stripping every invalid/empty entry from a Reasoning.ProviderData map can
// leave it non-nil-but-empty pre-marshal (so "provider_data,omitempty"
// keeps the field, encoded as "{}") while a map reloaded from that exact
// "{}" comes back as a *different* non-nil-but-empty map that
// omitempty then drops entirely on the next marshal — the same
// present-vs-reloaded-empty asymmetry Normalize exists to close, one layer
// up (the whole map, not one entry). The package's actual promise
// ("retranscoding an unchanged history produces identical wire requests")
// is scoped to messages that have gone through Normalize — the one ingest
// choke point every persisted message passes through
// (engine.Session.appendWithUsage) before its first marshal — so
// TestMessageNormalizedMarshalStable normalizes first, matching that scope
// exactly instead of over-claiming stability for input the package never
// promised it for.
//
// The properties actually asserted:
//  1. TestMessageMarshalNeverErrors — Marshal never errors for a raw
//     (un-Normalized) Message built from the part types this package
//     defines, and neither does remarshaling its reloaded form. This is the
//     defense-in-depth contract safeArguments/ProviderData.MarshalJSON
//     document explicitly: "even if a future producer bypasses Normalize
//     entirely ... must still never be able to make a marshal fail."
//  2. TestMessageNormalizedMarshalStable — once Normalized, remarshaling a
//     reloaded Message reproduces the exact same bytes (marshal is a fixed
//     point). This is "retranscoding an unchanged history produces
//     identical wire requests" (ProviderCallID's doc, restated on
//     Normalize) as a property.
//  3. TestNormalizeIdempotent — Normalize's own doc: it "scrubs known
//     encoding/json footguns from m's parts in place" (zero-length/invalid
//     ProviderData entries, invalid ToolCall.Arguments). Nothing on a
//     Message survives a second pass differently from the first, since
//     Normalize's mutations are all saturating (delete once; nil once).
//  4. ResolveOrphanToolCalls's own doc: output carries no orphaned ToolCall
//     (re-derived from "Immediately after mirrors the wire requirement..."
//     below as hasOrphanToolCall), re-applying it to its own output changes
//     nothing further, and the input is never mutated in place ("messages is
//     never mutated in place" — ResolveOrphanToolCalls's doc). See
//     TestResolveOrphanToolCallsPropertyNoOrphans,
//     TestResolveOrphanToolCallsPropertyFixedPoint, and
//     TestResolveOrphanToolCallsPropertyDoesNotMutateInput.

// --- Generators --------------------------------------------------------
//
// genMessage/genMessageWithPool build a Message from all five part types
// PartType enumerates (Text, Blob, ToolCall, ToolResult, Reasoning),
// deliberately WITHOUT constraining which roles carry which parts: none of
// the properties below depend on role/part correspondence (Normalize and
// the marshal round trip are role-agnostic by construction, and
// ResolveOrphanToolCalls's own "orphan" definition already ignores any
// ToolCall not sitting in a RoleAssistant message — see hasOrphanToolCall),
// so generating "off-label" combinations, e.g. a ToolCall inside a
// RoleUser message, is still valid input space and exercises that ignoring
// behavior directly instead of assuming it.
//
// A CallID pool is threaded through a whole generated message (or, for
// genMessageSequence, a whole sequence) so ToolCall and ToolResult CallIDs
// coincide often enough to exercise ResolveOrphanToolCalls's matching logic,
// not just its "definitely orphaned" path.

func genMessage(t *rapid.T) Message {
	var pool []string
	return genMessageWithPool(t, "m", &pool)
}

// genMessageSequence draws a short slice of Messages sharing one CallID
// pool across the whole sequence, so a ToolCall in one message and a
// ToolResult in a later one frequently reference the same CallID —
// producing realistic matched, partially-matched, and orphaned shapes for
// ResolveOrphanToolCalls, rather than relying on pure chance.
func genMessageSequence(t *rapid.T) []Message {
	n := rapid.IntRange(0, 8).Draw(t, "seqLen")
	var pool []string
	msgs := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, genMessageWithPool(t, fmt.Sprintf("seq%d", i), &pool))
	}
	return msgs
}

func genMessageWithPool(t *rapid.T, label string, pool *[]string) Message {
	return Message{
		ID:        rapid.StringN(0, 24, -1).Draw(t, label+"ID"),
		Role:      genRole(t, label),
		Parts:     genParts(t, label, pool),
		Model:     genModelRef(t, label),
		CreatedAt: genCreatedAt(t, label),
	}
}

// genRole draws one of the three roles the package defines most of the
// time, and an arbitrary string (including empty and unicode) the rest of
// the time — nothing in Role's type (a bare string) or Message's JSON
// encoding validates it, so an unrecognized role must not panic or corrupt
// anything else about the message.
func genRole(t *rapid.T, label string) Role {
	if rapid.IntRange(0, 9).Draw(t, label+"RoleWeighted") < 8 {
		return rapid.SampledFrom([]Role{RoleUser, RoleAssistant, RoleTool}).Draw(t, label+"RoleKnown")
	}
	return Role(rapid.StringN(0, 16, -1).Draw(t, label+"RoleArbitrary"))
}

// genModelRef draws the zero ModelRef or one built from two non-empty
// segments — see the file doc comment above for why a one-segment-empty
// ModelRef is out of scope. Provider is generated slash-free: ModelRef's
// doc says only "the model portion may itself contain slashes; the split is
// on the first one", implying Provider itself is assumed not to — a
// Provider containing '/' would still round-trip STABLY (the property this
// file actually checks) but would silently reassign characters from
// Provider to Model on the first reload, which is a real wrinkle worth
// avoiding in the generator rather than asserting on.
func genModelRef(t *rapid.T, label string) ModelRef {
	if rapid.Bool().Draw(t, label+"ModelZero") {
		return ModelRef{}
	}
	provider := strings.ReplaceAll(rapid.StringN(1, 20, -1).Draw(t, label+"ModelProvider"), "/", "-")
	model := rapid.StringN(1, 20, -1).Draw(t, label+"ModelModel")
	return ModelRef{Provider: provider, Model: model}
}

// genCreatedAt draws the zero time.Time (CreatedAt is "omitzero") or a
// UTC, non-monotonic instant built from Unix seconds/nanoseconds — avoiding
// time.Now()-shaped values sidesteps monotonic-reading and Location
// differences that are irrelevant to what this file's properties check
// (marshaled bytes, never time.Time equality directly).
func genCreatedAt(t *rapid.T, label string) time.Time {
	if rapid.Bool().Draw(t, label+"TimeZero") {
		return time.Time{}
	}
	sec := rapid.Int64Range(0, 4102444800).Draw(t, label+"TimeSec") // through ~2100
	nsec := rapid.Int64Range(0, 999999999).Draw(t, label+"TimeNsec")
	return time.Unix(sec, nsec).UTC()
}

func genParts(t *rapid.T, label string, pool *[]string) Parts {
	n := rapid.IntRange(0, 5).Draw(t, label+"PartsLen")
	if n == 0 {
		// Deliberately nil, not Parts{}: exercises the nil-Parts marshal
		// shape documented in the file comment above.
		return nil
	}
	parts := make(Parts, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, genPart(t, fmt.Sprintf("%sPart%d", label, i), pool))
	}
	return parts
}

// genPart draws one of the five concrete Part types PartType enumerates
// (message.go: PartText, PartBlob, PartToolCall, PartToolResult,
// PartReasoning) — every part type this package defines.
func genPart(t *rapid.T, label string, pool *[]string) Part {
	switch rapid.IntRange(0, 4).Draw(t, label+"Kind") {
	case 0:
		return genText(t, label)
	case 1:
		return genBlob(t, label)
	case 2:
		return genToolCall(t, label, pool)
	case 3:
		return genToolResult(t, label, pool)
	default:
		return genReasoning(t, label)
	}
}

func genText(t *rapid.T, label string) *Text {
	return &Text{Text: rapid.String().Draw(t, label+"Text")}
}

// genBlob draws MediaType plus, per Blob's doc ("Data ... Mutually
// exclusive with URL"), at most one of Data/URL.
func genBlob(t *rapid.T, label string) *Blob {
	b := &Blob{MediaType: rapid.StringN(0, 20, -1).Draw(t, label+"MediaType")}
	switch rapid.IntRange(0, 2).Draw(t, label+"BlobKind") {
	case 0:
		// Neither Data nor URL.
	case 1:
		b.Data = rapid.SliceOfN(rapid.Byte(), 0, 32).Draw(t, label+"BlobData")
	default:
		b.URL = rapid.StringN(0, 40, -1).Draw(t, label+"BlobURL")
	}
	return b
}

// genCallID draws a CallID either reused from pool (when non-empty) or
// freshly generated (and then added to pool), so ToolCall/ToolResult CallIDs
// coincide across a message or sequence with reasonable probability.
func genCallID(t *rapid.T, label string, pool *[]string) string {
	if len(*pool) > 0 && rapid.Bool().Draw(t, label+"FromPool") {
		return rapid.SampledFrom(*pool).Draw(t, label+"PoolPick")
	}
	id := rapid.StringN(0, 16, -1).Draw(t, label+"Fresh")
	*pool = append(*pool, id)
	return id
}

func genToolCall(t *rapid.T, label string, pool *[]string) *ToolCall {
	return &ToolCall{
		CallID:    genCallID(t, label+"CallID", pool),
		Name:      rapid.StringN(0, 20, -1).Draw(t, label+"Name"),
		Arguments: genToolArguments(t, label),
	}
}

// genToolArguments covers every Arguments shape safeArguments's doc
// comment names: absent (nil/empty), the valid-but-truncated shape a
// dropped stream leaves behind (Normalize's doc, "A salvaged tool call must
// never carry invalid Arguments"), and ordinary valid JSON.
func genToolArguments(t *rapid.T, label string) json.RawMessage {
	switch rapid.IntRange(0, 4).Draw(t, label+"ArgsKind") {
	case 0:
		return nil
	case 1:
		return json.RawMessage{}
	case 2:
		v := rapid.String().Draw(t, label+"ArgsValue")
		b, err := json.Marshal(map[string]string{"v": v})
		if err != nil {
			t.Fatalf("genToolArguments: marshal fixture: %v", err)
		}
		return json.RawMessage(b)
	case 3:
		return json.RawMessage(rapid.SampledFrom([]string{
			`{"command":"echo hel`,
			`{`,
			`not json`,
			`{"a":1`,
			`"unterminated string`,
		}).Draw(t, label+"ArgsInvalid"))
	default:
		return json.RawMessage(rapid.SliceOfN(rapid.Byte(), 0, 32).Draw(t, label+"ArgsBytes"))
	}
}

// genToolResult's Content is restricted to Text/Blob parts per ToolResult's
// own doc comment: "Content may hold Text and Blob parts only."
func genToolResult(t *rapid.T, label string, pool *[]string) *ToolResult {
	n := rapid.IntRange(0, 3).Draw(t, label+"ContentLen")
	content := make(Parts, 0, n)
	for i := 0; i < n; i++ {
		itemLabel := fmt.Sprintf("%sContent%d", label, i)
		if rapid.Bool().Draw(t, itemLabel+"Kind") {
			content = append(content, genText(t, itemLabel))
		} else {
			content = append(content, genBlob(t, itemLabel))
		}
	}
	return &ToolResult{
		CallID:  genCallID(t, label+"CallID", pool),
		Content: content,
		IsError: rapid.Bool().Draw(t, label+"IsError"),
	}
}

// genReasoning covers Reasoning.ProviderData's documented empty/oversized-
// adjacent shapes via genProviderData: a zero-length entry (which
// Normalize's doc says it deletes) is drawn with real probability so
// TestNormalizeIdempotent actually exercises that mutation, not just its
// absence.
func genReasoning(t *rapid.T, label string) *Reasoning {
	return &Reasoning{
		Text:         rapid.String().Draw(t, label+"ReasoningText"),
		ProviderData: genProviderData(t, label),
	}
}

func genProviderData(t *rapid.T, label string) ProviderData {
	n := rapid.IntRange(0, 3).Draw(t, label+"PDLen")
	if n == 0 {
		if rapid.Bool().Draw(t, label+"PDNil") {
			return nil
		}
		return ProviderData{}
	}
	pd := make(ProviderData, n)
	for i := 0; i < n; i++ {
		itemLabel := fmt.Sprintf("%sPD%d", label, i)
		family := rapid.OneOf(
			rapid.SampledFrom([]string{"anthropic", "openai-responses", "openai-chat"}),
			rapid.StringN(0, 12, -1),
		).Draw(t, itemLabel+"Family")
		var raw json.RawMessage
		switch rapid.IntRange(0, 2).Draw(t, itemLabel+"Kind") {
		case 0:
			// Zero-length: the shape Normalize deletes (see the package
			// doc's "A salvaged tool call must never carry invalid
			// Arguments" / Get's doc on the ProviderData twin footgun).
			raw = json.RawMessage{}
		case 1:
			v := rapid.String().Draw(t, itemLabel+"Value")
			b, err := json.Marshal(map[string]string{"v": v})
			if err != nil {
				t.Fatalf("genProviderData: marshal fixture: %v", err)
			}
			raw = json.RawMessage(b)
		default:
			raw = json.RawMessage(rapid.SliceOfN(rapid.Byte(), 1, 48).Draw(t, itemLabel+"Bytes"))
		}
		pd[family] = raw
	}
	return pd
}

// --- Properties ----------------------------------------------------------

// TestMessageMarshalRoundTripStable is property 1 — see the file doc
// comment for why this checks marshal STABILITY rather than
// Unmarshal(Marshal(m)) == m.
func TestMessageMarshalNeverErrors(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := genMessage(t) // deliberately NOT Normalized — see file doc comment.

		raw1, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal(m) failed for a Message built only from this package's own part types: %v", err)
		}

		var reloaded Message
		if err := json.Unmarshal(raw1, &reloaded); err != nil {
			t.Fatalf("Unmarshal(Marshal(m)) failed: %v\nraw: %s", err, raw1)
		}

		if _, err := json.Marshal(reloaded); err != nil {
			t.Fatalf("Marshal(Unmarshal(Marshal(m))) failed: %v\nfirst marshal: %s", err, raw1)
		}
	})
}

// TestMessageNormalizedMarshalStable is property 2 — see the file doc
// comment ("A finding, fixed before these properties could pass") for why
// this Normalizes before checking stability rather than on the raw
// generated Message.
func TestMessageNormalizedMarshalStable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := genMessage(t)
		m.Normalize()

		raw1, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal(m) after Normalize failed: %v", err)
		}

		var reloaded Message
		if err := json.Unmarshal(raw1, &reloaded); err != nil {
			t.Fatalf("Unmarshal(Marshal(m)) failed: %v\nraw: %s", err, raw1)
		}

		raw2, err := json.Marshal(reloaded)
		if err != nil {
			t.Fatalf("Marshal(Unmarshal(Marshal(m))) failed: %v\nfirst marshal: %s", err, raw1)
		}

		if !bytes.Equal(raw1, raw2) {
			t.Fatalf("marshal of a Normalized message is not a fixed point after one round trip:\n first: %s\nsecond: %s", raw1, raw2)
		}
	})
}

// TestNormalizeIdempotent is property 2: Normalize(Normalize(m)) leaves m in
// the same state Normalize(m) alone does (compared via marshaled bytes,
// which is exact for this purpose since Normalize's only effects — deleting
// map entries and nilling Arguments — are both fully captured by the
// canonical JSON encoding).
func TestNormalizeIdempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := genMessage(t)

		m.Normalize()
		once, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal after one Normalize: %v", err)
		}

		m.Normalize()
		twice, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal after two Normalize calls: %v", err)
		}

		if !bytes.Equal(once, twice) {
			t.Fatalf("Normalize is not idempotent:\n once: %s\ntwice: %s", once, twice)
		}
	})
}

// hasOrphanToolCall re-derives ResolveOrphanToolCalls's own documented
// definition of "orphan" independently of its implementation: a ToolCall
// carried by a RoleAssistant message at index i is orphaned unless
// messages[i+1] exists, has RoleTool, and carries a ToolResult with a
// matching CallID — ResolveOrphanToolCalls's doc comment, "Immediately
// after mirrors the wire requirement ... a ToolCall in messages[i] is
// satisfied only by a ToolResult carrying its CallID in messages[i+1] when
// that message has RoleTool".
func hasOrphanToolCall(messages []Message) bool {
	for i, m := range messages {
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
		present := make(map[string]bool)
		if i+1 < len(messages) && messages[i+1].Role == RoleTool {
			for _, p := range messages[i+1].Parts {
				if tr, ok := p.(*ToolResult); ok {
					present[tr.CallID] = true
				}
			}
		}
		for _, id := range callIDs {
			if !present[id] {
				return true
			}
		}
	}
	return false
}

// TestResolveOrphanToolCallsPropertyNoOrphans is the first half of property 3: no
// output of ResolveOrphanToolCalls contains an orphan under hasOrphan
// ToolCall's independent re-derivation of the function's own doc comment.
func TestResolveOrphanToolCallsPropertyNoOrphans(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := genMessageSequence(t)
		out := ResolveOrphanToolCalls(in)
		if hasOrphanToolCall(out) {
			t.Fatalf("ResolveOrphanToolCalls left an orphan tool call in its output: %+v", out)
		}
	})
}

// TestResolveOrphanToolCallsPropertyFixedPoint is the second half of property 3:
// re-applying ResolveOrphanToolCalls to its own output changes nothing
// further — every orphan it can find, it resolves in one pass.
func TestResolveOrphanToolCallsPropertyFixedPoint(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := genMessageSequence(t)
		out := ResolveOrphanToolCalls(in)

		raw1, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("Marshal(out): %v", err)
		}
		again := ResolveOrphanToolCalls(out)
		raw2, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("Marshal(again): %v", err)
		}
		if !bytes.Equal(raw1, raw2) {
			t.Fatalf("ResolveOrphanToolCalls is not a fixed point on its own output:\n first: %s\nsecond: %s", raw1, raw2)
		}
	})
}

// TestResolveOrphanToolCallsPropertyDoesNotMutateInput pins ResolveOrphanToolCalls's
// doc comment: "messages is never mutated in place; the input slice and its
// Message values are safe to reuse after this call."
func TestResolveOrphanToolCallsPropertyDoesNotMutateInput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := genMessageSequence(t)

		before, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal(in) before call: %v", err)
		}

		_ = ResolveOrphanToolCalls(in)

		after, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal(in) after call: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("ResolveOrphanToolCalls mutated its input in place:\nbefore: %s\nafter:  %s", before, after)
		}
	})
}
