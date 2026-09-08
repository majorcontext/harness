package engine

import (
	"context"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestOperatorBatchMessageSurvivesReplay is the named-failure test for a
// gap the adversarial review on this feature found: nothing pinned that a
// journal replay (LoadSession, the path a resumed or reloaded session
// takes) reconstructs an operator-batch message's Origin/OperatorBatch
// fields identically to a live session's own in-memory copy. record.Message
// embeds *message.Message directly (engine/store.go), so this SHOULD hold
// for free via encoding/json — this test proves it, rather than leaving it
// merely proven-by-inspection.
//
// Drives the same mid-turn drain TestDrainQueuedPromptsIntoHistoryStampsOperatorBatch
// does, then reloads the session from its own durable log and compares the
// reloaded history's batch message against the live one field for field.
func TestOperatorBatchMessageSurvivesReplay(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "gate", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "final"}),
	}}
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		System:     []string{"base"},
		SessionDir: dir,
		Tools:      []Tool{gateTool(entered, release)},
	})

	type outcome struct {
		msg *message.Message
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		m, err := s.Prompt(context.Background(), "please run gate")
		done <- outcome{m, err}
	}()

	<-entered

	if _, _, err := s.EnqueuePrompt("first operator prompt", "", PromptProvenance{}); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}
	if _, _, err := s.EnqueuePrompt("second operator prompt", "", PromptProvenance{
		Source:      message.PromptSourceSchedule,
		SourceID:    "sched_replay",
		SourceLabel: "nightly replay check",
	}); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}

	close(release)
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}

	var live *message.Message
	for _, m := range s.History() {
		if m.Origin == message.OriginOperatorBatch {
			m := m
			live = &m
		}
	}
	if live == nil {
		t.Fatalf("no live history message carries Origin=%q", message.OriginOperatorBatch)
	}
	if len(live.OperatorBatch) != 2 {
		t.Fatalf("live OperatorBatch = %+v, want 2 entries", live.OperatorBatch)
	}

	reloaded, err := LoadSession(Config{
		Providers:  provider.Registry{"test": prov},
		System:     []string{"base"},
		SessionDir: dir,
	}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	var replayed *message.Message
	for _, m := range reloaded.History() {
		if m.Origin == message.OriginOperatorBatch {
			m := m
			replayed = &m
		}
	}
	if replayed == nil {
		t.Fatalf("no replayed history message carries Origin=%q; history = %+v", message.OriginOperatorBatch, reloaded.History())
	}
	if replayed.Origin != live.Origin {
		t.Errorf("replayed Origin = %q, want %q (live)", replayed.Origin, live.Origin)
	}
	if len(replayed.OperatorBatch) != len(live.OperatorBatch) {
		t.Fatalf("replayed OperatorBatch = %+v, want %+v (live)", replayed.OperatorBatch, live.OperatorBatch)
	}
	for i, want := range live.OperatorBatch {
		if replayed.OperatorBatch[i] != want {
			t.Errorf("replayed OperatorBatch[%d] = %+v, want %+v (live)", i, replayed.OperatorBatch[i], want)
		}
	}
	// Pin the exact reconstructed shape too, not only "replayed == live":
	// a bug that corrupted BOTH identically would pass the comparison
	// above but still be wrong.
	want := message.OperatorBatchEntry{
		EnqueueID: 2, Text: "second operator prompt", Source: message.PromptSourceSchedule,
		SourceID: "sched_replay", SourceLabel: "nightly replay check",
	}
	if replayed.OperatorBatch[1] != want {
		t.Errorf("replayed OperatorBatch[1] = %+v, want %+v", replayed.OperatorBatch[1], want)
	}
}

// TestSoloMessageProvenanceSurvivesReplay proves a solo (non-batched)
// message's own Source/SourceID/SourceLabel — Message-level fields, not
// OperatorBatchEntry's — replay identically via LoadSession. A caller
// dispatched through EnqueuePrompt with explicit provenance, drained on
// its own dequeue (never batched — QueuedPrompts() drains one at a time
// via the SessionManager path, so this drives the field directly instead).
func TestSoloMessageProvenanceSurvivesReplay(t *testing.T) {
	dir := t.TempDir()
	scripted := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ack"}),
	}}
	s := NewSession(Config{
		Providers:  provider.Registry{"test": scripted},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		System:     []string{"base"},
		SessionDir: dir,
	})

	msg, err := s.PromptWithOriginFrom(context.Background(), "solo prompt", "", "", PromptProvenance{
		Source:      message.PromptSourceTyped,
		SourceID:    "console-1",
		SourceLabel: "web console",
	})
	if err != nil {
		t.Fatalf("PromptWithOriginFrom: %v", err)
	}
	_ = msg

	var live *message.Message
	for _, m := range s.History() {
		if m.Role == message.RoleUser && m.Parts.Text() == "solo prompt" {
			m := m
			live = &m
		}
	}
	if live == nil {
		t.Fatal("no live history message carries the solo prompt text")
	}
	if live.Source != message.PromptSourceTyped || live.SourceID != "console-1" || live.SourceLabel != "web console" {
		t.Fatalf("live message provenance = %+v, want source=typed source_id=console-1 source_label=%q", live, "web console")
	}

	reloaded, err := LoadSession(Config{
		Providers:  provider.Registry{"test": scripted},
		System:     []string{"base"},
		SessionDir: dir,
	}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	var replayed *message.Message
	for _, m := range reloaded.History() {
		if m.Role == message.RoleUser && m.Parts.Text() == "solo prompt" {
			m := m
			replayed = &m
		}
	}
	if replayed == nil {
		t.Fatalf("no replayed history message carries the solo prompt text; history = %+v", reloaded.History())
	}
	if replayed.Source != live.Source || replayed.SourceID != live.SourceID || replayed.SourceLabel != live.SourceLabel {
		t.Errorf("replayed provenance = %+v, want %+v (live)", replayed, live)
	}
}
