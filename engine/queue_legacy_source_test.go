package engine

import (
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestLegacyPromptQueuedRecordFoldsSourceAsAPI is the named-failure test
// for S4's legacy-record gap: a prompt.queued record written before this
// feature's Source field existed (no "source" key at all, exactly what
// every journal on disk before this PR looked like) must fold back reading
// as message.PromptSourceAPI, both on the in-memory QueuedPrompt
// (QueuedPrompts(), the source GET /session/{id}/queue reads) and on a
// later operator-batch drain's OperatorBatchEntry — never as an empty
// string a client would have to special-case.
func TestLegacyPromptQueuedRecordFoldsSourceAsAPI(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_0000000000000099"
	writeSessionLog(t, dir, id,
		`{"type":"session","id":"ses_0000000000000099","created_at":"2026-07-21T00:00:00Z"}`,
		`{"type":"model","model":"test/m1"}`,
		`{"type":"prompt.queued","prompt":{"id":1,"text":"legacy prompt"}}`,
	)
	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatal(err)
	}

	pending := s.QueuedPrompts()
	if len(pending) != 1 {
		t.Fatalf("QueuedPrompts = %+v, want exactly one legacy entry", pending)
	}
	if got := pending[0].Source; got != "" {
		t.Fatalf("legacy QueuedPrompt.Source = %q, want \"\" (unnormalized in memory)", got)
	}
	if got := pending[0].Source.Normalized(); got != message.PromptSourceAPI {
		t.Errorf("legacy QueuedPrompt.Source.Normalized() = %q, want %q", got, message.PromptSourceAPI)
	}

	entries := operatorBatchEntries(pending)
	if len(entries) != 1 {
		t.Fatalf("operatorBatchEntries = %+v, want exactly one entry", entries)
	}
	if entries[0].Source != message.PromptSourceAPI {
		t.Errorf("operatorBatchEntries[0].Source = %q, want %q (legacy record reads as an unlabeled caller)",
			entries[0].Source, message.PromptSourceAPI)
	}
}
