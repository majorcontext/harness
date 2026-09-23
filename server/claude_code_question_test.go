package server

import (
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// TestClaudeCodeQuestionAwaitsInputThenAnswers pins the frontend contract: a
// parked question must not read as a finished turn, it must name the call to
// answer, and POST .../answer must resume it.
func TestClaudeCodeQuestionAwaitsInputThenAnswers(t *testing.T) {
	bin := buildFakeClaudeForServer(t)
	dir := t.TempDir()
	t.Setenv("FAKE_CLAUDE_MODE", "question")
	t.Setenv("FAKE_CLAUDE_STATE", filepath.Join(dir, "parked"))
	t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(dir, "invocations.jsonl"))
	claudeModel := message.ModelRef{Provider: engine.ClaudeCodeProviderFamily, Model: "sonnet"}
	h := claudeCodeSwitchHarness(t, claudeModel, engine.ClaudeCodeConfig{BinaryPath: bin, AskUserQuestion: true}, &scriptedProvider{name: "codex"})
	id := h.createSession("")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "pick a db"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	waitIdleClaudeCode(t, h, id)
	if got := lastTurnForQuestion(t, h, id); got.Outcome != "awaiting_input" || got.QuestionCallID != "toolu_q" {
		t.Fatalf("parked last_turn = %+v, want outcome awaiting_input naming toolu_q", got)
	}

	resp, data = h.do("POST", "/session/"+id+"/question/toolu_q/answer", map[string]any{
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
