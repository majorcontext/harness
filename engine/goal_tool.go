// Goal self-set/adjust: the `goal` session tool lets the model itself
// inspect, set, or adjust this session's completion goal from inside a
// running turn, in-process — no HTTP round-trip, no run-slot claim (see
// goal.go for the underlying state machine and docs/design/
// 2026-07-19-goal-self-adjust.md for the full design).
//
// Three actions only: status (read-only), set (RegisterGoal — arms a new
// goal that begins running only after the current turn ends), and adjust
// (UpdateGoal — rewrites an already-active goal's condition in place).
// There is deliberately no clear action: clearing an active goal stays
// operator-only (DELETE /goal) per the locked design decision in the plan
// ("No self-clear"), so an unknown or "clear" action is rejected with a
// tool error naming that.
//
// Gated by Config.GoalTool (default false): registered in newSession only
// when the host opts in, exactly like the process tool is gated by a
// non-nil Config.Processes. The server/CLI wiring that flips this flag on
// is a later task (see the plan) — this package only defines the tool and
// its gate.
package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// goalToolName is the session tool's fixed name.
const goalToolName = "goal"

// goalToolArgs is the goal tool's input shape.
type goalToolArgs struct {
	Action    string `json:"action"`
	Condition string `json:"condition"`
}

// goalToolResult is the JSON payload every goal tool action returns: the
// goal's state after the action ran (status simply reports the current
// state; set/adjust report the state they just established).
type goalToolResult struct {
	Active    bool   `json:"active"`
	Condition string `json:"condition"`
}

// goalTool builds the `goal` session tool. See the package doc for the
// action contract.
func goalTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name: goalToolName,
			Description: "Inspect, set, or adjust this session's completion goal: a natural-language condition " +
				"that an independent, tool-less evaluator model checks after every turn (see the goal loop). " +
				"Actions: " +
				"status() reports whether a goal is currently active and its condition; " +
				"set(condition) arms a NEW goal — it does NOT start evaluating during this turn; the goal loop " +
				"begins running only AFTER the current turn ends, and every subsequent turn's completion is judged " +
				"by that separate, independent evaluator model, not by you. set fails if a goal is already active " +
				"— use adjust instead; " +
				"adjust(condition) rewrites the condition of an ALREADY-active goal in place; a running goal loop " +
				"picks up the new condition at its next turn boundary. " +
				"There is no action to clear a goal here — clearing an active goal is operator-only, via " +
				"DELETE /goal on the server API.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "enum": ["status", "set", "adjust"], "description": "The operation to perform"},
					"condition": {"type": "string", "description": "The goal completion condition (required for set/adjust)"}
				},
				"required": ["action"]
			}`),
		},
		Run: func(_ context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			return runGoalTool(s, args)
		},
	}
}

// runGoalTool dispatches one goal tool call against s.
func runGoalTool(s *Session, raw json.RawMessage) (message.Parts, error) {
	var in goalToolArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("goal: invalid arguments: %w", err)
	}

	switch in.Action {
	case "status":
		condition, active := s.ActiveGoal()
		return jsonResult(goalToolResult{Active: active, Condition: condition})

	case "set":
		// Pre-check under the same ActiveGoal() read the server handler uses
		// (see server/handlers.go's handleGoal) so the "already active" case
		// gets a message that names the adjust action, rather than surfacing
		// RegisterGoal's bare "already active" wording verbatim.
		if _, active := s.ActiveGoal(); active {
			return nil, fmt.Errorf("goal: a goal is already active; use action %q to change its condition instead", "adjust")
		}
		if err := s.RegisterGoal(in.Condition); err != nil {
			// Race fallback: a goal became active between the check above and
			// this call (or the condition was empty). Either way, surface the
			// same adjust-instead guidance for the active-goal case; the
			// empty-condition case still names its own error.
			return nil, fmt.Errorf("goal: %w (if a goal is already active, use action %q to change its condition instead)", err, "adjust")
		}
		return jsonResult(goalToolResult{Active: true, Condition: in.Condition})

	case "adjust":
		if err := s.UpdateGoal(in.Condition); err != nil {
			return nil, fmt.Errorf("goal: %w", err)
		}
		condition, active := s.ActiveGoal()
		return jsonResult(goalToolResult{Active: active, Condition: condition})

	default:
		return nil, fmt.Errorf("goal: unknown action %q (clearing a goal is operator-only — DELETE /goal on the server API)", in.Action)
	}
}
