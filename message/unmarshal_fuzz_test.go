package message

import (
	"bytes"
	"encoding/json"
	"testing"
)

// FuzzUnmarshalMessage fuzzes json.Unmarshal into a Message with arbitrary
// bytes — the entry point every consumer of the persisted format actually
// uses (session logs on disk, the server journal, GET /session/{id}/message
// responses being re-parsed) per the package doc: "Package message defines
// the canonical message format stored in session logs."
//
// Invariants: no panic; any input UnmarshalJSON accepts marshals without
// error (Normalized or not — the defense-in-depth guards this checks are
// unconditional); and once Normalized, that remarshal is itself stable
// (remarshal ∘ unmarshal ∘ remarshal == remarshal), mirroring
// TestMessageNormalizedMarshalStable's scope for generator-built Messages
// (see properties_test.go's file doc comment for exactly why stability is
// scoped to Normalize, including the real bug that distinction's absence
// surfaced and fixed) — here checked against whatever a byte-level parser
// can produce instead of only what the generator can construct.
func FuzzUnmarshalMessage(f *testing.F) {
	seed := func(m Message) {
		b, err := json.Marshal(m)
		if err != nil {
			f.Fatalf("seed fixture failed to marshal: %v", err)
		}
		f.Add(b)
	}
	// Happy path, mirroring TestMessageJSONRoundTrip's fixture: every part
	// type this package defines, in one message.
	seed(Message{
		ID:   "msg_1",
		Role: RoleAssistant,
		Parts: Parts{
			&Text{Text: "hello"},
			&Reasoning{
				Text:         "thinking summary",
				ProviderData: ProviderData{"anthropic": json.RawMessage(`{"signature":"abc"}`)},
			},
			&ToolCall{CallID: "tc_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
			&Blob{MediaType: "image/png", Data: []byte{0x89, 0x50}},
		},
		Model: ModelRef{Provider: "anthropic", Model: "claude-fable-5"},
	})
	seed(Message{
		ID:   "msg_2",
		Role: RoleTool,
		Parts: Parts{
			&ToolResult{
				CallID:  "tc_1",
				Content: Parts{&Text{Text: "output line"}, &Blob{MediaType: "image/png", URL: "https://example.com/x.png"}},
				IsError: true,
			},
		},
	})
	// The empty-non-nil / invalid-arguments shapes safeArguments and
	// Normalize exist to guard (message.go doc comments) — seeded here
	// pre-normalization, exactly as a hand-rolled or corrupted producer
	// might emit them.
	seed(Message{
		ID:   "msg_3",
		Role: RoleAssistant,
		Parts: Parts{
			&ToolCall{CallID: "tc_2", Name: "bash", Arguments: json.RawMessage(`{"command":"echo hel`)},
		},
	})
	seed(Message{
		ID:   "msg_4",
		Role: RoleAssistant,
		Parts: Parts{
			&Reasoning{Text: "t", ProviderData: ProviderData{"anthropic": json.RawMessage{}}},
		},
	})
	// Structural edge cases a byte-level fuzzer is well placed to find that
	// the seed fixtures above (all built via Marshal, so already canonical)
	// cannot produce on their own.
	f.Add([]byte(`{"id":"m","role":"assistant","parts":[{"type":"holo_deck"}]}`)) // unknown part type
	f.Add([]byte(`{"id":"m","role":"assistant","parts":null}`))
	f.Add([]byte(`{"id":"m","role":"assistant","parts":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))
	f.Add([]byte(`{"model":""}`))
	f.Add([]byte(`{"model":"/nomodel"}`))
	f.Add([]byte(`{"model":"noprovider/"}`))
	f.Add([]byte(`{"parts":[{"type":"tool_call","call_id":"c","name":"n","arguments":null}]}`))
	f.Add([]byte(`{"parts":[{"type":"text"}]}`))
	f.Add([]byte(`{"created_at":"not-a-time"}`))
	f.Add([]byte(`{"parts":"not-an-array"}`))
	f.Add([]byte(`[1,2,3]`))
	f.Add([]byte(`"just a string"`))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			t.Skip()
		}

		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			return // rejected input: a valid terminal outcome.
		}

		// Marshal must never error, Normalized or not: safeArguments and
		// ProviderData.MarshalJSON are defense-in-depth specifically so any
		// producer's Message marshals safely, per their own doc comments
		// ("even if a future producer bypasses Normalize entirely ... must
		// still never be able to make a marshal fail").
		if _, err := json.Marshal(m); err != nil {
			t.Fatalf("accepted input failed to marshal: %v\ninput: %s", err, data)
		}

		// The "retranscoding an unchanged history produces identical wire
		// requests" invariant (Normalize's doc comment) is promised for
		// messages that have gone through Normalize — the one ingest choke
		// point every persisted message passes through
		// (engine.Session.appendWithUsage) before its first marshal. A
		// message that never went through Normalize is explicitly allowed to
		// marshal differently across one reload (same doc: "Both shapes are
		// safe ... but they are not byte-identical"), so the stability check
		// below Normalizes first, matching what the package actually
		// promises — see message/properties_test.go's file doc comment for
		// the full account, including the real bug this exact distinction
		// surfaced and fixed.
		m.Normalize()
		raw1, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal after Normalize failed: %v\ninput: %s", err, data)
		}

		var reloaded Message
		if err := json.Unmarshal(raw1, &reloaded); err != nil {
			t.Fatalf("remarshal of a Normalized message failed to re-parse: %v\nfirst marshal: %s", err, raw1)
		}

		raw2, err := json.Marshal(reloaded)
		if err != nil {
			t.Fatalf("second remarshal failed: %v\nfirst marshal: %s", err, raw1)
		}

		if !bytes.Equal(raw1, raw2) {
			t.Fatalf("remarshal of a Normalized message is not stable:\n first: %s\nsecond: %s", raw1, raw2)
		}
	})
}
