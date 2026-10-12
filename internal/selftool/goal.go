// Package selftool holds the tools through which a model inspects and
// changes its own session: goal, model, and session_info. A tool reaches its
// session through the Sessions interface of this package.
package selftool

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
	"DELETE /goal on the server API."

const goalSchema = `{
	"type": "object",
	"properties": {
		"action": {"type": "string", "enum": ["status", "set", "adjust"], "description": "The operation to perform"},
		"condition": {"type": "string", "description": "The goal completion condition (required for set/adjust)"}
	},
	"required": ["action"]
}`

const adjustInstead = `use action "adjust" to change its condition instead`

// Goal is the goal tool. It lets the model read, set, and adjust the goal of
// its session. The runtime binds session when the session starts. A child
// session has no goal tool.
type Goal struct {
	host    Sessions
	session string
}

// NewGoal returns the goal tool, unbound, over host.
func NewGoal(host Sessions) Goal { return Goal{host: host} }

// Spec returns the specification of the goal tool.
func (Goal) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "goal", Description: goalDescription, InputSchema: json.RawMessage(goalSchema)}
}

// Alone makes the goal tool run with no other call of its model call in flight.
func (Goal) Alone() {}

// Bind returns the tool of session id, or nil for a child session.
func (t Goal) Bind(id string, child bool, _ int) turn.Tool {
	if child {
		return nil
	}
	t.session = id
	return t
}

// Run performs the action of c.
func (t Goal) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct{ Action, Condition string }
	if err := json.Unmarshal(c.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("goal: invalid arguments: %w", err)
	}
	s, ok := t.host.Session(t.session)
	if !ok {
		return protocol.ToolResult{}, session.ErrNotOwned
	}
	cond := strings.TrimSpace(in.Condition)
	var err error
	switch in.Action {
	case "status":
	case "set":
		if cond == "" {
			return protocol.ToolResult{}, fmt.Errorf("goal: engine: RegisterGoal requires a non-empty condition (if a goal is already active, %s)", adjustInstead)
		}
		if err = s.StartGoal(ctx, cond); errors.Is(err, session.ErrGoalActive) {
			return protocol.ToolResult{}, fmt.Errorf("goal: a goal is already active; %s", adjustInstead)
		}
	case "adjust":
		if cond == "" {
			return protocol.ToolResult{}, errors.New("goal: engine: UpdateGoal requires a non-empty condition")
		}
		if err = s.AdjustGoal(ctx, cond); errors.Is(err, session.ErrNoGoal) {
			return protocol.ToolResult{}, errors.New("goal: engine: no active goal to update")
		}
	default:
		return protocol.ToolResult{}, fmt.Errorf("goal: unknown action %q (clearing a goal is operator-only — DELETE /goal on the server API)", in.Action)
	}
	if err != nil {
		return protocol.ToolResult{}, fmt.Errorf("goal: %w", err)
	}
	return goalStatus(s.Snapshot().Goal)
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
