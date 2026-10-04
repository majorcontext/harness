package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

const (
	askTool  = "AskUserQuestion"
	askKind  = "question"
	planMode = ",EnterPlanMode,ExitPlanMode"
	// deferHook parks an AskUserQuestion call: the turn ends with stop_reason
	// tool_deferred, and the CLI runs the call again on the next --resume.
	deferHook  = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"defer"}}`
	dismissed  = "The user dismissed this question without answering."
	noApprover = "Permission denied: this session has no interactive approver."
)

// resolution answers the call that the CLI session parked, over the control
// channel. A dismissal interrupts the CLI right after the denial, so it
// costs no model call.
type resolution struct {
	callID   string
	decision map[string]any
	dismiss  bool
}

var denyAll = map[string]any{"behavior": "deny", "message": noApprover}

// questionSettings is the --settings value that defers every AskUserQuestion
// call except pass, which a resolution answers over the control channel. pass
// reaches a shell command, so an ID outside the tool use alphabet is never passed.
func questionSettings(pass string) string {
	cmd := "cat >/dev/null; printf '%s' '" + deferHook + "'"
	if pass != "" && strings.Trim(pass, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") == "" {
		cmd = `input=$(cat); case "$input" in *'"` + pass + `"'*) ;; *) printf '%s' '` + deferHook + `';; esac`
	}
	hook := map[string]any{"matcher": askTool, "hooks": []any{map[string]any{"type": "command", "command": cmd}}}
	data, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []any{hook}}})
	return string(data)
}

func controlResponse(requestID string, decision map[string]any) map[string]any {
	return map[string]any{"type": "control_response",
		"response": map[string]any{"subtype": "success", "request_id": requestID, "response": decision}}
}

// resolve returns the resolution of the call that the CLI session parked, and
// whether this run only carries it. An answer with no input is the turn that
// Resolve starts; anything else dismisses the call before the run goes on.
func resolve(req turn.Request, out turn.Sink, callID string) (*resolution, bool, error) {
	res, ok := out.Resolution(callID)
	if !ok || res.Resolution != eventlog.ResolutionAnswered || len(req.Input) > 0 {
		return &resolution{callID: callID, dismiss: true,
			decision: map[string]any{"behavior": "deny", "message": dismissed, "interrupt": true}}, false, nil
	}
	input, err := answeredInput(req.History, callID, res.Answer)
	if err != nil {
		return nil, false, err
	}
	return &resolution{callID: callID, decision: map[string]any{"behavior": "allow", "updatedInput": input}}, true, nil
}

// answeredInput rebuilds the input of the parked call with the answers added,
// the shape that the AskUserQuestion tool of the CLI reads its result from.
func answeredInput(history []eventlog.Message, callID string, answers json.RawMessage) (map[string]any, error) {
	for i := len(history) - 1; i >= 0; i-- {
		for _, p := range history[i].Parts {
			if p.Type != eventlog.PartToolCall || p.CallID != callID {
				continue
			}
			input := map[string]any{}
			if err := json.Unmarshal(p.Arguments, &input); err != nil {
				return nil, fmt.Errorf("claudecode: decode question %s: %w", callID, err)
			}
			input["answers"] = answers
			return input, nil
		}
	}
	return nil, fmt.Errorf("claudecode: no tool call %s in the history", callID)
}

// ask opens the request of the question that the CLI parked, and remembers
// the call, so the next run resolves it.
func (r *run) ask() error {
	if r.question == nil || !r.questions || r.result.StopReason != "tool_deferred" {
		return nil
	}
	r.mirror.Parked = r.question.callID
	return r.out.Ask(r.question.callID, askKind, r.question.input)
}

// question is the AskUserQuestion call of the main thread.
type question struct {
	callID string
	input  json.RawMessage
}

// control answers a permission request of the CLI: the parked call by its
// resolution, any other call by a denial.
func (r *run) control(env envelope) error {
	if env.Request == nil || env.Request.Subtype != "can_use_tool" {
		return nil
	}
	decision := denyAll
	if r.resolution != nil && env.Request.ToolUseID == r.resolution.callID {
		decision, r.denied = r.resolution.decision, r.dismissing()
	}
	return r.proc.Send(controlResponse(env.RequestID, decision))
}
