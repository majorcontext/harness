package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const taskSchema = `{
	"type": "object",
	"properties": {
		"agent": {"type": "string", "description": "The agent profile of the child (default general-purpose)."},
		"prompt": {"type": "string", "description": "The whole task. The child sees nothing of this conversation."}
	},
	"required": ["prompt"]
}`

const taskDescription = "Start a child agent that does a task in the background, in a session of its own. " +
	"The call returns at once with the session id of the child. The final report of the child arrives later as a new message: " +
	"do not poll or wait for it. agent selects the profile of the child: general-purpose has every tool, " +
	"explore finds code with read-only tools, plan returns an implementation plan with read-only tools, " +
	"and each .agents/*.md file of the project adds a profile. A call with an unknown agent lists the profiles."

// taskTool starts a child of the session parent. The runtime binds parent
// when the session starts.
type taskTool struct {
	r      *Runtime
	parent string
}

func (taskTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "task", Description: taskDescription, InputSchema: json.RawMessage(taskSchema)}
}

func (t taskTool) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct{ Agent, Prompt string }
	if err := json.Unmarshal(c.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("task: invalid arguments: %w", err)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return protocol.ToolResult{}, errors.New("task: prompt is required")
	}
	in.Agent = cmp.Or(in.Agent, prompt.GeneralPurpose)
	id, err := t.r.spawn(ctx, t.parent, in.Agent, in.Prompt)
	if err != nil {
		return protocol.ToolResult{}, fmt.Errorf("task: %w", err)
	}
	out, err := json.Marshal(struct {
		SessionID string `json:"session_id"`
		Agent     string `json:"agent"`
		Note      string `json:"note"`
	}{id, in.Agent, "running in the background; its report arrives later as a message. Do not poll or wait for it."})
	return protocol.ToolResult{Text: string(out)}, err
}

// spawn appends child.spawned to parent, then creates the child with task
// as its first input. A child that fails to start settles failed at once.
func (r *Runtime) spawn(ctx context.Context, parent, agent, task string) (string, error) {
	profiles := prompt.Profiles(r.workDir)
	p, ok := profiles[agent]
	if !ok {
		return "", fmt.Errorf("unknown agent %q; the agents are %s", agent, strings.Join(slices.Sorted(maps.Keys(profiles)), ", "))
	}
	ps := r.running(parent)
	if ps == nil {
		return "", ErrSessionNotOwned
	}
	root, depth, err := r.lineage(ctx, parent)
	if err != nil {
		return "", err
	}
	id := "ses_" + newSuffix()
	if err := r.sup.admit(root, id, depth+1); err != nil {
		return "", err
	}
	c, err := ps.a.Spawn(ctx, id, agent)
	if err != nil {
		r.sup.done(id)
		return "", err
	}
	if p.Model != "" {
		c.Model = r.resolve(p.Model)
	}
	c.Origin, c.AllowedTools = "task", r.available(c.Model, narrow(p.Tools, c.AllowedTools))
	first := eventlog.InputAdmitted{InputID: "input_" + newSuffix(), Delivery: eventlog.DeliveryQueue, Source: "parent",
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: task}}}
	ctx = context.WithoutCancel(ctx)
	if _, err := r.create(ctx, id, c, &first); err != nil {
		r.sup.done(id)
		return "", errors.Join(err, ps.a.Settle(ctx, eventlog.ChildSettled{ChildID: id, Outcome: eventlog.OutcomeFailed}, ""))
	}
	return id, nil
}

// narrow returns the names of a profile that the parent allows. nil allows every name.
func narrow(profile, parent []string) []string {
	switch {
	case profile == nil:
		return parent
	case parent == nil:
		return profile
	}
	return slices.DeleteFunc(slices.Clone(profile), func(n string) bool { return !slices.Contains(parent, n) })
}

// available drops each name that neither a tool of r nor a built-in tool
// of model has, because a profile can name the tools of every backend.
func (r *Runtime) available(model string, names []string) []string {
	if names == nil {
		return nil
	}
	builtins := r.backend.Capabilities(model).Tools
	has := func(n string) bool {
		return slices.Contains(builtins, n) || r.mcp != nil && mcpsrc.Reserved(n) ||
			slices.ContainsFunc(r.tools, func(t turn.Tool) bool { return t.Spec().Name == n })
	}
	out := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !has(n) })
	if len(out) == 0 && len(names) > 0 {
		slog.Warn("harness: the child has no tool of its profile", "model", model, "tools", names)
	}
	return out
}

// lineage returns the root of the tree of session id and the depth of id
// below it. It reads the session.created record of each ancestor, and
// stops past max_task_depth, where no spawn can pass.
func (r *Runtime) lineage(ctx context.Context, id string) (string, int, error) {
	for depth := 0; ; depth++ {
		recs, err := r.store.Read(ctx, id, 0, 1)
		if err == nil && len(recs) == 0 {
			err = fmt.Errorf("%w: %s", ErrSessionNotFound, id)
		}
		if err != nil {
			return "", 0, err
		}
		env, err := eventlog.Decode(recs[0].Data)
		if err != nil {
			return "", 0, err
		}
		c, _ := env.Event.(eventlog.SessionCreated)
		if c.ParentID == "" || depth > r.sup.depth {
			return id, depth, nil
		}
		id = c.ParentID
	}
}

// report delivers the outcome of a child to its parent, which it opens
// when this runtime does not run it, even when the child has settled. It
// never waits for the child.
func (r *Runtime) report(parent string, s eventlog.ChildSettled, text string) {
	r.group.Go(func() {
		r.sup.done(s.ChildID)
		if p, err := r.Open(r.base, parent); err == nil {
			_ = p.a.Settle(r.base, s, text)
		}
	})
}

// recoverChildren settles each unsettled child of a that has ended, or
// that never started, and opens each other one, which reports when its
// turn ends. Each opened child counts against the limits of its tree. A crash can come between the end of a child turn and its
// child.settled record.
func (r *Runtime) recoverChildren(parent string, a *session.Actor) {
	ids, err := a.Unsettled(r.base)
	if err != nil {
		return
	}
	root, _, err := r.lineage(r.base, parent)
	if err != nil {
		return
	}
	for _, id := range ids {
		st, err := session.Load(r.base, id, storeLog{r.store, id})
		if errors.Is(err, ErrSessionNotFound) {
			r.sup.done(id)
			_ = a.Settle(r.base, eventlog.ChildSettled{ChildID: id, Outcome: eventlog.OutcomeFailed}, "")
			continue
		}
		if err != nil {
			continue
		}
		if s, text, ok := session.Settlement(id, st); ok {
			r.sup.done(id)
			_ = a.Settle(r.base, s, text)
			continue
		}
		r.sup.adopt(root, id)
		_, _ = r.Open(r.base, id)
	}
}

// supervisor holds the limits of each session tree: the depth of a child,
// and the unsettled children of each root.
type supervisor struct {
	depth, running int

	mu    sync.Mutex
	roots map[string]string
}

func (s *supervisor) admit(root, child string, depth int) error {
	if depth > s.depth {
		return fmt.Errorf("max_task_depth %d allows no child at depth %d", s.depth, depth)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.roots {
		if r == root {
			n++
		}
	}
	if n >= s.running {
		return fmt.Errorf("max_concurrent_tasks %d: %d children of this session tree have not settled", s.running, n)
	}
	s.roots[child] = root
	return nil
}

func (s *supervisor) adopt(root, child string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roots[child] = root
}

func (s *supervisor) done(child string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.roots, child)
}
