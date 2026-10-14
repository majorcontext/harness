package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/majorcontext/harness/protocol"
)

// AskTool is the name of the tool that lets a model of the harness loop ask
// the user a question.
const AskTool = "ask_user_question"

const askDescription = "Ask the user one to four multiple-choice questions when you need a decision that the conversation does not give. " +
	"Each question has two to four options; the user may also give free text. " +
	"The call ends your turn until the user answers. The result holds the answer to each question."

const askSchema = `{
	"type": "object",
	"properties": {
		"questions": {
			"type": "array",
			"minItems": 1,
			"maxItems": 4,
			"description": "The questions to ask the user",
			"items": {
				"type": "object",
				"properties": {
					"question": {"type": "string", "description": "The complete question, ending with a question mark"},
					"header": {"type": "string", "description": "A very short label for the question, at most 12 characters"},
					"options": {
						"type": "array",
						"minItems": 2,
						"maxItems": 4,
						"items": {
							"type": "object",
							"properties": {
								"label": {"type": "string", "description": "The choice, in one to five words"},
								"description": {"type": "string", "description": "What the choice means"}
							},
							"required": ["label", "description"]
						}
					},
					"multiSelect": {"type": "boolean", "description": "Let the user choose more than one option"}
				},
				"required": ["question", "header", "options", "multiSelect"]
			}
		}
	},
	"required": ["questions"]
}`

// NewAskTool returns the question tool. The loop opens the request of a call
// to it, so the tool never runs.
func NewAskTool() Tool { return askTool{} }

type askTool struct{}

func (askTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: AskTool, Description: askDescription, InputSchema: json.RawMessage(askSchema)}
}

func (askTool) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	return protocol.ToolResult{}, errors.New("the loop opens the request of this call")
}

type askInput struct {
	Questions []struct {
		Question string `json:"question"`
		Options  []struct {
			Label string `json:"label"`
		} `json:"options"`
	} `json:"questions"`
}

// checkAsk reports why args are not a question that the user can answer.
func checkAsk(args json.RawMessage) error {
	var in askInput
	if err := json.Unmarshal(args, &in); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if n := len(in.Questions); n < 1 || n > 4 {
		return fmt.Errorf("invalid input: ask one to four questions, not %d", n)
	}
	for _, q := range in.Questions {
		if strings.TrimSpace(q.Question) == "" || len(q.Options) < 2 || len(q.Options) > 4 {
			return errors.New("invalid input: each question needs text and two to four options")
		}
	}
	return nil
}

// answerText is the result of an answered question: each question of args
// with its answer, then each answer for a question that args lacks.
func answerText(args, answer json.RawMessage) string {
	var in askInput
	var got map[string]string
	_ = json.Unmarshal(args, &in)
	_ = json.Unmarshal(answer, &got)
	var pairs []string
	for _, q := range in.Questions {
		if a, ok := got[q.Question]; ok {
			pairs = append(pairs, fmt.Sprintf("%q=%q", q.Question, a))
			delete(got, q.Question)
		}
	}
	for _, q := range slices.Sorted(maps.Keys(got)) {
		pairs = append(pairs, fmt.Sprintf("%q=%q", q, got[q]))
	}
	return "User has answered your questions: " + strings.Join(pairs, ", ") + ". You can now continue with the user's answers in mind."
}

// splitAsks separates the calls to the question tool from the rest, when t offers the tool.
func splitAsks(t runner, calls []protocol.ToolCall) (asks, rest []protocol.ToolCall) {
	if t.find(AskTool) == nil {
		return nil, calls
	}
	for _, c := range calls {
		if c.Name == AskTool {
			asks = append(asks, c)
		} else {
			rest = append(rest, c)
		}
	}
	return asks, rest
}

// openAsk gives each call of asks an error result, except the first that a
// hook allows and that holds a valid question: it opens the request of that
// call, and the call stays open. It reports whether a request opened.
func openAsk(ctx context.Context, t runner, to Sink, asks []protocol.ToolCall, record func(protocol.ToolCall, protocol.ToolResult) error) (bool, error) {
	opened := false
	for _, c := range asks {
		deny, args := "", c.Arguments
		switch {
		case opened:
			deny = "Only one question is open at a time. Ask this question again after the user answers."
		case t.hooks != nil:
			var run protocol.ToolCall
			run, deny = t.hooks.Before(ctx, c)
			args = run.Arguments
		}
		if deny == "" && !opened {
			if err := checkAsk(args); err != nil {
				deny = err.Error()
			}
		}
		if deny != "" {
			if err := record(c, protocol.ToolResult{Text: deny, IsError: true}); err != nil {
				return opened, err
			}
			continue
		}
		if err := to.Ask(c.ID, protocol.RequestQuestion, args); err != nil {
			return opened, err
		}
		opened = true
	}
	return opened, nil
}

// answerAsks records the result of each question call of history that has no
// result and that the user answered.
func answerAsks(ctx context.Context, t runner, to Sink, history []protocol.LogMessage, record func(protocol.ToolCall, protocol.ToolResult) error) error {
	done := map[string]bool{}
	for _, m := range history {
		for _, p := range m.Parts {
			if p.Type == protocol.LogPartToolResult {
				done[p.CallID] = true
			}
		}
	}
	for _, m := range history {
		for _, p := range m.Parts {
			if p.Type != protocol.LogPartToolCall || p.Name != AskTool || done[p.CallID] {
				continue
			}
			res, ok := to.Resolution(p.CallID)
			if !ok || res.Resolution != protocol.ResolutionAnswered {
				continue
			}
			c := protocol.ToolCall{ID: p.CallID, Name: p.Name, Arguments: p.Arguments}
			r := protocol.ToolResult{Text: answerText(p.Arguments, res.Answer)}
			if t.hooks != nil {
				r = t.hooks.After(ctx, c, r)
			}
			if err := record(c, t.join(ctx, c, r)); err != nil {
				return err
			}
		}
	}
	return nil
}
