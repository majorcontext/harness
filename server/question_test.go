package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
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

// TestWaitUntilAwaitingInputResolvesOnQuestion is the red-first test for
// docs/design/question-tool.md §3's GET /session/{id}/wait addition:
// until=awaiting-input resolves once a pending ask_user question appears
// (the park point a consumer loop actually needs — wait -> POST /answer).
func TestWaitUntilAwaitingInputResolvesOnQuestion(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		askUserTurn("tc1", `{"questions":[{"question":"Which environment?"}]}`),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "help me deploy"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}

	resp, data = h.do("GET", "/session/"+id+"/wait?until=awaiting-input&timeout_s=5", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait until=awaiting-input status %d: %s", resp.StatusCode, data)
	}
	var wait struct {
		State    string `json:"state"`
		Question *struct {
			CallID string `json:"call_id"`
		} `json:"question"`
	}
	if err := json.Unmarshal(data, &wait); err != nil {
		t.Fatal(err)
	}
	if wait.State != "awaiting-input" {
		t.Errorf("wait response state = %q, want awaiting-input: %s", wait.State, data)
	}
	if wait.Question == nil || wait.Question.CallID != "tc1" {
		t.Errorf("wait response question = %+v, want CallID tc1: %s", wait.Question, data)
	}
}

// TestWaitRejectsBadUntilMentionsAwaitingInput proves the until validation
// error string was updated to name the new value.
func TestWaitRejectsBadUntilMentionsAwaitingInput(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")
	resp, data := h.do("GET", "/session/"+id+"/wait?until=bogus", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "awaiting-input") {
		t.Errorf("error body = %s, want it to mention awaiting-input", data)
	}
}

// TestPromptAsyncEndsAtAskUser proves the interactive (no-goal) path: a
// prompt_async turn that calls ask_user ends the turn, journals
// question.asked (durable, SSE-visible), and the session's composite state
// and Question field reflect it — "awaiting-input" outranks plain "busy"/
// "idle".
func TestPromptAsyncEndsAtAskUser(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		askUserTurn("tc1", `{"questions":[{"question":"Which environment?","options":["staging","prod"]}]}`),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "help me deploy"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	evs := sse.collectUntilIdle(t)
	var sawAsked bool
	for _, ev := range evs {
		if ev.Type == "question.asked" {
			sawAsked = true
		}
	}
	if !sawAsked {
		t.Errorf("no question.asked event journaled: %v", evs)
	}
	sess := h.getSessionQuestion(id)
	if sess.State != "awaiting-input" {
		t.Errorf("state = %q, want awaiting-input", sess.State)
	}
	if sess.Question == nil || sess.Question.CallID != "tc1" {
		t.Errorf("question = %+v, want CallID tc1", sess.Question)
	}
}

// TestAnswerInteractiveDeliversPrompt proves the no-active-goal branch:
// /answer formats the answers into one text block and delivers it through
// the ordinary Session.Prompt path — Session.Prompt's own clear-and-persist
// is the single owner of question.answered here, not the handler.
func TestAnswerInteractiveDeliversPrompt(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		askUserTurn("tc1", `{"questions":[{"question":"Which environment?","options":["staging","prod"]}]}`),
		asstTurn("noted, deploying to staging"),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "help me deploy"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t)

	resp, data = h.do("POST", "/session/"+id+"/answer", map[string]any{
		"call_id": "tc1",
		"answers": []map[string]any{{"question": "Which environment?", "selected": []string{"staging"}}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer status %d: %s", resp.StatusCode, data)
	}
	evs := sse.collectUntilIdle(t)
	var sawAnswered int
	for _, ev := range evs {
		if ev.Type == "question.answered" {
			sawAnswered++
		}
	}
	if sawAnswered != 1 {
		t.Errorf("question.answered records = %d, want exactly 1: %v", sawAnswered, evs)
	}
	sess := h.getSessionQuestion(id)
	if sess.Question != nil {
		t.Errorf("question = %+v, want nil after answering", sess.Question)
	}
	if sess.State != "idle" {
		t.Errorf("state = %q, want idle", sess.State)
	}

	resp, data = h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message status %d: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "staging") {
		t.Errorf("message history missing the delivered answer text: %s", data)
	}
}

// TestAnswerUnknownSessionIs404 proves the 404 status code (design doc §3).
func TestAnswerUnknownSessionIs404(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session/ses_0000000000000000/answer", map[string]any{
		"call_id": "tc1",
		"answers": []map[string]any{{"question": "q", "text": "a"}},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
}

// TestAnswerNothingPendingIs409 proves the 409 status code (design doc §3).
func TestAnswerNothingPendingIs409(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")
	resp, data := h.do("POST", "/session/"+id+"/answer", map[string]any{
		"call_id": "tc1",
		"answers": []map[string]any{{"question": "q", "text": "a"}},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
}

// TestAnswerStaleCallIDIs400 proves the 400 status code (design doc §3): a
// call_id that doesn't match the pending question is rejected, the pending
// question is left untouched.
func TestAnswerStaleCallIDIs400(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		askUserTurn("tc1", `{"questions":[{"question":"Which environment?"}]}`),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "help me deploy"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t)

	resp, data = h.do("POST", "/session/"+id+"/answer", map[string]any{
		"call_id": "tc-stale",
		"answers": []map[string]any{{"question": "Which environment?", "text": "staging"}},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	sess := h.getSessionQuestion(id)
	if sess.Question == nil || sess.Question.CallID != "tc1" {
		t.Errorf("question = %+v after stale answer, want still pending with CallID tc1", sess.Question)
	}
}

// TestAnswerResumesGoal proves the goal-paused branch end to end: POST
// /answer persists question.answered, clears the pending question, and
// re-spawns PursueGoal with the answer folded into turn 1's directive —
// driving the goal on to completion without ever calling Prompt directly
// from the handler (which would violate PursueGoal's "not concurrently with
// itself or Prompt" contract).
func TestAnswerResumesGoal(t *testing.T) {
	prov := &goalProv{
		name: "test",
		worker: [][]provider.Event{
			askUserTurn("tc1", `{"questions":[{"question":"Which environment?"}]}`),
			asstTurn("deployed to staging"),
		},
		eval: [][]provider.Event{asstTurn("MET: deployment confirmed")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "deploy the service", "max_turns": 5})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t) // paused on ask_user

	resp, data = h.do("POST", "/session/"+id+"/answer", map[string]any{
		"call_id": "tc1",
		"answers": []map[string]any{{"question": "Which environment?", "selected": []string{"staging"}}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer status %d: %s", resp.StatusCode, data)
	}

	evs := sse.collectUntilIdle(t)
	var sawAnswered, sawAchieved bool
	var end *Event
	for i := range evs {
		switch evs[i].Type {
		case "question.answered":
			sawAnswered = true
		case "goal.achieved":
			sawAchieved = true
		case "turn.end":
			end = &evs[i]
		}
	}
	if !sawAnswered {
		t.Errorf("no question.answered record after resuming: %v", evs)
	}
	if !sawAchieved {
		t.Fatalf("goal never achieved after resuming: %v", evs)
	}
	if end == nil || end.Outcome != "completed" {
		t.Errorf("resumed turn.end = %+v, want outcome completed", end)
	}

	sess := h.getSessionQuestion(id)
	if sess.Question != nil {
		t.Errorf("session question = %+v, want nil after resuming", sess.Question)
	}
	if sess.Goal == nil || sess.Goal.Active {
		t.Errorf("goal = %+v, want inactive (achieved)", sess.Goal)
	}

	resp, data = h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message status %d: %s", resp.StatusCode, data)
	}
	var msgs []struct {
		Role  string `json:"role"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "deploy the service") && strings.Contains(p.Text, "staging") {
				found = true
			}
		}
	}
	if !found {
		t.Error("no resumed directive message carrying both the condition and the answer")
	}
}

// TestPromptRejectedWhileGoalPausedOnQuestion proves handlePrompt's guard:
// while a goal is paused awaiting an answer, a bare prompt_async must 409
// (naming /answer as the resume path) rather than silently consuming the
// answer without ever resuming the goal — the "zombie pause" the design doc
// warns about.
func TestPromptRejectedWhileGoalPausedOnQuestion(t *testing.T) {
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{askUserTurn("tc1", `{"questions":[{"question":"Which environment?"}]}`)},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "deploy the service"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t) // paused on ask_user; the run slot is free

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "staging"}},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("prompt_async while goal-paused status = %d, want 409: %s", resp.StatusCode, data)
	}
	if !strings.Contains(strings.ToLower(string(data)), "/answer") {
		t.Errorf("409 body = %s, want it to name /answer as the resume path", data)
	}

	sess := h.getSessionQuestion(id)
	if sess.Question == nil || sess.Question.CallID != "tc1" {
		t.Errorf("question = %+v after rejected prompt, want still pending", sess.Question)
	}
	if sess.Goal == nil || !sess.Goal.Active {
		t.Errorf("goal = %+v after rejected prompt, want still active", sess.Goal)
	}

	resp, data = h.do("POST", "/session/"+id+"/answer", map[string]any{
		"call_id": "tc1",
		"answers": []map[string]any{{"question": "Which environment?", "text": "staging"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("answer after rejected prompt status = %d, want 202: %s", resp.StatusCode, data)
	}
}

// TestAnswerConcurrentRaceExactlyOneWins is the red-first test for the
// design doc §3 invariant the goal-paused branch exists to enforce: "Exactly
// one /answer wins; the loser gets the same 409 a stale call_id gets." Two
// concurrent POST /session/{id}/answer requests for the very same pending
// question (same call_id) must never both resume PursueGoal — that would
// violate its "must not be called concurrently with itself or Prompt"
// contract — so exactly one must be accepted (202) and the other rejected
// (409), and the goal must reach completion exactly once (one turn.end
// outcome "completed", one goal.achieved), never twice.
func TestAnswerConcurrentRaceExactlyOneWins(t *testing.T) {
	prov := &goalProv{
		name: "test",
		worker: [][]provider.Event{
			askUserTurn("tc1", `{"questions":[{"question":"Which environment?"}]}`),
			asstTurn("deployed to staging"),
		},
		eval: [][]provider.Event{asstTurn("MET: deployment confirmed")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "deploy the service", "max_turns": 5})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t) // paused on ask_user; the run slot is free

	// Fire two identical /answer requests as close to simultaneously as
	// possible: a start barrier (closed once) releases both goroutines at
	// the same instant rather than relying on any sleep/timing guess.
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, _ := h.do("POST", "/session/"+id+"/answer", map[string]any{
				"call_id": "tc1",
				"answers": []map[string]any{{"question": "Which environment?", "selected": []string{"staging"}}},
			})
			codes[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()

	var accepted, conflicted int
	for _, c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicted++
		default:
			t.Errorf("unexpected /answer status %d, want 202 or 409", c)
		}
	}
	if accepted != 1 || conflicted != 1 {
		t.Fatalf("codes = %v, want exactly one 202 and one 409", codes)
	}

	evs := sse.collectUntilIdle(t)
	var completedEnds, achieved int
	for _, ev := range evs {
		if ev.Type == "turn.end" && ev.Outcome == "completed" {
			completedEnds++
		}
		if ev.Type == "goal.achieved" {
			achieved++
		}
	}
	if completedEnds != 1 {
		t.Errorf("completed turn.end records = %d, want exactly 1 (no double resume): %v", completedEnds, evs)
	}
	if achieved != 1 {
		t.Errorf("goal.achieved records = %d, want exactly 1 (no double resume): %v", achieved, evs)
	}
}
