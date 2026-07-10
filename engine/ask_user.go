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
	s.persistQuestionLocked(recQuestionAsked, questionRecord{CallID: callID, Questions: in.Questions})
	s.mu.Unlock()

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
