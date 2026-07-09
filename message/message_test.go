package message

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	in := Message{
		ID:   "msg_1",
		Role: RoleAssistant,
		Parts: Parts{
			&Text{Text: "hello"},
			&Reasoning{
				Text: "thinking summary",
				ProviderData: ProviderData{
					"anthropic": json.RawMessage(`{"signature":"abc"}`),
				},
			},
			&ToolCall{
				CallID:    "tc_1",
				Name:      "bash",
				Arguments: json.RawMessage(`{"command":"ls"}`),
			},
			&Blob{MediaType: "image/png", Data: []byte{0x89, 0x50}},
		},
		Model:     ModelRef{Provider: "anthropic", Model: "claude-fable-5"},
		CreatedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Message
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestToolResultRoundTrip(t *testing.T) {
	in := Message{
		ID:   "msg_2",
		Role: RoleTool,
		Parts: Parts{
			&ToolResult{
				CallID: "tc_1",
				Content: Parts{
					&Text{Text: "output line"},
					&Blob{MediaType: "image/png", URL: "https://example.com/x.png"},
				},
				IsError: true,
			},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Message
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestPartTypeDiscriminator(t *testing.T) {
	raw, err := json.Marshal(Parts{&Text{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"type":"text"`) {
		t.Errorf("marshaled part missing type discriminator: %s", raw)
	}
}

func TestUnknownPartTypeErrors(t *testing.T) {
	var ps Parts
	err := json.Unmarshal([]byte(`[{"type":"holo_deck"}]`), &ps)
	if err == nil {
		t.Fatal("expected error for unknown part type")
	}
}

func TestPartsText(t *testing.T) {
	ps := Parts{
		&Text{Text: "a"},
		&Blob{MediaType: "image/png"},
		&Text{Text: "b"},
	}
	if got := ps.Text(); got != "a\nb" {
		t.Errorf("Text() = %q, want %q", got, "a\nb")
	}
}

func TestProviderCallID(t *testing.T) {
	a := ProviderCallID("toolu_", "tc_1", 24)
	b := ProviderCallID("toolu_", "tc_1", 24)
	if a != b {
		t.Errorf("not deterministic: %q vs %q", a, b)
	}
	if len(a) != 24 {
		t.Errorf("len = %d, want 24", len(a))
	}
	if !strings.HasPrefix(a, "toolu_") {
		t.Errorf("missing prefix: %q", a)
	}
	if ProviderCallID("call_", "tc_2", 0) == ProviderCallID("call_", "tc_3", 0) {
		t.Error("distinct call IDs collided")
	}
}

func TestModelRef(t *testing.T) {
	ref, err := ParseModelRef("amazon-bedrock/us.anthropic.claude-opus-4-8")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Provider != "amazon-bedrock" || ref.Model != "us.anthropic.claude-opus-4-8" {
		t.Errorf("unexpected parse: %+v", ref)
	}

	// Model portion may contain slashes.
	ref, err = ParseModelRef("vertex/publishers/google/gemini-3.1-pro")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Model != "publishers/google/gemini-3.1-pro" {
		t.Errorf("slash split wrong: %+v", ref)
	}

	for _, bad := range []string{"", "anthropic", "/model", "anthropic/"} {
		if _, err := ParseModelRef(bad); err == nil {
			t.Errorf("ParseModelRef(%q) should fail", bad)
		}
	}

	raw, err := json.Marshal(ModelRef{Provider: "anthropic", Model: "claude-fable-5"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"anthropic/claude-fable-5"` {
		t.Errorf("marshal = %s", raw)
	}
	var out ModelRef
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "anthropic/claude-fable-5" {
		t.Errorf("unmarshal = %+v", out)
	}

	// Zero ref round-trips through "".
	raw, _ = json.Marshal(ModelRef{})
	if string(raw) != `""` {
		t.Errorf("zero marshal = %s", raw)
	}
	var zero ModelRef
	if err := json.Unmarshal(raw, &zero); err != nil {
		t.Fatal(err)
	}
	if !zero.IsZero() {
		t.Errorf("zero unmarshal = %+v", zero)
	}
}

// TestToolCallEmptyArgumentsMarshal is the regression guard for the
// json.RawMessage footgun that produced the goal-supervised session
// incident (session ses_01kx3pvqttfwgbf2n5x1f1y8yh.jsonl): a worker turn's
// json.Marshal failed with "json: error calling MarshalJSON for type
// json.RawMessage: unexpected end of JSON input" because a ToolCall's
// Arguments field was an empty-but-non-nil json.RawMessage.
// json.RawMessage.MarshalJSON only special-cases nil (-> "null"); any other
// zero-length value is handed to the encoder unvalidated, and zero bytes is
// not valid JSON. `omitempty` does not help: it tests the Go zero value
// (nil), not len == 0.
//
// Both an empty non-nil Arguments and a nil Arguments must marshal cleanly
// — as a Parts element (marshalPart's tagged union) and as a bare ToolCall
// value (any direct struct field elsewhere, e.g. an SSE event's ToolCall
// pointer) — and must not lose the "type" discriminator when marshaled as
// a Parts element.
//
// The normalized value must be "{}", not "null": every transcoder
// (provider/anthropic, provider/openai) already coerces a zero-length
// Arguments to an empty JSON object before sending it to the provider, so
// the canonical marshal must agree — a "null" here would diverge from what
// actually goes out on the wire and would not survive a resumed session's
// retranscode as a valid tool-call arguments value.
func TestToolCallEmptyArgumentsMarshal(t *testing.T) {
	cases := []struct {
		name string
		args json.RawMessage
	}{
		{"nil", nil},
		{"empty-non-nil", json.RawMessage{}},
		{"empty-string-literal", json.RawMessage("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := &ToolCall{CallID: "tc_1", Name: "bash", Arguments: c.args}

			// Bare ToolCall value, as any direct struct field would encode
			// it (e.g. an event's ToolCall pointer) — this is exactly the
			// json.Marshal(ToolCall{...}) call that failed in production.
			bareRaw, err := json.Marshal(*tc)
			if err != nil {
				t.Fatalf("marshal bare ToolCall: %v", err)
			}
			if !strings.Contains(string(bareRaw), `"arguments":{}`) {
				t.Errorf("bare ToolCall arguments = %s, want normalized to {}", bareRaw)
			}

			// As a Parts element: must round-trip through the tagged
			// union without losing the "type" discriminator.
			raw, err := json.Marshal(Parts{tc})
			if err != nil {
				t.Fatalf("marshal Parts: %v", err)
			}
			if !strings.Contains(string(raw), `"arguments":{}`) {
				t.Errorf("Parts element arguments = %s, want normalized to {}", raw)
			}
			var out Parts
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("unmarshal Parts: %v (raw=%s)", err, raw)
			}
			if len(out) != 1 {
				t.Fatalf("len(out) = %d, want 1 (raw=%s)", len(out), raw)
			}
			got, ok := out[0].(*ToolCall)
			if !ok {
				t.Fatalf("out[0] = %T, want *ToolCall (raw=%s)", out[0], raw)
			}
			if got.CallID != "tc_1" || got.Name != "bash" {
				t.Errorf("got = %+v, want CallID=tc_1 Name=bash (raw=%s)", got, raw)
			}
			if string(got.Arguments) != "{}" {
				t.Errorf("got.Arguments = %s, want {}", got.Arguments)
			}
		})
	}
}

// TestToolCallEmptyArgumentsRoundTripMatchesTranscodeConvention is the
// marshal -> unmarshal -> transcode-path round trip: a ToolCall with
// zero-length Arguments, persisted to canonical JSON and reloaded (as a
// resumed session would), must present Arguments in exactly the form the
// provider transcoders expect to consume — an empty JSON object, not null
// — so that a transcoder's own "coerce empty to {}" fallback (which only
// fires on len(Arguments) == 0) is not silently bypassed by a literal
// "null" that has non-zero length but is not a usable arguments object.
func TestToolCallEmptyArgumentsRoundTripMatchesTranscodeConvention(t *testing.T) {
	msg := Message{
		ID:   "msg_1",
		Role: RoleAssistant,
		Parts: Parts{
			&ToolCall{CallID: "tc_1", Name: "bash", Arguments: json.RawMessage{}},
		},
	}

	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var reloaded Message
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	tc, ok := reloaded.Parts[0].(*ToolCall)
	if !ok {
		t.Fatalf("reloaded part = %T, want *ToolCall", reloaded.Parts[0])
	}

	// The transcode-path expectation: transcoders test len(Arguments) == 0
	// to decide whether to substitute their own empty-object literal (see
	// provider/anthropic/transcode.go and provider/openai/transcode.go).
	// After a round trip through canonical JSON, Arguments must already be
	// "{}" — valid, non-empty, parseable JSON that a transcoder can pass
	// straight through — never the 4-byte non-object literal "null".
	if len(tc.Arguments) == 0 {
		t.Fatalf("reloaded Arguments has zero length, want non-empty {}")
	}
	if string(tc.Arguments) == "null" {
		t.Fatalf("reloaded Arguments round-tripped to null, want {} (diverges from transcoder convention)")
	}
	var obj map[string]any
	if err := json.Unmarshal(tc.Arguments, &obj); err != nil {
		t.Fatalf("reloaded Arguments not a JSON object: %v (arguments=%s)", err, tc.Arguments)
	}
	if len(obj) != 0 {
		t.Errorf("reloaded Arguments = %s, want empty object {}", tc.Arguments)
	}
}

// TestMarshalPartToolCallFieldsMatchStruct is a reflection-based divergence
// guard for marshalPart's *ToolCall case. That case deliberately does not
// embed *ToolCall (embedding would promote ToolCall.MarshalJSON onto the
// wrapper and silently drop the "type" discriminator — see the comment on
// marshalPart) and instead reconstructs ToolCall's fields one by one in an
// inline anonymous struct. That reconstruction is invisible to the
// compiler: adding a field to ToolCall does not fail to compile here, it
// just silently stops appearing in the Parts-element JSON.
//
// This test compares the set of JSON keys ToolCall's own field tags produce
// against the set of keys marshalPart actually emits for a *ToolCall (minus
// the "type" discriminator, which has no counterpart on the bare struct).
// If someone adds a field to ToolCall without updating marshalPart's
// reconstruction, the key sets diverge and this test fails with the
// specific missing key named, instead of the field silently vanishing from
// every persisted tool call.
func TestMarshalPartToolCallFieldsMatchStruct(t *testing.T) {
	structKeys := jsonFieldKeys(t, reflect.TypeOf(ToolCall{}))

	tc := &ToolCall{CallID: "tc_1", Name: "bash", Arguments: json.RawMessage(`{"a":1}`)}
	raw, err := marshalPart(tc)
	if err != nil {
		t.Fatalf("marshalPart: %v", err)
	}
	var wireObj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wireObj); err != nil {
		t.Fatalf("unmarshal marshalPart output: %v", err)
	}
	delete(wireObj, "type") // the tagged-union discriminator; not a ToolCall field.
	wireKeys := make(map[string]bool, len(wireObj))
	for k := range wireObj {
		wireKeys[k] = true
	}

	for k := range structKeys {
		if !wireKeys[k] {
			t.Errorf("ToolCall field with JSON key %q is present on the struct but missing from marshalPart's Parts-element encoding — the explicit field-by-field reconstruction in marshalPart's *ToolCall case must be updated to include it", k)
		}
	}
	for k := range wireKeys {
		if !structKeys[k] {
			t.Errorf("marshalPart's Parts-element encoding emits key %q with no corresponding ToolCall struct field", k)
		}
	}
}

// jsonFieldKeys returns the set of JSON object keys a struct type's own
// fields (as reflected via their `json` tags) would produce. It ignores
// "-" (skip) tags and options like ",omitempty"; fields without a json tag
// fall back to their Go name to match encoding/json's default behavior.
func jsonFieldKeys(t *testing.T, typ reflect.Type) map[string]bool {
	t.Helper()
	if typ.Kind() != reflect.Struct {
		t.Fatalf("jsonFieldKeys: %v is not a struct", typ)
	}
	keys := make(map[string]bool)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		keys[name] = true
	}
	return keys
}

// TestMessageWithEmptyToolCallArgumentsMarshal proves the full incident
// shape: an assistant Message carrying a ToolCall with an empty-non-nil
// Arguments — the shape engine.Session.append persists to the session log
// and the server journals and serves from GET /session/{id}/message —
// marshals successfully end to end.
func TestMessageWithEmptyToolCallArgumentsMarshal(t *testing.T) {
	m := Message{
		ID:   "msg_1",
		Role: RoleAssistant,
		Parts: Parts{
			&ToolCall{CallID: "tc_1", Name: "bash", Arguments: json.RawMessage{}},
		},
	}
	if _, err := json.Marshal(m); err != nil {
		t.Fatalf("marshal message with empty-non-nil tool call arguments: %v", err)
	}
	if _, err := json.Marshal([]Message{m}); err != nil {
		t.Fatalf("marshal []Message (the /message response shape): %v", err)
	}
}

// TestReasoningProviderDataEmptyMarshal is the round-2 forensic regression
// guard: #42 normalized ToolCall.Arguments (a bare json.RawMessage field)
// but left Reasoning.ProviderData — a map[string]json.RawMessage carrying
// the exact same footgun one layer of indirection away — completely
// unguarded. This reproduces the incident shape directly: a Reasoning part
// whose provider_data entry is present but zero-length (non-nil), the same
// "no data yet" shape a partially-assembled provider stream item can leave
// behind. Before the ProviderData.MarshalJSON guard this failed with
// exactly "json: error calling MarshalJSON for type json.RawMessage:
// unexpected end of JSON input" — the production error.
func TestReasoningProviderDataEmptyMarshal(t *testing.T) {
	cases := []struct {
		name string
		data json.RawMessage
	}{
		{"nil", nil},
		{"empty-non-nil", json.RawMessage{}},
		{"empty-string-literal", json.RawMessage("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Reasoning{
				Text:         "thinking",
				ProviderData: ProviderData{"anthropic": c.data},
			}

			// Bare Reasoning value, as a direct struct field would encode
			// it (e.g. an Event's Message field, or a plugin hook payload
			// carrying a message.Message) — no tagged-union wrapper
			// involved.
			if _, err := json.Marshal(*r); err != nil {
				t.Fatalf("marshal bare Reasoning: %v", err)
			}

			// As a Parts element: the tagged-union path every session
			// message goes through.
			raw, err := json.Marshal(Parts{r})
			if err != nil {
				t.Fatalf("marshal Parts: %v", err)
			}
			var out Parts
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("unmarshal Parts: %v (raw=%s)", err, raw)
			}
			if len(out) != 1 {
				t.Fatalf("len(out) = %d, want 1 (raw=%s)", len(out), raw)
			}
			got, ok := out[0].(*Reasoning)
			if !ok {
				t.Fatalf("out[0] = %T, want *Reasoning (raw=%s)", out[0], raw)
			}
			// The empty entry must not survive as spurious "present" data:
			// Get must report it absent, exactly as ToolCall.Arguments
			// normalizes an empty entry to a well-defined shape rather than
			// silently keeping a footgun around for the next consumer.
			if _, ok := got.ProviderData.Get("anthropic"); ok {
				t.Errorf("got.ProviderData.Get(\"anthropic\") = present, want absent (raw=%s)", raw)
			}
		})
	}
}

// TestMessageWithEmptyReasoningProviderDataMarshal proves the full incident
// shape end to end: an assistant Message carrying a Reasoning part whose
// provider_data entry is empty-non-nil — the shape engine.Session.append
// persists to the session log and the server journals — marshals
// successfully, both alone and as the []Message shape GET
// /session/{id}/message returns.
func TestMessageWithEmptyReasoningProviderDataMarshal(t *testing.T) {
	m := Message{
		ID:   "msg_1",
		Role: RoleAssistant,
		Parts: Parts{
			&Reasoning{
				Text:         "thinking",
				ProviderData: ProviderData{"anthropic": json.RawMessage{}},
			},
			&Text{Text: "hello"},
		},
	}
	if _, err := json.Marshal(m); err != nil {
		t.Fatalf("marshal message with empty-non-nil reasoning provider_data: %v", err)
	}
	if _, err := json.Marshal([]Message{m}); err != nil {
		t.Fatalf("marshal []Message (the /message response shape): %v", err)
	}
}

// TestProviderDataGetOversizedEntryIsAbsent is the round-3 forensic
// regression guard: ProviderData.Get treated only an empty entry as
// "absent" (round 2's fix), leaving an oversized one — a thinking
// signature or redacted_thinking payload with no upper bound at all — to
// be replayed verbatim on every subsequent request for the rest of the
// session (see the package doc, "Unbounded replay is a request-size/time
// bomb"). This is a synthetic fixture shaped like the incident (a
// production session carried one ~30KB signature against seven ~500-byte
// siblings in the same run), not session-log content: a byte slice one
// byte over maxProviderDataEntry, and one exactly at the boundary.
func TestProviderDataGetOversizedEntryIsAbsent(t *testing.T) {
	small := json.RawMessage(`{"signature":"c2hvcnQ="}`) // ~24 bytes, ordinary size
	atCap := append(append(json.RawMessage(`"`), bytes.Repeat([]byte("a"), maxProviderDataEntry-2)...), '"')
	overCap := append(append(json.RawMessage(`"`), bytes.Repeat([]byte("a"), maxProviderDataEntry-1)...), '"')

	pd := ProviderData{
		"small":  small,
		"at-cap": atCap,
		"over":   overCap,
	}

	if _, ok := pd.Get("small"); !ok {
		t.Error(`Get("small") = absent, want present (ordinary-sized entry)`)
	}
	if got := len(atCap); got != maxProviderDataEntry {
		t.Fatalf("fixture bug: len(atCap) = %d, want exactly %d", got, maxProviderDataEntry)
	}
	if _, ok := pd.Get("at-cap"); !ok {
		t.Error(`Get("at-cap") = absent, want present (exactly at the cap)`)
	}
	if got := len(overCap); got <= maxProviderDataEntry {
		t.Fatalf("fixture bug: len(overCap) = %d, want > %d", got, maxProviderDataEntry)
	}
	if _, ok := pd.Get("over"); ok {
		t.Error(`Get("over") = present, want absent (one byte over the cap — the request-size-bomb entry)`)
	}
}

// TestToolCallInvalidTruncatedArgumentsMarshal is the defense-in-depth
// regression guard from the incident behind two production goal sessions,
// ses_01kx453ewfedqrg7p3c64f8sca and ses_01kx453ev9ejattygpf7rbzptw: both
// died at the start of a worker turn with "json: error calling MarshalJSON
// for type json.RawMessage: unexpected end of JSON input", and
// GET /session/{id}/message on them then 500'd with the message.Parts
// wrapper of the same error.
//
// Every guard here at the time (TestToolCallEmptyArgumentsMarshal,
// TestReasoningProviderDataEmptyMarshal) special-cased len(Arguments) == 0
// only. A provider stream that dies mid tool_use block — a dropped
// connection during input_json_delta accumulation, or (as audited in
// provider/anthropic/anthropic.go) a max_tokens cutoff mid tool-call, which
// the Anthropic wire protocol still closes out with a normal
// content_block_stop/message_delta/message_stop sequence — can leave
// Arguments non-empty but syntactically invalid (truncated) JSON. That
// value sails straight past the len==0 guard, and
// json.RawMessage.MarshalJSON does not validate its bytes at all: the
// failure only appears once the value is embedded in a larger document and
// encoding/json compacts it to validate, which is exactly why it looked
// like two different bugs (a bare marshal "succeeds", the same value one
// layer deeper fails) before this test pinned both call sites at once.
//
// This is the second, independent half of the incident's fix (see
// TestNormalizeDropsInvalidToolCallArguments for the primary, ingest-time
// half in Message.Normalize): even if a future producer bypasses Normalize
// entirely — a plugin's chat.message hook building a Message by hand, a
// hand-rolled provider adapter, a test's scripted provider — safeArguments
// itself must never let invalid Arguments reach the encoder.
func TestToolCallInvalidTruncatedArgumentsMarshal(t *testing.T) {
	cases := []struct {
		name string
		args json.RawMessage
	}{
		{"truncated-object", json.RawMessage(`{"command":"echo hel`)},
		{"truncated-string", json.RawMessage(`{"command":"ls`)},
		{"lone-open-brace", json.RawMessage(`{`)},
		{"garbage", json.RawMessage(`not json`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := &ToolCall{CallID: "tc_1", Name: "bash", Arguments: c.args}

			// Bare ToolCall value, as any direct struct field would encode
			// it (e.g. an event's ToolCall pointer) — this is the shape
			// that would otherwise marshal "successfully" (RawMessage's own
			// MarshalJSON does not validate) only to fail once nested.
			bareRaw, err := json.Marshal(*tc)
			if err != nil {
				t.Fatalf("marshal bare ToolCall: %v", err)
			}
			if !strings.Contains(string(bareRaw), `"arguments":{}`) {
				t.Errorf("bare ToolCall arguments = %s, want normalized to {}", bareRaw)
			}

			// As a Parts element: the tagged-union path every session
			// message goes through, and the exact nesting (Parts inside a
			// Message inside a []Message) that turned this into "message:
			// unexpected end of JSON input" in persistMessage and
			// "message.Parts: ..." from GET /message.
			raw, err := json.Marshal(Parts{tc})
			if err != nil {
				t.Fatalf("marshal Parts: %v", err)
			}
			if !strings.Contains(string(raw), `"arguments":{}`) {
				t.Errorf("Parts element arguments = %s, want normalized to {}", raw)
			}

			m := Message{ID: "msg_1", Role: RoleAssistant, Parts: Parts{tc}}
			if _, err := json.Marshal(m); err != nil {
				t.Fatalf("marshal message: %v", err)
			}
			if _, err := json.Marshal([]Message{m}); err != nil {
				t.Fatalf("marshal []Message (the /message response shape): %v", err)
			}
		})
	}
}

// TestNormalizeDropsInvalidToolCallArguments is the primary-fix regression
// guard for the incident behind two production goal sessions,
// ses_01kx453ewfedqrg7p3c64f8sca and ses_01kx453ev9ejattygpf7rbzptw: both
// died at the start of a worker turn with "json: error calling MarshalJSON
// for type json.RawMessage: unexpected end of JSON input" — three identical
// attempts, because every retry re-transcoded the same poisoned history —
// and GET /session/{id}/message on them then 500'd with the message.Parts
// wrapper of the same error, while the on-disk log stayed clean (the
// poisoned message failed to persist and was never journaled).
//
// Message.Normalize is the one ingest choke point every message passes
// through before entering a session's history (engine.Session.append), so
// it is where a salvaged, truncated tool call — left behind by a provider
// stream that dies mid tool_use block, e.g. a connection drop during
// input_json_delta accumulation, or a max_tokens cutoff mid tool-call that
// the wire protocol still closes out normally (see
// provider/anthropic/anthropic.go) — must be sanitized once, rather than
// leaving every downstream consumer (persist, GET /message, a future
// transcoder) to separately guard against it.
//
// Dropping only Arguments (not the whole ToolCall part) preserves the most
// information safely: CallID and Name say which tool the model was in the
// middle of calling, which is useful context for a human or a next turn,
// and neither can be malformed the way a truncated JSON blob can — they are
// plain strings the provider adapter set directly from wire fields, never
// json.RawMessage. Only Arguments carries the footgun, so only Arguments is
// cleared, to nil (not "{}") at this layer: safeArguments and every
// transcoder already coerce a nil/zero-length Arguments to an empty JSON
// object at the one place each of them needs to, so nil here is the same
// "no usable arguments" signal Normalize already sends for a
// present-but-zero-length ProviderData entry, not a third distinct shape
// for downstream code to learn.
func TestNormalizeDropsInvalidToolCallArguments(t *testing.T) {
	m := Message{
		ID:   "msg_1",
		Role: RoleAssistant,
		Parts: Parts{
			&Text{Text: "running"},
			&ToolCall{CallID: "tc_1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo hel`)},
		},
	}
	m.Normalize()

	tc, ok := m.Parts[1].(*ToolCall)
	if !ok {
		t.Fatalf("m.Parts[1] = %T, want *ToolCall", m.Parts[1])
	}
	if tc.CallID != "tc_1" || tc.Name != "bash" {
		t.Errorf("Normalize must preserve CallID/Name, got %+v", tc)
	}
	if len(tc.Arguments) != 0 {
		t.Errorf("Normalize left invalid Arguments = %s, want cleared", tc.Arguments)
	}

	// A genuinely empty (already-handled) Arguments is left exactly as
	// Normalize found it — Normalize does not need to touch what
	// safeArguments already normalizes at marshal time.
	m2 := Message{Parts: Parts{&ToolCall{CallID: "tc_2", Name: "bash", Arguments: json.RawMessage{}}}}
	m2.Normalize()
	if len(m2.Parts[0].(*ToolCall).Arguments) != 0 {
		t.Errorf("Normalize should not add content to an already-empty Arguments")
	}

	// Valid, complete Arguments must survive untouched.
	m3 := Message{Parts: Parts{&ToolCall{CallID: "tc_3", Name: "bash", Arguments: json.RawMessage(`{"a":1}`)}}}
	m3.Normalize()
	if string(m3.Parts[0].(*ToolCall).Arguments) != `{"a":1}` {
		t.Errorf("Normalize altered valid Arguments: %s, want unchanged {\"a\":1}", m3.Parts[0].(*ToolCall).Arguments)
	}
}
