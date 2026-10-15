package turn

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/majorcontext/harness/protocol"
)

// ErrNoQuestions is the error of Sink.Ask when the session may not ask a question now.
var ErrNoQuestions = errors.New("you cannot ask the user a question now; continue without asking")

// AskTool is the name of the tool that lets a model of the harness loop ask
// the user a question.
const AskTool = "ask_user_question"

const askDescription = "Ask the user one to four multiple-choice questions during a task. " +
	"Use it to gather preferences or requirements, to clarify an ambiguous instruction, to get a decision on an implementation choice, or to offer the user choices of direction. " +
	"The user can always pick Other and type their own text, so do not add an Other option yourself. " +
	"Set multiSelect to true when the choices are not mutually exclusive. " +
	"To recommend an option, make it the first option and end its label with \" (Recommended)\". " +
	"Use preview on an option only for a concrete artifact that the user must compare side by side, such as a code snippet, a mockup, or a diagram; a preview works only when multiSelect is false. " +
	"Each question text must be unique, and each option label must be unique within its question. " +
	"Do not use this tool to ask whether a plan is ready or whether to proceed. " +
	"The call ends your turn until the user answers; the result holds the answer to each question."

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
					"header": {"type": "string", "maxLength": 12, "description": "A very short label for the question, at most 12 characters"},
					"options": {
						"type": "array",
						"minItems": 2,
						"maxItems": 4,
						"items": {
							"type": "object",
							"properties": {
								"label": {"type": "string", "description": "The choice, in one to five words"},
								"description": {"type": "string", "description": "What the choice means"},
								"preview": {"type": "string", "description": "Optional. A mockup, code snippet, or diagram that the user sees when the option is focused. Only for a question with multiSelect false"}
							},
							"required": ["label", "description"]
						}
					},
					"multiSelect": {"type": "boolean", "default": false, "description": "Let the user choose more than one option"}
				},
				"required": ["question", "header", "options", "multiSelect"]
			}
		},
		"metadata": {
			"type": "object",
			"description": "Optional. Tracking data that the tool ignores",
			"properties": {"source": {"type": "string", "description": "An identifier of the origin of the question"}}
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
		Question    string  `json:"question"`
		Header      *string `json:"header"`
		MultiSelect *bool   `json:"multiSelect"`
		Options     []struct {
			Label       string  `json:"label"`
			Description *string `json:"description"`
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
	seen := map[string]bool{}
	for _, q := range in.Questions {
		if seen[q.Question] {
			return fmt.Errorf("invalid input: the question %q is repeated; each question text must be unique", q.Question)
		}
		seen[q.Question] = true
		if strings.TrimSpace(q.Question) == "" || q.Header == nil || q.MultiSelect == nil || len(q.Options) < 2 || len(q.Options) > 4 {
			return errors.New("invalid input: each question needs question, header, multiSelect, and two to four options")
		}
		if utf8.RuneCountInString(*q.Header) > 12 {
			return fmt.Errorf("invalid input: the header %q has more than 12 characters", *q.Header)
		}
		labels := map[string]bool{}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" || o.Description == nil {
				return errors.New("invalid input: each option needs label and description")
			}
			if labels[o.Label] {
				return fmt.Errorf("invalid input: the option label %q is repeated in the question %q; each label must be unique", o.Label, q.Question)
			}
			labels[o.Label] = true
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

const onlyOneQuestion = "Only one question is open at a time. Ask this question again after the user answers."

// asker returns the Ask of a backend that owns the loop. It handles a call as
// openAsk handles the first call of a model call, and gives every later call
// of the turn the error result of a second question.
func (t runner) asker(to Sink) func(context.Context, protocol.ToolCall) (protocol.ToolResult, bool) {
	var mu sync.Mutex
	opened := false
	return func(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, bool) {
		mu.Lock()
		defer mu.Unlock()
		var res protocol.ToolResult
		if opened {
			return t.join(ctx, c, protocol.ToolResult{Text: onlyOneQuestion, IsError: true}), false
		}
		record := func(_ protocol.ToolCall, r protocol.ToolResult) error { res = r; return nil }
		ok, err := openAsk(ctx, ctx, t, to, []protocol.ToolCall{c}, record)
		if err != nil {
			return protocol.ToolResult{Text: err.Error(), IsError: true}, false
		}
		opened = ok
		return res, ok
	}
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

// openAsk handles the question calls of one model call. It tries the first
// call only: a hook may deny it, and its input may be invalid, and each gives
// an error result. Otherwise it opens the request of that call, and the call
// stays open. Every later call gets an error result. It reports whether a
// request opened.
func openAsk(ctx, step context.Context, t runner, to Sink, asks []protocol.ToolCall, record func(protocol.ToolCall, protocol.ToolResult) error) (bool, error) {
	if err := cmp.Or(context.Cause(ctx), context.Cause(step)); err != nil {
		return false, err
	}
	opened := false
	for i, c := range asks {
		deny, args := onlyOneQuestion, c.Arguments
		if i == 0 {
			deny = ""
			if t.hooks != nil {
				var run protocol.ToolCall
				run, deny = t.hooks.Before(ctx, c)
				args = run.Arguments
			}
			if err := cmp.Or(context.Cause(ctx), context.Cause(step)); err != nil {
				return false, err
			}
			if err := checkAsk(args); deny == "" && err != nil {
				deny = err.Error()
			}
			if deny == "" {
				err := to.Ask(c.ID, protocol.RequestQuestion, args)
				if err == nil {
					opened = true
					continue
				}
				if !errors.Is(err, ErrNoQuestions) {
					return false, err
				}
				deny = err.Error()
			}
		}
		if err := record(c, t.join(ctx, c, protocol.ToolResult{Text: deny, IsError: true})); err != nil {
			return opened, err
		}
	}
	return opened, nil
}

// answerAsks records the result of each question call of req.History that has
// no result and that the user answered, between the hooks of t. The hooks see
// the call with the input of the request, which a before hook may have
// rewritten. It runs before the pins of the first model call, so that no pin
// comes between a call and its result.
func answerAsks(ctx context.Context, t runner, to Sink, req *Request, record func(protocol.ToolCall, protocol.ToolResult) error) error {
	done := map[string]bool{}
	for _, m := range req.History {
		for _, p := range m.Parts {
			if p.Type == protocol.LogPartToolResult {
				done[p.CallID] = true
			}
		}
	}
	for _, m := range slices.Clone(req.History) {
		for _, p := range m.Parts {
			if p.Type != protocol.LogPartToolCall || p.Name != AskTool || done[p.CallID] {
				continue
			}
			res, ok := to.Resolution(p.CallID)
			if !ok || res.Resolution != protocol.ResolutionAnswered {
				continue
			}
			args, ok := to.Asked(p.CallID)
			if !ok {
				args = p.Arguments
			}
			c := protocol.ToolCall{ID: p.CallID, Name: p.Name, Arguments: args}
			r := protocol.ToolResult{Text: answerText(args, res.Answer)}
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
