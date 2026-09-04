package server

import (
	"testing"

	"github.com/majorcontext/harness/provider"
)

// TestTranscriptSeqs_ParallelToMessages pins the contract
// docs/design/transcript-tail-seqs.md states: GET
// /session/{id}/message?stream_from=1 answers a `seqs` array, parallel to
// `messages`, one durable journal seq per entry, so a caller that trims
// this history down to a shorter tail (meetneptune/boxes's byte-budget
// console-bootstrap read) can still learn which durable seq its own kept
// window starts at.
//
// Before this change transcriptJSON carried no such field at all, so a
// client decoding it never saw `seqs` — this test's failure mode without
// the fix is exactly that: len(got.Seqs) == 0 for a session with three
// messages.
func TestTranscriptSeqs_ParallelToMessages(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("one"), asstTurn("two"), asstTurn("three"),
	}})
	id := h.createSession("")

	for i := 0; i < 3; i++ {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "go"}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
		}
		h.waitIdle(id)
	}

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET transcript = %d: %s", meta.status, meta.body)
	}

	// One user + one assistant message per turn: 6 total.
	if len(got.Messages) != 6 {
		t.Fatalf("len(messages) = %d, want 6", len(got.Messages))
	}
	if len(got.Seqs) != len(got.Messages) {
		t.Fatalf("len(seqs) = %d, want %d (parallel to messages)", len(got.Seqs), len(got.Messages))
	}
	for i, seq := range got.Seqs {
		if seq <= 0 {
			t.Errorf("seqs[%d] = %d, want a positive durable seq for message %q", i, seq, got.Messages[i].ID)
		}
		if i > 0 && seq <= got.Seqs[i-1] {
			t.Errorf("seqs[%d] = %d, want strictly greater than seqs[%d] = %d (messages append in order)", i, seq, i-1, got.Seqs[i-1])
		}
	}
	if got.Seqs[len(got.Seqs)-1] != got.StreamFrom {
		t.Errorf("seqs[last] = %d, want == stream_from %d (the watermark is the newest message's own seq here)", got.Seqs[len(got.Seqs)-1], got.StreamFrom)
	}
}
