package tree

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const taskSchema = `{
	"type": "object",
	"properties": {
		"action": {"type": "string", "enum": ["spawn", "cancel", "status", "send", "log"], "description": "The operation to perform; defaults to \"spawn\" if omitted"},
		"agent": {"type": "string", "description": "spawn only: the agent type to spawn: general-purpose, explore, plan, or a custom .agents/*.md definition name — call with an unrecognized name to see this project's full current roster in the error"},
		"prompt": {"type": "string", "description": "The task for the child session to perform (spawn), or the message to deliver to it (send)"},
		"model": {"type": "string", "description": "spawn only: optional model override, as \"provider/model\""},
		"effort": {"type": "string", "description": "spawn only: optional reasoning-effort level for the child: off, minimal, low, medium, or high; omitted means the provider default"},
		"session_id": {"type": "string", "description": "cancel/status/send/log only: the id of a session you spawned, directly or transitively"},
		"tail": {"type": "integer", "description": "log only: how many of the descendant's most recent transcript entries to return (default 20, capped)"}
	}
}`

const taskDescription = "Delegate work to a child session, or manage one you already spawned (directly or transitively). action selects the " +
	"operation and defaults to \"spawn\" if omitted. " +
	"spawn(agent, prompt, model?, effort?): starts a child session that runs independently in the background and returns immediately with its " +
	"session id — it does NOT wait for the child to finish, and you do not need to poll for the result. The child's outcome arrives " +
	"later as engine context on one of your own future turns. agent selects the child's tool set and persona: built-in types are " +
	"\"general-purpose\" (full tool set, can itself spawn children), \"explore\" (read-only, for fast code search), and \"plan\" " +
	"(read-only, returns an implementation plan instead of edits) — a project's .agents/*.md files may define more, and this project's " +
	"current full roster (built-ins plus any custom types) is listed in the error if you call this tool with an agent name it does " +
	"not recognize. model optionally overrides which model the child uses. effort optionally sets the child's reasoning-effort level. " +
	"cancel(session_id): stops a descendant you spawned and its entire subtree — anything IT has spawned too. " +
	"status(session_id): reports a descendant's current status, lineage, and cumulative token usage. " +
	"send(session_id, prompt): delivers a message to a descendant — if it is still running, the message is queued and delivered at its " +
	"next turn boundary (you do not need to wait for it to go idle first); if it is NOT actively running (finished, or idle and never " +
	"started), it is relaunched with your message as a fresh turn, and that outcome arrives later exactly like a new spawn's would. " +
	"log(session_id, tail?): returns the last tail transcript entries of a descendant — living or dead — so you can read what it was doing and how it " +
	"ended, instead of guessing from its fail_reason. tail defaults to 20 and is capped; entries are filled newest-first under a total size " +
	"budget, and the reply reports how many of the transcript's messages it returned. " +
	"cancel/status/send/log only work on a session YOU spawned, directly or through a chain of your own children — anything else is refused."

var taskActions = []string{"spawn", "cancel", "status", "send", "log"}

// taskTool starts and manages the descendants of the session parent. The
// runtime binds parent when the session starts.
type taskTool struct {
	tree   *Tree
	parent string
}

// Tool returns the task tool of the sessions of t. Its Bind method gives the
// tool to one session, as the runtime asks of a session tool.
func (t *Tree) Tool() turn.Tool { return taskTool{tree: t} }

type taskToolArgs struct {
	Action, Agent, Prompt string
	Model, Effort         string
	SessionID             string `json:"session_id"`
	Tail                  int
}

// Bind returns the tool of session id at depth, or nil for a session at
// max_task_depth.
func (t taskTool) Bind(id string, _ bool, depth int) turn.Tool {
	if !t.tree.allowsTask(depth) {
		return nil
	}
	t.parent = id
	return t
}

// Spec returns the definition of the task tool.
func (taskTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "task", Description: taskDescription, InputSchema: json.RawMessage(taskSchema)}
}

// Key makes the calls that name one session run in call order.
func (taskTool) Key(c protocol.ToolCall) string {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(c.Arguments, &in) != nil || in.SessionID == "" {
		return ""
	}
	return "task:" + in.SessionID
}

// Run runs one action of the task tool.
func (t taskTool) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	var in taskToolArgs
	if err := json.Unmarshal(c.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("task: invalid arguments: %w", err)
	}
	in.Action = cmp.Or(in.Action, "spawn")
	var up []string
	var err error
	switch {
	case !slices.Contains(taskActions, in.Action):
		err = fmt.Errorf("unknown action %q (want one of: %s)", in.Action, strings.Join(taskActions, ", "))
	case in.Action == "spawn":
	case in.SessionID == "":
		err = fmt.Errorf("session_id is required for action %q", in.Action)
	default:
		up, err = t.descendant(ctx, in.SessionID)
	}
	var out any
	if err == nil {
		out, err = t.act(ctx, in, up)
	}
	if err != nil {
		return protocol.ToolResult{}, fmt.Errorf("task: %w", err)
	}
	b, err := json.Marshal(out)
	return protocol.ToolResult{Text: string(b)}, err
}

func (t taskTool) act(ctx context.Context, in taskToolArgs, up []string) (any, error) {
	switch in.Action {
	case "spawn":
		return t.spawn(ctx, in)
	case "cancel":
		return t.cancel(ctx, in.SessionID, up)
	case "status":
		return t.status(ctx, in.SessionID, up)
	case "send":
		return t.send(ctx, in, up)
	}
	return t.log(ctx, in)
}

// descendant returns the ancestors of session id, nearest first, when the
// session of the tool is one of them.
func (t taskTool) descendant(ctx context.Context, id string) ([]string, error) {
	up, err := t.tree.ancestors(ctx, id)
	switch {
	case errors.Is(err, session.ErrNotFound):
		return nil, fmt.Errorf("no such session %q", id)
	case err != nil:
		return nil, err
	case !slices.Contains(up, t.parent):
		return nil, fmt.Errorf("%s is not a session you spawned, directly or transitively", id)
	}
	return up, nil
}

func (t taskTool) spawn(ctx context.Context, in taskToolArgs) (any, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	in.Agent = cmp.Or(in.Agent, prompt.GeneralPurpose)
	id, err := t.tree.spawn(ctx, t.parent, in.Agent, in.Prompt, Choice{in.Model, in.Effort})
	return struct {
		SessionID string `json:"session_id"`
		Agent     string `json:"agent"`
		Note      string `json:"note"`
	}{id, in.Agent, "spawned and running in the background; its result will arrive later as engine context — no need to poll or wait for it"}, err
}

// cancel stops session id and each of its descendants, and withdraws
// their queued inputs. The report of session id reaches its parent. The
// report of a descendant reaches the nearest ancestor of session id that has
// not ended its work, as the engine routed it: the parent of session id, or
// the session of the tool when the parents between are done.
func (t taskTool) cancel(ctx context.Context, id string, up []string) (any, error) {
	route, err := t.tree.live(ctx, up, t.parent)
	if err != nil {
		return nil, err
	}
	if err := t.tree.Cancel(ctx, id, route); err != nil {
		return nil, err
	}
	k, err := t.tree.child(ctx, id, 0)
	return struct {
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
	}{id, k.status}, err
}

func (t taskTool) status(ctx context.Context, id string, up []string) (any, error) {
	k, err := t.tree.child(ctx, id, 0)
	if err != nil {
		return nil, err
	}
	u := k.usage
	return struct {
		SessionID  string         `json:"session_id"`
		ParentID   string         `json:"parent_id"`
		Depth      int            `json:"depth"`
		Status     string         `json:"status"`
		Children   []string       `json:"children"`
		AgentType  string         `json:"agent_type"`
		Result     string         `json:"result,omitempty"`
		FailReason string         `json:"fail_reason,omitempty"`
		FailKind   string         `json:"fail_kind,omitempty"`
		Usage      protocol.Usage `json:"usage"`
	}{id, up[0], len(up), k.status, k.children, k.agent, k.result, k.reason, k.kind,
		protocol.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens}}, nil
}

func (t taskTool) send(ctx context.Context, in taskToolArgs, up []string) (any, error) {
	text := strings.TrimSpace(in.Prompt)
	if text == "" {
		return nil, errors.New(`prompt is required for action "send"`)
	}
	queued, err := t.tree.send(ctx, up, in.SessionID, text)
	note := "the descendant was not actively running, so this was dispatched as a fresh turn with your message; " +
		"check back with task status on this session_id if you want to confirm it actually started"
	if queued {
		note = "queued for delivery at the descendant's next turn boundary — no need to poll or wait for it, " +
			"unless the descendant's turn is interrupted first by a cancel or an abort " +
			"(an interrupted descendant leaves anything still queued undelivered, like the rest of its own state)"
	}
	return struct {
		SessionID string `json:"session_id"`
		Queued    bool   `json:"queued"`
		Note      string `json:"note"`
	}{in.SessionID, queued, note}, err
}

func (t taskTool) log(ctx context.Context, in taskToolArgs) (any, error) {
	if in.Tail < 0 {
		return nil, errors.New(`tail must not be negative for action "log"`)
	}
	k, err := t.tree.child(ctx, in.SessionID, min(cmp.Or(in.Tail, logTail), logMaxTail))
	if err != nil {
		return nil, err
	}
	entries := renderLog(k.tail)
	return struct {
		SessionID  string     `json:"session_id"`
		Status     string     `json:"status"`
		AgentType  string     `json:"agent_type,omitempty"`
		FailReason string     `json:"fail_reason,omitempty"`
		FailKind   string     `json:"fail_kind,omitempty"`
		Total      int        `json:"total_messages"`
		Returned   int        `json:"returned"`
		Entries    []logEntry `json:"entries"`
	}{in.SessionID, k.status, k.agent, k.reason, k.kind, k.total, len(entries), entries}, nil
}

// childView is a child session as its ancestors see it.
type childView struct {
	status, result, reason, kind, agent string
	usage                               eventlog.Usage
	children                            []string
	// tail holds the newest messages of the conversation that the caller
	// asked for, and total is the length of the whole conversation.
	tail  []eventlog.Message
	total int
}

// child reads session id, with the newest tail messages of its conversation.
// Its status is running until its last turn ends, then the outcome that it
// reports to its parent.
func (t *Tree) child(ctx context.Context, id string, tail int) (childView, error) {
	k := childView{status: "running"}
	err := t.s.Read(ctx, id, func(st *eventlog.State) {
		k.usage, k.children, k.agent = st.Usage(), append([]string{}, st.Children()...), st.Agent()
		if tail > 0 {
			h := st.History()
			k.total, k.tail = len(h), h[max(0, len(h)-tail):]
		}
		s, rep, ok := session.Settlement(id, st)
		if !ok {
			return
		}
		k.status = string(s.Outcome)
		switch last := st.LastEnded(); s.Outcome {
		case eventlog.OutcomeDone:
			k.result, _ = session.CapRunes(session.LastText(st.History()), session.ResultCap)
		case eventlog.OutcomeFailed:
			k.reason = rep.Reason()
			if last.Cause == eventlog.CauseProviderExhausted {
				k.kind = string(eventlog.CauseProviderExhausted)
			}
		}
	})
	return k, err
}

// live returns the first of the sessions in up whose turn has not ended with
// an outcome, or fallback when each has.
func (t *Tree) live(ctx context.Context, up []string, fallback string) (string, error) {
	for _, id := range up {
		var ended bool
		if err := t.s.Read(ctx, id, func(st *eventlog.State) { _, _, ended = session.Settlement(id, st) }); err != nil {
			return "", err
		}
		if !ended {
			return id, nil
		}
	}
	return fallback, nil
}

// send admits text to child as an input with source parent. When the
// parent of child has settled it, the parent spawns it again first, so the
// child reports the turn that text starts. queued reports a running turn.
func (t *Tree) send(ctx context.Context, up []string, child, text string) (bool, error) {
	c, err := t.s.Open(ctx, child)
	if err != nil {
		return false, err
	}
	p, err := t.s.Open(ctx, up[0])
	if err != nil {
		return false, err
	}
	select {
	case <-p.Recovered:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	rearm := !slices.Contains(p.Actor.View().Unsettled, child)
	if rearm {
		if _, err := t.SpawnChild(ctx, p, child, c.Actor.View().Session.Agent); err != nil {
			return false, err
		}
	}
	queued := c.Actor.View().Session.TurnID != ""
	in := eventlog.InputAdmitted{InputID: "input_" + t.cfg.Suffix(), Delivery: eventlog.DeliverySteer, Source: "parent",
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
	if _, _, err := c.Actor.Submit(ctx, in, ""); err != nil {
		if rearm {
			err = errors.Join(err, p.Actor.Settle(context.WithoutCancel(ctx), eventlog.ChildSettled{ChildID: child, Outcome: eventlog.OutcomeFailed}, nil))
		}
		return false, err
	}
	return queued, nil
}
