package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestGoalWorkerTurnInheritsMidTurnInjection proves the goal loop's worker
// turns inherit the tool-call-boundary injection wired into Session.Prompt
// (engine.go) automatically, since PursueGoal drives every worker turn
// through the ordinary Prompt loop (see promptTurnWithRetry). A prompt
// queued while the goal's FIRST worker turn is mid-tool-call must be
// delivered inside that SAME worker turn's next provider request — not wait
// for the goal's own turn-boundary drain (goal.go, which only runs at the
// TOP of the next PursueGoal iteration and would otherwise never see this
// prompt, since the goal achieves on turn 1 here and there is no turn 2).
//
// This also proves no double delivery across the two drain sites: only one
// prompt.dequeued(injected) record is ever journaled for the one enqueued
// prompt, even though both the mid-turn drain (engine.go) and the goal-
// boundary drain (goal.go, at the top of every iteration) run
// DequeueAllPrompts("injected") against the same queue.
func TestGoalWorkerTurnInheritsMidTurnInjection(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})

	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopToolUse, toolCall("tc1", "gate", `{}`)),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1 done"}),
		},
		eval: [][]provider.Event{
			evalTurn("MET: looks done"),
		},
	}
	s := goalSession(t, prov, dir)
	s.tools["gate"] = gateTool(entered, release)

	type outcome struct {
		res *GoalResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.PursueGoal(context.Background(), "the condition", GoalOptions{Evaluator: evalModel, MaxTurns: 1})
		done <- outcome{res, err}
	}()

	<-entered // turn 1's tool call is genuinely executing

	if _, err := s.EnqueuePrompt("operator mid worker tool"); err != nil {
		t.Fatalf("EnqueuePrompt = %v", err)
	}
	close(release)

	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if !out.res.Achieved || out.res.Turns != 1 {
		t.Fatalf("result = %+v, want achieved in 1 turn", out.res)
	}

	if pending := s.QueuedPrompts(); len(pending) != 0 {
		t.Fatalf("QueuedPrompts after goal completion = %+v, want empty", pending)
	}

	// Requests in order: worker call 1 (tool call), worker call 2 (final,
	// carries the injection), evaluator call.
	if len(prov.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3 (worker x2 + eval)", len(prov.requests))
	}
	worker2 := prov.requests[1]
	last := worker2.Messages[len(worker2.Messages)-1]
	if last.Role != message.RoleUser {
		t.Fatalf("worker call 2's trailing message role = %s, want user (the injected operator block)", last.Role)
	}
	text := last.Parts.Text()
	if !strings.Contains(text, "OPERATOR MESSAGES") || !strings.Contains(text, "operator mid worker tool") {
		t.Fatalf("worker call 2's trailing message = %q, want the labeled operator block with the queued text", text)
	}
	// This injection came from engine.go's tool-call-boundary drain (this
	// test's whole point is that a goal's worker turn inherits it
	// automatically), not goal.go's own turn-boundary drain — so it must
	// use the plain-turn "task" wording, never "goal", even though the
	// enclosing loop is PursueGoal (see operatorMessagesBlock, queue.go).
	if !strings.Contains(text, "continue the task") {
		t.Errorf("worker call 2's trailing message = %q, want plain-turn wording (continue the task)", text)
	}
	if strings.Contains(text, "continue the goal") {
		t.Errorf("worker call 2's trailing message = %q, must not reference a goal (this is engine.go's call site)", text)
	}

	// The evaluator's own request must never see the raw queue mechanics —
	// only the condition, same as any other goal-boundary injection.
	evalReq := prov.requests[2]
	evalContent := evalReq.Messages[0].Parts.Text()
	if !strings.HasPrefix(evalContent, "GOAL CONDITION:\nthe condition\n\n") {
		t.Errorf("evaluator request's GOAL CONDITION section = %q, want the unchanged condition only", evalContent)
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if n := strings.Count(log, `"type":"prompt.dequeued"`); n != 1 {
		t.Fatalf("prompt.dequeued record count = %d, want exactly 1 (delivered once, no double delivery across the two drain sites)", n)
	}
	if !strings.Contains(log, `"reason":"injected"`) {
		t.Fatalf("log missing reason:injected: %s", log)
	}
}
