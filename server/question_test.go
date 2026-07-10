package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// askUserTurn builds a scripted worker turn whose assistant message is a
// single ask_user tool call, StopToolUse — the shape a scriptedProvider (or
// goalProv's worker script) needs to exercise the ask_user pause end to
// end, without depending on engine package internals.
func askUserTurn(callID, questionsJSON string) []provider.Event {
	msg := &message.Message{
		ID:   "msg_ask_" + callID,
		Role: message.RoleAssistant,
		Parts: message.Parts{&message.ToolCall{
			CallID:    callID,
			Name:      "ask_user",
			Arguments: json.RawMessage(questionsJSON),
		}},
	}
	return []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopToolUse}}
}

// sessionQuestionView decodes the Session JSON fields these tests need.
type sessionQuestionView struct {
	State    string `json:"state"`
	Question *struct {
		CallID    string `json:"call_id"`
		Questions []struct {
			Question string   `json:"question"`
			Options  []string `json:"options,omitempty"`
			Multi    bool     `json:"multi,omitempty"`
		} `json:"questions"`
	} `json:"question"`
	Goal *struct {
		Active bool `json:"active"`
	} `json:"goal"`
	LastTurn *lastTurnJSONForTest `json:"last_turn"`
}

func (h *harness) getSessionQuestion(id string) sessionQuestionView {
	h.t.Helper()
	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var v sessionQuestionView
	if err := json.Unmarshal(data, &v); err != nil {
		h.t.Fatal(err)
	}
	return v
}

// TestGoalPausesOnAskUserOutcome is the red-first test for docs/design/
// question-tool.md §2/§3's server-side plumbing: runGoal must record a
// distinct last_turn.outcome ("awaiting_input") for a goal that paused on
// ask_user — never "completed", never max_turns_exceeded — and the
// session's composite state must read "awaiting-input" (outranking
// goal-running), with the pending question surfaced on Session.question.
func TestGoalPausesOnAskUserOutcome(t *testing.T) {
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{askUserTurn("tc1", `{"questions":[{"question":"Which environment?"}]}`)},
		// No evaluator turns scripted: evaluateGoal must never be called.
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "deploy the service"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}

	evs := sse.collectUntilIdle(t)
	var end *Event
	var sawEval bool
	for i := range evs {
		if evs[i].Type == "turn.end" {
			end = &evs[i]
		}
		if evs[i].Type == "goal.eval" {
			sawEval = true
		}
	}
	if sawEval {
		t.Error("goal.eval observed while paused on ask_user, want the evaluator never invoked")
	}
	if end == nil {
		t.Fatal("no turn.end record for a goal paused on ask_user")
	}
	if end.Outcome != "awaiting_input" {
		t.Errorf("turn.end outcome = %q, want awaiting_input", end.Outcome)
	}

	sess := h.getSessionQuestion(id)
	if sess.State != "awaiting-input" {
		t.Errorf("session state = %q, want awaiting-input (outranks goal-running)", sess.State)
	}
	if sess.Goal == nil || !sess.Goal.Active {
		t.Errorf("goal = %+v, want active (a pause is not a clear)", sess.Goal)
	}
	if sess.Question == nil || sess.Question.CallID != "tc1" {
		t.Errorf("question = %+v, want CallID tc1", sess.Question)
	}
	if sess.LastTurn == nil || sess.LastTurn.Outcome != "awaiting_input" {
		t.Errorf("last_turn = %+v, want outcome awaiting_input", sess.LastTurn)
	}
}
