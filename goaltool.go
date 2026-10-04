package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const goalDescription = "Inspect, set, or adjust this session's completion goal: a natural-language condition " +
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
	"DELETE /sessions/{id}/goal on the HTTP API."

const goalSchema = `{
	"type": "object",
	"properties": {
		"action": {"type": "string", "enum": ["status", "set", "adjust"], "description": "The operation to perform"},
		"condition": {"type": "string", "description": "The goal completion condition (required for set/adjust)"}
	},
	"required": ["action"]
}`

const adjustInstead = `use action "adjust" to change its condition instead`

// goalTool lets the model read, set, and adjust the goal of its session.
// The runtime binds session when the session starts.
type goalTool struct {
	r       *Runtime
	session string
}

func (goalTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "goal", Description: goalDescription, InputSchema: json.RawMessage(goalSchema)}
}

func (t goalTool) bind(id string) turn.Tool { t.session = id; return t }

func (t goalTool) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct{ Action, Condition string }
	if err := json.Unmarshal(c.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("goal: invalid arguments: %w", err)
	}
	s := t.r.running(t.session)
	if s == nil {
		return protocol.ToolResult{}, ErrSessionNotOwned
	}
	cond := strings.TrimSpace(in.Condition)
	var err error
	switch in.Action {
	case "status":
	case "set":
		if cond == "" {
			return protocol.ToolResult{}, fmt.Errorf("goal: set requires a non-empty condition (if a goal is already active, %s)", adjustInstead)
		}
		if err = s.a.StartGoal(ctx, cond); errors.Is(err, session.ErrGoalActive) {
			return protocol.ToolResult{}, fmt.Errorf("goal: a goal is already active; %s", adjustInstead)
		}
	case "adjust":
		if cond == "" {
			return protocol.ToolResult{}, errors.New("goal: adjust requires a non-empty condition")
		}
		if err = s.a.AdjustGoal(ctx, cond); errors.Is(err, session.ErrNoGoal) {
			return protocol.ToolResult{}, errors.New("goal: no active goal to update")
		}
	default:
		return protocol.ToolResult{}, fmt.Errorf("goal: unknown action %q (clearing a goal is operator-only — DELETE /sessions/{id}/goal on the HTTP API)", in.Action)
	}
	if err != nil {
		return protocol.ToolResult{}, fmt.Errorf("goal: %w", err)
	}
	return goalStatus(s.View().Goal)
}

func goalStatus(g *protocol.GoalView) (protocol.ToolResult, error) {
	var out struct {
		Active    bool   `json:"active"`
		Condition string `json:"condition"`
	}
	if g != nil && (g.State == "active" || g.State == "paused") {
		out.Active, out.Condition = true, g.Condition
	}
	b, err := json.Marshal(out)
	return protocol.ToolResult{Text: string(b)}, err
}
