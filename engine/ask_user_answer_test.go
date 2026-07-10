package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestPromptClearsAwaitingQuestionAndPersistsAnswer is the red-first test
// for docs/design/question-tool.md §3: any new user message (a bare
// prompt_async, or POST /session/{id}/answer's interactive-branch delivery)
// clears s.awaitingQuestion and persists question.answered — Session.Prompt
// is the single, idempotent owner of that record for the interactive path.
func TestPromptClearsAwaitingQuestionAndPersistsAnswer(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			toolCall("tc1", "ask_user", `{"questions":[{"question":"Which environment?"}]}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "thanks, using staging"}),
	}}
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	if _, err := s.Prompt(context.Background(), "ask"); err != nil {
		t.Fatalf("Prompt 1: %v", err)
	}
	if _, ok := s.AwaitingQuestion(); !ok {
		t.Fatal("expected AwaitingQuestion after ask_user")
	}
	if _, err := s.Prompt(context.Background(), "staging"); err != nil {
		t.Fatalf("Prompt 2: %v", err)
	}
	if _, ok := s.AwaitingQuestion(); ok {
		t.Fatal("expected AwaitingQuestion cleared after the next prompt")
	}

	reloaded, err := LoadSession(Config{Providers: provider.Registry{"test": prov}, SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if _, ok := reloaded.AwaitingQuestion(); ok {
		t.Error("reloaded session should not be awaiting a question after question.answered replays")
	}
}

// TestAnswerQuestionAtomicClaim is the red-first test for the atomic
// check-persist-clear POST /answer's goal-paused branch uses (design doc
// §3): AnswerQuestion persists question.answered carrying the formatted
// answer text itself (not just the fact of answering) and clears
// awaitingQuestion, reporting ok only when callID matched a pending
// question.
func TestAnswerQuestionAtomicClaim(t *testing.T) {
	s := NewSession(Config{
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if ok, hadPending := s.AnswerQuestion("tc1", "x"); ok || hadPending {
		t.Errorf("AnswerQuestion with nothing pending = (%v, %v), want (false, false)", ok, hadPending)
	}
	s.runAskUser("tc1", []byte(`{"questions":[{"question":"q"}]}`))
	if ok, hadPending := s.AnswerQuestion("tc-stale", "x"); ok || !hadPending {
		t.Errorf("AnswerQuestion with a stale call_id = (%v, %v), want (false, true)", ok, hadPending)
	}
	if ok, hadPending := s.AnswerQuestion("tc1", "staging"); !ok || !hadPending {
		t.Errorf("AnswerQuestion with the correct call_id = (%v, %v), want (true, true)", ok, hadPending)
	}
	if _, awaiting := s.AwaitingQuestion(); awaiting {
		t.Error("AwaitingQuestion still true after a winning AnswerQuestion call")
	}
}

// TestLoadSessionReplaysQuestionAnswered proves question.answered clears
// awaitingQuestion on replay, same as recGoalCleared clears goalActive.
func TestLoadSessionReplaysQuestionAnswered(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{
		Providers:  provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	s.runAskUser("tc1", []byte(`{"questions":[{"question":"q"}]}`))
	if ok, _ := s.AnswerQuestion("tc1", "staging"); !ok {
		t.Fatal("AnswerQuestion should have won")
	}

	reloaded, err := LoadSession(Config{Providers: provider.Registry{"test": &scriptedProvider{name: "test"}}, SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if _, awaiting := reloaded.AwaitingQuestion(); awaiting {
		t.Error("reloaded session should not be awaiting after question.answered replays")
	}
}

// TestPendingResumeAnswerRebuiltOnCrash is the red-first test for the crash
// window docs/design/question-tool.md §3 calls out: "reload of an
// answered-but-never-resumed question rebuilds ResumeAnswer from the
// journal." A process that dies between POST /answer's atomic persist and
// PursueGoal's respawn leaves question.answered on disk with no subsequent
// message ever appended for it — reload must expose the answer text so a
// caller resuming the goal can rebuild GoalOptions.ResumeAnswer, rather than
// silently losing it and replaying the raw (unanswered) condition.
func TestPendingResumeAnswerRebuiltOnCrash(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{
		Providers:  provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	s.runAskUser("tc1", []byte(`{"questions":[{"question":"q"}]}`))
	// Simulate the goal-paused /answer branch: persist+clear via
	// AnswerQuestion directly, WITHOUT ever calling Prompt (the "crash
	// before PursueGoal resumed" case — no message record follows).
	if ok, _ := s.AnswerQuestion("tc1", "staging, please"); !ok {
		t.Fatal("AnswerQuestion should have won")
	}

	reloaded, err := LoadSession(Config{Providers: provider.Registry{"test": &scriptedProvider{name: "test"}}, SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	text, ok := reloaded.PendingResumeAnswer()
	if !ok || text != "staging, please" {
		t.Fatalf("PendingResumeAnswer() = (%q, %v), want (\"staging, please\", true)", text, ok)
	}
}

// TestPendingResumeAnswerClearedOnceDelivered proves the OTHER half: once
// the answer has actually been delivered as a message (the ordinary case,
// no crash), PendingResumeAnswer must report false — there is nothing left
// to rebuild.
func TestPendingResumeAnswerClearedOnceDelivered(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			toolCall("tc1", "ask_user", `{"questions":[{"question":"q"}]}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	if _, err := s.Prompt(context.Background(), "ask"); err != nil {
		t.Fatalf("Prompt 1: %v", err)
	}
	if _, err := s.Prompt(context.Background(), "staging"); err != nil {
		t.Fatalf("Prompt 2: %v", err)
	}

	reloaded, err := LoadSession(Config{Providers: provider.Registry{"test": prov}, SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if _, ok := reloaded.PendingResumeAnswer(); ok {
		t.Error("PendingResumeAnswer should be false once the answer was actually delivered as a message")
	}
}

// TestAnswerQuestionConcurrentExactlyOneWins is the red-first test for
// AnswerQuestion's atomicity claim (docs/design/question-tool.md §3): "two
// concurrent POST /answer ... could both observe the pending question and
// both re-spawn PursueGoal ... Exactly one /answer wins; the loser gets the
// same 409 a stale call_id gets." That guarantee is enforced entirely by
// AnswerQuestion's single locked check-persist-clear (see ask_user.go) —
// this exercises it directly, with real goroutines released by a shared
// start barrier (never a sleep), so the race is genuine, not simulated.
func TestAnswerQuestionConcurrentExactlyOneWins(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{
		Providers:  provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	s.runAskUser("tc1", []byte(`{"questions":[{"question":"which env?"}]}`))

	const n = 50
	start := make(chan struct{})
	results := make([]bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ok, _ := s.AnswerQuestion("tc1", "staging")
			results[i] = ok
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for _, ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("AnswerQuestion wins = %d across %d concurrent callers, want exactly 1", wins, n)
	}
	if _, awaiting := s.AwaitingQuestion(); awaiting {
		t.Error("question still awaiting after a winning AnswerQuestion call")
	}

	reloaded, err := LoadSession(Config{Providers: provider.Registry{"test": &scriptedProvider{name: "test"}}, SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if _, awaiting := reloaded.AwaitingQuestion(); awaiting {
		t.Error("reloaded session still awaiting a question after the concurrent race resolved it")
	}
}
