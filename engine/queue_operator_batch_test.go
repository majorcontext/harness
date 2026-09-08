package engine

import (
	"context"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestDrainQueuedPromptsIntoHistoryStampsOperatorBatch is the named-
// failure test for the console mis-split bug (a batch delivered as one
// "OPERATOR MESSAGES" user message with no structured boundary and no
// distinguishing Origin, forcing a client to guess prompt boundaries by
// scanning for "\nN. " — which misparses a prompt whose own text embeds a
// numbered list). It drives the SAME mid-turn drain
// TestMidTurnInjectionAtToolBoundary already proves the timing of
// (drainQueuedPromptsIntoHistory, engine.go), but asserts on the
// STRUCTURED shape instead of the rendered text: the appended message's
// Origin must be OriginOperatorBatch (never empty, never OriginClaudeCode)
// and its OperatorBatch must carry one entry per queued prompt, in order,
// each with its own provenance — not folded into the rendered text at
// all. A regression that stops stamping Origin, or stops attaching
// OperatorBatch, fails this test on that exact missing field.
func TestDrainQueuedPromptsIntoHistoryStampsOperatorBatch(t *testing.T) {
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

	// First prompt: no source named — must fold to PromptSourceAPI, never
	// PromptSourceTyped (an untagged caller is never presented as human).
	id1, _, err := s.EnqueuePrompt("first operator prompt", "")
	if err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}
	// Second prompt: an explicit schedule delivery, the shape the boxes
	// control plane's schedule_task/cron worker asserts.
	id2, _, err := s.EnqueuePromptFrom("second operator prompt", "", PromptProvenance{
		Source:      message.PromptSourceSchedule,
		SourceID:    "sched_123",
		SourceLabel: "nightly CI check",
	})
	if err != nil {
		t.Fatalf("EnqueuePromptFrom: %v", err)
	}

	close(release)

	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.msg.Parts.Text() != "final" {
		t.Errorf("final = %q", out.msg.Parts.Text())
	}

	// Find the batch message the drain appended into history.
	var batch *message.Message
	for _, m := range s.History() {
		if m.Origin == message.OriginOperatorBatch {
			m := m
			batch = &m
		}
	}
	if batch == nil {
		t.Fatalf("no history message carries Origin=%q; history = %+v", message.OriginOperatorBatch, s.History())
	}

	want := []message.OperatorBatchEntry{
		{EnqueueID: id1, Text: "first operator prompt", Source: message.PromptSourceAPI},
		{
			EnqueueID: id2, Text: "second operator prompt", Source: message.PromptSourceSchedule,
			SourceID: "sched_123", SourceLabel: "nightly CI check",
		},
	}
	if len(batch.OperatorBatch) != len(want) {
		t.Fatalf("OperatorBatch = %+v, want %d entries: %+v", batch.OperatorBatch, len(want), want)
	}
	for i, e := range want {
		if batch.OperatorBatch[i] != e {
			t.Errorf("OperatorBatch[%d] = %+v, want %+v", i, batch.OperatorBatch[i], e)
		}
	}
}
