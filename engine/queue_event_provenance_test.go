package engine

import (
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestEventPromptQueuedCarriesProvenance is the named-failure test for
// S3's gap: EventPromptQueued (the durable prompt.queued record's own
// live-event twin — see server/journal.go's publishQueue) carried NO
// provenance fields, so a consumer that reconciles from the event/journal
// stream alone (rather than a follow-up GET /session/{id}/queue) could not
// see who queued a prompt. Proves both EnqueuePrompt (plain, memory-then-
// disk) and EnqueuePromptDurable (durable, seq-gated) emit
// QueueSource/QueueSourceID/QueueSourceLabel, always Normalized.
func TestEventPromptQueuedCarriesProvenance(t *testing.T) {
	s, events := durableTestSession(t)

	if _, _, err := s.EnqueuePrompt("plain enqueue", "", PromptProvenance{
		Source: message.PromptSourceTyped,
	}); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}
	if _, _, err := s.EnqueuePromptDurable("durable enqueue", 1, PromptProvenance{
		Source: message.PromptSourceSchedule, SourceID: "sched_9", SourceLabel: "nightly",
	}); err != nil {
		t.Fatalf("EnqueuePromptDurable: %v", err)
	}

	var sawTyped, sawSchedule bool
	for _, ev := range *events {
		if ev.Type != EventPromptQueued {
			continue
		}
		switch ev.QueueText {
		case "plain enqueue":
			sawTyped = true
			if ev.QueueSource != string(message.PromptSourceTyped) {
				t.Errorf("plain enqueue QueueSource = %q, want %q", ev.QueueSource, message.PromptSourceTyped)
			}
		case "durable enqueue":
			sawSchedule = true
			if ev.QueueSource != string(message.PromptSourceSchedule) || ev.QueueSourceID != "sched_9" || ev.QueueSourceLabel != "nightly" {
				t.Errorf("durable enqueue queue provenance = source=%q source_id=%q source_label=%q, want schedule/sched_9/nightly",
					ev.QueueSource, ev.QueueSourceID, ev.QueueSourceLabel)
			}
		}
	}
	if !sawTyped {
		t.Fatal("no EventPromptQueued for the plain enqueue")
	}
	if !sawSchedule {
		t.Fatal("no EventPromptQueued for the durable enqueue")
	}
}

// TestEventPromptQueuedNormalizesUnlabeledSource proves an unlabeled
// caller's EventPromptQueued still carries a Normalized, non-empty
// QueueSource ("api") — mirroring queuedItemJSON/OperatorBatchEntry's own
// always-normalized Source, never an empty string a consumer would have
// to special-case.
func TestEventPromptQueuedNormalizesUnlabeledSource(t *testing.T) {
	s, events := durableTestSession(t)

	if _, _, err := s.EnqueuePrompt("unlabeled", "", PromptProvenance{}); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}

	var found bool
	for _, ev := range *events {
		if ev.Type == EventPromptQueued && ev.QueueText == "unlabeled" {
			found = true
			if ev.QueueSource != string(message.PromptSourceAPI) {
				t.Errorf("QueueSource = %q, want %q", ev.QueueSource, message.PromptSourceAPI)
			}
		}
	}
	if !found {
		t.Fatal("no EventPromptQueued for the unlabeled enqueue")
	}
}
