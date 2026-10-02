package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestClaudeCodeQuestionAwaitsInputThenAnswers pins the frontend contract: a
// parked question must not read as a finished turn, it must name the call to
// answer, and POST .../answer must resume it.
func TestClaudeCodeQuestionAwaitsInputThenAnswers(t *testing.T) {
	h, id := claudeCodeQuestionHarness(t, &scriptedProvider{name: "codex"})

	promptAndWait(t, h, id, "pick a db")
	if got := lastTurnForQuestion(t, h, id); got.Outcome != "awaiting_input" || got.QuestionCallID != "toolu_q" {
		t.Fatalf("parked last_turn = %+v, want outcome awaiting_input naming toolu_q", got)
	}

	resp, data := h.do("POST", "/session/"+id+"/question/toolu_q/answer", map[string]any{
		"answers": map[string]string{"Which database?": "SQLite"},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("answer status %d: %s", resp.StatusCode, data)
	}
	waitIdleClaudeCode(t, h, id)
	if got := lastTurnForQuestion(t, h, id); got.Outcome != "completed" || got.QuestionCallID != "" {
		t.Errorf("answered last_turn = %+v, want outcome completed", got)
	}
}

// claudeCodeQuestionHarness builds a server whose default model parks
// questions through the fakeclaude stand-in, with nativeProv registered for
// a later POST /session/{id}/model switch.
func claudeCodeQuestionHarness(t *testing.T, nativeProv provider.Provider) (*harness, string) {
	t.Helper()
	bin := buildFakeClaudeForServer(t)
	dir := t.TempDir()
	t.Setenv("FAKE_CLAUDE_MODE", "question")
	t.Setenv("FAKE_CLAUDE_STATE", filepath.Join(dir, "parked"))
	t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(dir, "invocations.jsonl"))
	claudeModel := message.ModelRef{Provider: engine.ClaudeCodeProviderFamily, Model: "sonnet"}
	h := claudeCodeSwitchHarness(t, claudeModel, engine.ClaudeCodeConfig{BinaryPath: bin, AskUserQuestion: true}, nativeProv, 0)
	return h, h.createSession("")
}

func promptAndWait(t *testing.T, h *harness, id, text string) {
	t.Helper()
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async %q status %d: %s", text, resp.StatusCode, data)
	}
	waitIdleClaudeCode(t, h, id)
}

func lastTurnForQuestion(t *testing.T, h *harness, id string) struct{ Outcome, QuestionCallID string } {
	t.Helper()
	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var got struct {
		LastTurn struct {
			Outcome        string `json:"outcome"`
			QuestionCallID string `json:"question_call_id"`
		} `json:"last_turn"`
	}
	mustUnmarshal(t, data, &got)
	return struct{ Outcome, QuestionCallID string }{got.LastTurn.Outcome, got.LastTurn.QuestionCallID}
}

// TestClaudeCodeQuestionAfterNativeSwitchIsNotAwaitingInput pins the
// misclassification: a parked question stays pending across a switch to a
// native model, so runTurn reported the next, unrelated native turn as
// awaiting_input and the answer route started a doomed AnswerQuestion.
func TestClaudeCodeQuestionAfterNativeSwitchIsNotAwaitingInput(t *testing.T) {
	h, id := claudeCodeQuestionHarness(t, &scriptedProvider{name: "codex", turns: [][]provider.Event{asstTurn("done on codex")}})

	promptAndWait(t, h, id, "pick a db")
	if resp, data := h.do("POST", "/session/"+id+"/model", map[string]string{"model": "codex/gpt-5.6-sol"}); resp.StatusCode != 200 {
		t.Fatalf("set model status %d: %s", resp.StatusCode, data)
	}
	promptAndWait(t, h, id, "carry on")
	if got := lastTurnForQuestion(t, h, id); got.Outcome != "completed" || got.QuestionCallID != "" {
		t.Errorf("native turn last_turn = %+v, want outcome completed naming no question", got)
	}
	resp, data := h.do("POST", "/session/"+id+"/question/toolu_q/answer", map[string]any{
		"answers": map[string]string{"Which database?": "SQLite"},
	})
	if resp.StatusCode != 409 {
		t.Errorf("answer while the model is native: status %d: %s, want 409", resp.StatusCode, data)
	}
}

// TestClaudeCodeQuestionAnswerBodyIsBounded pins the unbounded decode: the
// answer route read r.Body with no limit, so a parked question accepted an
// arbitrarily large answer and forwarded it to the child as one frame.
func TestClaudeCodeQuestionAnswerBodyIsBounded(t *testing.T) {
	h, id := claudeCodeQuestionHarness(t, &scriptedProvider{name: "codex"})

	promptAndWait(t, h, id, "pick a db")
	resp, data := h.do("POST", "/session/"+id+"/question/toolu_q/answer", map[string]any{
		"answers": map[string]string{"Which database?": strings.Repeat("x", answerRequestMaxBytes)},
	})
	if resp.StatusCode != 413 {
		t.Fatalf("oversize answer status %d: %s, want 413", resp.StatusCode, data)
	}
	if got := lastTurnForQuestion(t, h, id); got.Outcome != "awaiting_input" || got.QuestionCallID != "toolu_q" {
		t.Errorf("last_turn after a rejected answer = %+v, want the question still parked", got)
	}
}
