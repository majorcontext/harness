package harness

import (
	"context"
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
)

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
	if _, err := r.sup.admit(root, id, depth+1); err != nil {
		return "", err
	}
	if err := r.withinBudget(ctx, root); err != nil {
		r.sup.done(id)
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
	caps := r.models.Capabilities(model)
	has := func(n string) bool {
		return slices.Contains(caps.Tools, n) || !caps.OwnsLoop && r.builtin(n) || r.mcp != nil && mcpsrc.Reserved(n) ||
			slices.ContainsFunc(r.tools, func(t turn.Tool) bool { return t.Spec().Name == n })
	}
	out := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !has(n) })
	if len(out) == 0 && len(names) > 0 {
		slog.Warn("harness: the child has no tool of its profile", "model", model, "tools", names)
	}
	return out
}

// lineage returns the root of the tree of session id and the depth of id below it.
func (r *Runtime) lineage(ctx context.Context, id string) (string, int, error) {
	up, err := r.ancestors(ctx, id)
	if err != nil || len(up) == 0 {
		return id, 0, err
	}
	return up[len(up)-1], len(up), nil
}

// ancestors returns the parents of session id, nearest first. It reads the
// session.created record of each, up to the root.
func (r *Runtime) ancestors(ctx context.Context, id string) ([]string, error) {
	var up []string
	for {
		c, err := r.created(ctx, id)
		if err != nil {
			return nil, err
		}
		if c.ParentID == "" {
			break
		}
		id = c.ParentID
		up = append(up, id)
	}
	return up, nil
}

// created returns the session.created record of session id.
func (r *Runtime) created(ctx context.Context, id string) (eventlog.SessionCreated, error) {
	recs, err := r.store.Read(ctx, id, 0, 1)
	if err == nil && len(recs) == 0 {
		err = fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	if err != nil {
		return eventlog.SessionCreated{}, err
	}
	env, err := eventlog.Decode(recs[0].Data)
	if err != nil {
		return eventlog.SessionCreated{}, err
	}
	c, _ := env.Event.(eventlog.SessionCreated)
	return c, nil
}

// withinBudget fails once the sessions of the tree of root have used
// max_tree_tokens. Each session counts the turns that ended in its log.
func (r *Runtime) withinBudget(ctx context.Context, root string) error {
	if r.sup.tokens <= 0 {
		return nil
	}
	n, err := r.tokens(ctx, root)
	if err == nil && n >= int64(r.sup.tokens) {
		err = fmt.Errorf("max_tree_tokens %d: this session tree has used %d tokens", r.sup.tokens, n)
	}
	return err
}

// tokens returns the tokens that session id and its descendants have used.
func (r *Runtime) tokens(ctx context.Context, id string) (int64, error) {
	st, err := session.Load(ctx, id, storeLog{r.store, id})
	if errors.Is(err, ErrSessionNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	u := st.Usage()
	n := u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
	for _, kid := range st.Children() {
		k, err := r.tokens(ctx, kid)
		if err != nil {
			return 0, err
		}
		n += k
	}
	return n, nil
}

// report delivers the outcome of a child to its parent, which it opens
// when this runtime does not run it, even when the child has settled. It
// never waits for the child. A child that a tree interrupt stops settles
// with no report input, so no parent inside the tree starts a turn.
func (r *Runtime) report(parent string, s eventlog.ChildSettled, text string) {
	if r.sup.quieted(s.ChildID) {
		text = ""
	}
	r.group.Go(func() {
		r.sup.done(s.ChildID)
		if p, err := r.Open(r.base, parent); err == nil {
			_ = p.a.Settle(r.base, s, text)
		}
	})
}

// recoverChildren settles each unsettled child of a that has ended, or
// that never started, and opens each other one, which reports when its
// turn ends. Each opened child counts against the limits of its tree. A
// crash can come between the end of a child turn and its child.settled
// record.
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
// the unsettled children of each root, and the token budget. It also holds
// the children that a tree interrupt stops.
type supervisor struct {
	depth, running, tokens int

	mu    sync.Mutex
	roots map[string]string
	quiet map[string]int
}

// admit counts child against the limits of root. added is false for a
// child that the supervisor already counts.
func (s *supervisor) admit(root, child string, depth int) (added bool, err error) {
	if depth > s.depth {
		return false, fmt.Errorf("max_task_depth %d allows no child at depth %d", s.depth, depth)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.roots[child]; ok {
		return false, nil
	}
	n := 0
	for _, r := range s.roots {
		if r == root {
			n++
		}
	}
	if n >= s.running {
		return false, fmt.Errorf("max_concurrent_tasks %d: %d children of this session tree have not settled", s.running, n)
	}
	s.roots[child] = root
	return true, nil
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

// silence adds n to the tree interrupts that stop child.
func (s *supervisor) silence(child string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quiet[child] += n; s.quiet[child] <= 0 {
		delete(s.quiet, child)
	}
}

func (s *supervisor) quieted(child string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quiet[child] > 0
}
