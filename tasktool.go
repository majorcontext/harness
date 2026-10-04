package harness

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
		"agent": {"type": "string", "description": "spawn only: the agent profile of the child (default general-purpose)"},
		"prompt": {"type": "string", "description": "The whole task for the child (spawn), or the message to deliver to it (send). The child sees nothing of this conversation."},
		"session_id": {"type": "string", "description": "cancel/status/send/log only: the id of a session you spawned, directly or transitively"},
		"tail": {"type": "integer", "description": "log only: how many of the descendant's most recent transcript entries to return (default 20, capped)"}
	}
}`

const taskDescription = "Delegate work to a child session, or manage one you already spawned (directly or transitively). " +
	"action selects the operation and defaults to \"spawn\" if omitted. " +
	"spawn(agent?, prompt): starts a child agent that does a task in the background, in a session of its own. " +
	"The call returns at once with the session id of the child. The final report of the child arrives later as a new message: " +
	"do not poll or wait for it. agent selects the profile of the child: general-purpose has every tool, " +
	"explore finds code with read-only tools, plan returns an implementation plan with read-only tools, " +
	"and each .agents/*.md file of the project adds a profile. A call with an unknown agent lists the profiles. " +
	"cancel(session_id): stops a descendant you spawned and its entire subtree — anything IT has spawned too. " +
	"status(session_id): reports a descendant's current status, lineage, and cumulative token usage. " +
	"send(session_id, prompt): delivers a message to a descendant — if it is still running, the message is queued and delivered " +
	"at its next turn boundary; if it is not running, it runs a fresh turn with your message, and that report arrives later " +
	"exactly like a new spawn's would. " +
	"log(session_id, tail?): returns the last tail transcript entries of a descendant — living or dead — so you can read what it " +
	"was doing and how it ended. tail defaults to 20 and is capped; entries are filled newest-first under a total size budget, " +
	"and the reply reports how many of the transcript's messages it returned. " +
	"cancel/status/send/log only work on a session YOU spawned, directly or through a chain of your own children — anything else is refused."

var taskActions = []string{"spawn", "cancel", "status", "send", "log"}

// taskTool starts and manages the descendants of the session parent. The
// runtime binds parent when the session starts.
type taskTool struct {
	r      *Runtime
	parent string
}

type taskArgs struct {
	Action, Agent, Prompt string
	SessionID             string `json:"session_id"`
	Tail                  int
}

func (t taskTool) bind(id string, _ bool) turn.Tool { t.parent = id; return t }

func (taskTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "task", Description: taskDescription, InputSchema: json.RawMessage(taskSchema)}
}

func (t taskTool) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	var in taskArgs
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

func (t taskTool) act(ctx context.Context, in taskArgs, up []string) (any, error) {
	switch in.Action {
	case "spawn":
		return t.spawn(ctx, in)
	case "cancel":
		return t.cancel(ctx, in.SessionID)
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
	if checkName("session", id) != nil {
		return nil, fmt.Errorf("no such session %q", id)
	}
	up, err := t.r.ancestors(ctx, id)
	switch {
	case errors.Is(err, ErrSessionNotFound):
		return nil, fmt.Errorf("no such session %q", id)
	case err != nil:
		return nil, err
	case !slices.Contains(up, t.parent):
		return nil, fmt.Errorf("%s is not a session you spawned, directly or transitively", id)
	}
	return up, nil
}

func (t taskTool) spawn(ctx context.Context, in taskArgs) (any, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	in.Agent = cmp.Or(in.Agent, prompt.GeneralPurpose)
	id, err := t.r.spawn(ctx, t.parent, in.Agent, in.Prompt)
	return struct {
		SessionID string `json:"session_id"`
		Agent     string `json:"agent"`
		Note      string `json:"note"`
	}{id, in.Agent, "running in the background; its report arrives later as a message. Do not poll or wait for it."}, err
}

// cancel stops session id and each of its descendants, and withdraws
// their queued inputs. The report of session id reaches its parent.
func (t taskTool) cancel(ctx context.Context, id string) (any, error) {
	if err := t.r.interruptTree(ctx, id, func(ctx context.Context) error { return t.r.cancelTurn(ctx, id) }); err != nil {
		return nil, err
	}
	k, err := t.r.child(ctx, id)
	return struct {
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
	}{id, k.status}, err
}

func (t taskTool) status(ctx context.Context, id string, up []string) (any, error) {
	k, err := t.r.child(ctx, id)
	if err != nil {
		return nil, err
	}
	u := k.st.Usage()
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
	}{id, up[0], len(up), k.status, append([]string{}, k.st.Children()...), k.st.Agent(), k.result, k.reason, k.kind,
		protocol.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens}}, nil
}

func (t taskTool) send(ctx context.Context, in taskArgs, up []string) (any, error) {
	text := strings.TrimSpace(in.Prompt)
	if text == "" {
		return nil, errors.New(`prompt is required for action "send"`)
	}
	queued, err := t.r.send(ctx, up, in.SessionID, text)
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

func (t taskTool) log(ctx context.Context, in taskArgs) (any, error) {
	if in.Tail < 0 {
		return nil, errors.New(`tail must not be negative for action "log"`)
	}
	k, err := t.r.child(ctx, in.SessionID)
	if err != nil {
		return nil, err
	}
	h := k.st.History()
	entries := renderLog(h[max(0, len(h)-min(cmp.Or(in.Tail, logTail), logMaxTail)):])
	return struct {
		SessionID  string     `json:"session_id"`
		Status     string     `json:"status"`
		AgentType  string     `json:"agent_type,omitempty"`
		FailReason string     `json:"fail_reason,omitempty"`
		FailKind   string     `json:"fail_kind,omitempty"`
		Total      int        `json:"total_messages"`
		Returned   int        `json:"returned"`
		Entries    []logEntry `json:"entries"`
	}{in.SessionID, k.status, k.st.Agent(), k.reason, k.kind, len(h), len(entries), entries}, nil
}

// childView is a child session as its ancestors see it.
type childView struct {
	st                           *eventlog.State
	status, result, reason, kind string
}

// child reads session id from the store. Its status is running until its
// last turn ends, then the outcome that it reports to its parent.
func (r *Runtime) child(ctx context.Context, id string) (childView, error) {
	st, err := session.Load(ctx, id, storeLog{r.store, id})
	if err != nil {
		return childView{}, err
	}
	k := childView{st: st, status: "running"}
	s, _, ok := session.Settlement(id, st)
	if !ok {
		return k, nil
	}
	k.status = string(s.Outcome)
	switch last := st.LastEnded(); s.Outcome {
	case eventlog.OutcomeDone:
		k.result, _ = capRunes(session.LastText(st.History()), resultCap)
	case eventlog.OutcomeFailed:
		k.reason = last.Error
		if eventlog.Cause(last.Error) == eventlog.CauseProviderExhausted {
			k.kind = string(eventlog.CauseProviderExhausted)
		}
	}
	return k, nil
}

// send admits text to child as an input with source parent. When the
// parent of child has settled it, the parent spawns it again first, so the
// child reports the turn that text starts. queued reports a running turn.
func (r *Runtime) send(ctx context.Context, up []string, child, text string) (bool, error) {
	c, err := r.Open(ctx, child)
	if err != nil {
		return false, err
	}
	p, err := r.Open(ctx, up[0])
	if err != nil {
		return false, err
	}
	select {
	case <-p.recovered:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	unsettled, err := p.a.Unsettled(ctx)
	if err != nil {
		return false, err
	}
	rearm, added := !slices.Contains(unsettled, child), false
	if rearm {
		if added, err = r.rearm(ctx, p, up[len(up)-1], child, len(up)); err != nil {
			return false, err
		}
	}
	queued := c.View().TurnID != ""
	in := eventlog.InputAdmitted{InputID: "input_" + newSuffix(), Delivery: eventlog.DeliverySteer, Source: "parent",
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
	if _, _, err := c.a.Submit(ctx, in, ""); err != nil {
		if added {
			r.sup.done(child)
		}
		if rearm {
			err = errors.Join(err, p.a.Settle(context.WithoutCancel(ctx), eventlog.ChildSettled{ChildID: child, Outcome: eventlog.OutcomeFailed}, ""))
		}
		return false, err
	}
	return queued, nil
}

// rearm spawns the settled child of p again with its agent. added reports
// whether the supervisor counts child because of this call.
func (r *Runtime) rearm(ctx context.Context, p *Session, root, child string, depth int) (added bool, err error) {
	c, err := r.created(ctx, child)
	if err != nil {
		return false, err
	}
	if added, err = r.sup.admit(root, child, depth); err != nil {
		return false, err
	}
	if _, err := p.a.Spawn(ctx, child, c.Agent); err != nil {
		if added {
			r.sup.done(child)
		}
		return false, err
	}
	return added, nil
}

// cancelTurn withdraws the queued inputs of session id and stops its turn,
// when this runtime runs it.
func (r *Runtime) cancelTurn(ctx context.Context, id string) error {
	s := r.loaded(ctx, id)
	if s == nil {
		return nil
	}
	if err := s.a.Cancel(ctx); !errors.Is(err, ErrSessionNotOwned) {
		return err
	}
	return nil
}

// interruptTree stops session id by stop, then each descendant that this
// runtime runs, and withdraws the queued inputs of each descendant. It
// silences the children of each session before it stops that session, so
// no parent inside the tree starts a turn on a report. The walk outlives
// ctx, so a caller that leaves does not stop half of a tree.
func (r *Runtime) interruptTree(ctx context.Context, id string, stop func(context.Context) error) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	defer context.AfterFunc(r.base, cancel)()
	quiet := map[string]bool{}
	defer func() {
		for kid := range quiet {
			r.sup.silence(kid, -1)
		}
	}()
	return r.stopTree(ctx, id, stop, quiet)
}

func (r *Runtime) stopTree(ctx context.Context, id string, stop func(context.Context) error, quiet map[string]bool) error {
	silence := func() ([]string, error) {
		st, err := session.Load(ctx, id, storeLog{r.store, id})
		if errors.Is(err, ErrSessionNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, kid := range st.Children() {
			if !quiet[kid] {
				quiet[kid] = true
				r.sup.silence(kid, 1)
			}
		}
		return st.Children(), nil
	}
	if _, err := silence(); err != nil {
		return err
	}
	if err := stop(ctx); err != nil {
		return err
	}
	kids, err := silence()
	for _, kid := range kids {
		err = errors.Join(err, r.stopTree(ctx, kid, func(ctx context.Context) error { return r.cancelTurn(ctx, kid) }, quiet))
	}
	return err
}
