package message

import (
	"encoding/json"
	"testing"
	"time"
)

// TestOperatorBatchMessageWireShape is the golden wire-shape test for a
// batch-delivered message: it pins the exact field names a client (boxes'
// console) reads to reconstruct prompt boundaries structurally instead of
// parsing the rendered "OPERATOR MESSAGES" text for a "\nN. " marker — the
// bug this feature closes (a prompt whose own text embeds a numbered list
// misparses under the old heuristic). Named failure: an agent that renames
// a JSON tag, reorders fields into the wrong Go type, or forgets
// omitempty on an optional field breaks this test with a diff naming the
// exact field — never a passing test that silently drifted.
func TestOperatorBatchMessageWireShape(t *testing.T) {
	createdAt := time.Date(2026, 9, 8, 17, 27, 0, 0, time.UTC)
	msg := Message{
		ID:        "msg_01m210y3yvfmhtykzhd9j6gs2w",
		Role:      RoleUser,
		Parts:     Parts{&Text{Text: "OPERATOR MESSAGES (address these, then continue the task):\n1. first\n2. second\n"}},
		CreatedAt: createdAt,
		Origin:    OriginOperatorBatch,
		OperatorBatch: []OperatorBatchEntry{
			{
				EnqueueID:       1,
				Text:            "first",
				Source:          PromptSourceAPI,
				AttachmentCount: 0,
			},
			{
				EnqueueID:       2,
				Text:            "second",
				Source:          PromptSourceSchedule,
				SourceID:        "sched_123",
				SourceLabel:     "nightly CI check",
				AttachmentCount: 1,
			},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	const want = `{"id":"msg_01m210y3yvfmhtykzhd9j6gs2w","role":"user",` +
		`"parts":[{"type":"text","text":"OPERATOR MESSAGES (address these, then continue the task):\n1. first\n2. second\n"}],` +
		`"created_at":"2026-09-08T17:27:00Z",` +
		`"origin":"operator_batch",` +
		`"operator_batch":[` +
		`{"enqueue_id":1,"text":"first","source":"api"},` +
		`{"enqueue_id":2,"text":"second","source":"schedule","source_id":"sched_123","source_label":"nightly CI check","attachment_count":1}` +
		`]}`

	if string(data) != want {
		t.Fatalf("Marshal(msg) =\n%s\nwant\n%s", data, want)
	}

	// Round-trip: Unmarshal must reconstruct the same OperatorBatch, field
	// for field — a client relies on this to read the batch back exactly.
	var out Message
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(out.OperatorBatch) != 2 {
		t.Fatalf("OperatorBatch after round trip = %+v, want 2 entries", out.OperatorBatch)
	}
	if out.OperatorBatch[0] != msg.OperatorBatch[0] || out.OperatorBatch[1] != msg.OperatorBatch[1] {
		t.Fatalf("OperatorBatch after round trip = %+v, want %+v", out.OperatorBatch, msg.OperatorBatch)
	}
	if out.Origin != OriginOperatorBatch {
		t.Fatalf("Origin after round trip = %q, want %q", out.Origin, OriginOperatorBatch)
	}
}

// TestPromptSourceNormalized is the named-failure test for
// PromptSource.Normalized's one rule: an absent/empty source must default
// to PromptSourceAPI, never PromptSourceTyped — an untagged caller must
// never be presented as a human. A regression that flips the default
// (or that stops normalizing at all) fails this test on the exact wrong
// value, not a generic mismatch.
func TestPromptSourceNormalized(t *testing.T) {
	if got := PromptSource("").Normalized(); got != PromptSourceAPI {
		t.Errorf("PromptSource(\"\").Normalized() = %q, want %q", got, PromptSourceAPI)
	}
	for _, s := range []PromptSource{PromptSourceTyped, PromptSourceAPI, PromptSourceSchedule, PromptSourceTask, PromptSourceCrossBox} {
		if got := s.Normalized(); got != s {
			t.Errorf("PromptSource(%q).Normalized() = %q, want unchanged %q", s, got, s)
		}
	}
}
