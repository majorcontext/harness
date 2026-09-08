package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestGoalTurnBoundaryDrainStampsOperatorBatch is the named-failure test
// for the THIRD operatorMessagesBlock producer's own gap: goal.go's
// PursueGoal turn-boundary drain (operatorContextGoal) prepends the
// rendered "OPERATOR MESSAGES (... continue the goal)" block to the next
// turn's directive, but — unlike the two operatorContextTask drain sites
// (engine.go's drainQueuedPromptsIntoHistory and engine/
// claude_code_backend.go's delegated equivalent) — used to send that text
// through PromptWithOrigin's plain, origin-less path: the appended message
// carried NEITHER Origin=OriginOperatorBatch NOR a structured OperatorBatch
// list, only the same ambiguous rendered text a client would have to
// "\nN. "-scan to split, misparsing a queued prompt whose own body embeds
// a numbered list — the exact class of bug this whole feature exists to
// close, left open for every goal-supervised session (which boxes
// dispatches routinely).
//
// Reuses TestGoalInjectsQueuedPromptsAtBoundary's exact provider/timing
// rig (blockingFirstWorkerProvider) so this test observes the SAME
// turn-2-directive delivery that test already proves happens, but asserts
// on the durable history message's structured fields instead of the
// rendered text.
func TestGoalTurnBoundaryDrainStampsOperatorBatch(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	prov := &blockingFirstWorkerProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1 done"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 2 done"}),
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: keep going"),
			evalTurn("MET: looks done"),
		},
		entered: entered,
		release: release,
	}
	s := goalSession(t, prov, dir)

	type outcome struct {
		res *GoalResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.PursueGoal(context.Background(), "the condition", GoalOptions{Evaluator: evalModel, MaxTurns: 2})
		done <- outcome{res, err}
	}()

	<-entered // turn 1's worker call is genuinely in flight

	// No source named — must fold to PromptSourceAPI.
	id1, _, err := s.EnqueuePrompt("first operator message", "", PromptProvenance{})
	if err != nil {
		t.Fatalf("EnqueuePrompt = %v", err)
	}
	// An explicit schedule delivery, the shape the boxes control plane's
	// schedule_task/cron worker asserts.
	id2, _, err := s.EnqueuePrompt("second operator message", "", PromptProvenance{
		Source:      message.PromptSourceSchedule,
		SourceID:    "sched_456",
		SourceLabel: "nightly goal check",
	})
	if err != nil {
		t.Fatalf("EnqueuePrompt = %v", err)
	}

	releaseOnce.Do(func() { close(release) }) // let turn 1 complete

	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if !out.res.Achieved || out.res.Turns != 2 {
		t.Fatalf("result = %+v, want achieved in 2 turns", out.res)
	}

	// Find turn 2's directive message in durable history: the one whose
	// text carries the goal wording, appended after both enqueues above.
	var batch *message.Message
	for _, m := range s.History() {
		if m.Role == message.RoleUser && strings.Contains(m.Parts.Text(), "continue the goal") {
			m := m
			batch = &m
		}
	}
	if batch == nil {
		t.Fatalf("no history message contains the goal directive's operator block; history = %+v", s.History())
	}
	if batch.Origin != message.OriginOperatorBatch {
		t.Fatalf("turn 2 directive message Origin = %q, want %q", batch.Origin, message.OriginOperatorBatch)
	}

	want := []message.OperatorBatchEntry{
		{EnqueueID: id1, Text: "first operator message", Source: message.PromptSourceAPI},
		{
			EnqueueID: id2, Text: "second operator message", Source: message.PromptSourceSchedule,
			SourceID: "sched_456", SourceLabel: "nightly goal check",
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

	// The directive's OWN goal condition/guidance text must still follow
	// the block, untouched by this stamping — the message's OperatorBatch
	// entries cover only the two queued prompts, never the trailing
	// directive text itself (see operatorBatchDrain's own doc comment).
	if !strings.Contains(batch.Parts.Text(), "keep going") {
		t.Errorf("turn 2 directive text = %q, want the guidance text to still follow the operator block", batch.Parts.Text())
	}
}
