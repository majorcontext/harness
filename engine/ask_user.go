// The ask_user built-in tool: model-initiated elicitation. See
// docs/design/question-tool.md for the binding design; this file and the
// Prompt/PursueGoal call sites it wires into implement it, not a redesign.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// askUserToolName is the built-in tool's name, checked by Prompt's tool loop
// (see askUserExecuted in engine.go) to decide whether a round ends the turn.
const askUserToolName = "ask_user"

// askUserSchema is ask_user's JSON Schema input, exactly as specified in
// docs/design/question-tool.md §1.
const askUserSchema = `{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "question": {
            "type": "string",
            "description": "The question text to show the user."
          },
          "options": {
            "type": "array",
            "items": { "type": "string" },
            "description": "Optional enumerated choices. Omit for a freeform text answer."
          },
          "multi": {
            "type": "boolean",
            "description": "Only meaningful with options: whether more than one may be selected. Defaults to false (single-select)."
          }
        },
        "required": ["question"]
      }
    }
  },
  "required": ["questions"]
}`

// askUserDescription is ask_user's model-facing description, exactly as
// specified in docs/design/question-tool.md §1.
const askUserDescription = "Ask the user one or more questions and stop until they answer. Use this " +
	"instead of guessing when a decision needs the user's input — a choice between options, a missing " +
	"piece of information, confirmation before an irreversible action. The current turn ends here; the " +
	"answer arrives as the next prompt."

// askUserArgs is ask_user's call arguments, decoded from the tool call's
// Arguments. Its shape mirrors askUserSchema exactly.
type askUserArgs struct {
	Questions []plugin.QuestionItem `json:"questions"`
}

// askUserTool is the built-in ask_user tool, installed unconditionally like
// bash/read_file (see newSession). Its Run is never actually invoked in
// ordinary operation — ask_user needs the executing tool call's own CallID
// (question.asked is keyed on it, per the design doc §2), which the generic
// Tool.Run signature does not carry, so executeTool special-cases it ahead
// of the tools map and dispatches to runAskUser instead. Run exists only so
// the Tool value (used for the Def in toolDefs' tool listing) is
// well-formed.
func askUserTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        askUserToolName,
			Description: askUserDescription,
			InputSchema: json.RawMessage(askUserSchema),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			return nil, errors.New("ask_user: must be dispatched via runAskUser, not Tool.Run")
		},
	}
}

// runAskUser implements docs/design/question-tool.md §2's three steps for
// one ask_user call: persist a durable question.asked record keyed on
// callID, set s.awaitingQuestion, and return an ordinary, fully-resolved
// (never an error) tool result. There is no "pending" tool_result shape —
// the call is real and final the instant this returns; "waiting" is
// session-level state only (s.awaitingQuestion), never a message-level one.
func (s *Session) runAskUser(callID string, args json.RawMessage) message.Parts {
	var in askUserArgs
	if err := json.Unmarshal(args, &in); err != nil || len(in.Questions) == 0 {
		// ask_user's contract is "always resolves"; a malformed call still
		// gets a real (error) result rather than a pending shape, but does
		// NOT set awaitingQuestion or persist anything — there is nothing
		// durable to explain a "waiting" state that was never entered.
		return message.Parts{&message.Text{Text: "ask_user: questions must be a non-empty array"}}
	}

	s.mu.Lock()
	s.awaitingQuestion = true
	s.questionCallID = callID
	s.pendingResumeAnswer = ""
	s.pendingResumeAnswerSet = false
	s.persistQuestionLocked(recQuestionAsked, questionRecord{CallID: callID, Questions: in.Questions})
	// Emit while still holding s.mu (see ClearGoal's doc comment on the same
	// pattern in goal.go): keeps the event stream ordered the same as the
	// log write above under a concurrent clear. OnEvent must not call back
	// into this Session, which would deadlock on s.mu, held here.
	s.emit(Event{Type: EventQuestionAsked, QuestionCallID: callID, QuestionItems: in.Questions})
	s.mu.Unlock()

	if s.cfg.Hooks != nil {
		props, _ := json.Marshal(plugin.QuestionAskedProperties{CallID: callID, Questions: in.Questions})
		s.cfg.Hooks.Emit([]plugin.Event{{
			Type:       plugin.EventQuestionAsked,
			SessionID:  s.ID,
			Properties: props,
		}})
	}

	text := fmt.Sprintf("%d question(s) recorded (call %s); waiting for the user's answer as the next prompt.", len(in.Questions), callID)
	return message.Parts{&message.Text{Text: text}}
}

// AwaitingQuestion reports the CallID of the currently pending ask_user
// call, if any (guarded by mu, same pattern as ActiveGoal).
func (s *Session) AwaitingQuestion() (callID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.questionCallID, s.awaitingQuestion
}

// clearAwaitingQuestionOnPrompt is Session.Prompt's own idempotent
// clear-and-persist for the interactive path (design doc §3): whenever a
// new prompt arrives while a question is pending, that prompt IS the
// answer, so this persists question.answered — AnswerText is the new
// prompt's own text, since for this path the prompt is the delivery — and
// clears s.awaitingQuestion. It is a no-op when no question is pending,
// which covers both the ordinary case (no question ever asked) and the
// goal-paused resume case, where POST /answer's AnswerQuestion already
// cleared the pending state before PursueGoal's resumed Prompt call ever
// ran. That is what makes "one answer yields exactly one question.answered,
// whether it arrived via /answer or a bare prompt_async" true without
// either path needing to know about the other.
func (s *Session) clearAwaitingQuestionOnPrompt(text string) {
	s.mu.Lock()
	if !s.awaitingQuestion {
		s.mu.Unlock()
		return
	}
	callID := s.questionCallID
	s.awaitingQuestion = false
	s.questionCallID = ""
	s.pendingResumeAnswer = ""
	s.pendingResumeAnswerSet = false
	s.persistQuestionLocked(recQuestionAnswered, questionRecord{CallID: callID, AnswerText: text})
	s.emit(Event{Type: EventQuestionAnswered, QuestionCallID: callID})
	s.mu.Unlock()
}

// AnswerQuestion is the atomic claim POST /session/{id}/answer's
// goal-paused branch (server) uses, mirroring claimForPrompt: under one
// lock, it checks that callID matches the pending question, persists
// question.answered carrying answerText — the formatted answer block
// itself, not just the fact of answering (design doc §3) — and clears
// s.awaitingQuestion. It reports ok=false when nothing is pending at all
// (hadPending=false, the caller's 409 case) or a DIFFERENT question is
// pending (hadPending=true, the caller's 400 stale-call_id case). Two
// concurrent calls can never both observe the pending question and both
// win — exactly the guarantee PursueGoal's "not concurrently with itself"
// contract needs.
func (s *Session) AnswerQuestion(callID, answerText string) (ok, hadPending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.awaitingQuestion {
		return false, false
	}
	if s.questionCallID != callID {
		return false, true
	}
	s.awaitingQuestion = false
	s.questionCallID = ""
	s.persistQuestionLocked(recQuestionAnswered, questionRecord{CallID: callID, AnswerText: answerText})
	s.emit(Event{Type: EventQuestionAnswered, QuestionCallID: callID})
	return true, true
}

// PendingResumeAnswer returns the formatted answer text of a
// question.answered record that LoadSession replayed with no subsequent
// message record — the crash window between POST /answer's atomic claim
// persisting the answer and the resumed goal worker's first Prompt call
// ever appending it (design doc §3, "reload of an answered-but-never-
// resumed question rebuilds ResumeAnswer"). ok is false once that answer
// has actually been delivered as a message (the ordinary case, and the
// zero-value case where no question was ever asked).
func (s *Session) PendingResumeAnswer() (text string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingResumeAnswer, s.pendingResumeAnswerSet
}

// TakePendingResumeAnswer is PendingResumeAnswer with consume-on-read: the
// pending answer is cleared the moment a caller commits to delivering it,
// so a retried recovery (a second /answer for the already-recovered
// question, or any later resume path) can never re-deliver the same answer
// twice. Live delivery has no other clear site — the load-replay clear only
// fires on LoadSession, and the resumed worker's own Prompt appends
// messages without touching this flag — which makes consuming here the
// single thing standing between "recovered once" and "recovered on every
// retry". Callers that only need to know whether an answer is pending use
// PendingResumeAnswer.
func (s *Session) TakePendingResumeAnswer() (text string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, ok = s.pendingResumeAnswer, s.pendingResumeAnswerSet
	s.pendingResumeAnswer = ""
	s.pendingResumeAnswerSet = false
	return text, ok
}
