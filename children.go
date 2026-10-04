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
)

// spawn appends child.spawned to parent, then creates the child with task
// as its first input. A child that fails to start settles failed at once.
func (r *Runtime) spawn(ctx context.Context, parent, agent, task string) (string, error) {
	profiles := prompt.Profiles(r.agentDirs)
	p, ok := profiles[agent]
	if !ok {
		return "", fmt.Errorf("unknown agent %q; the agents are %s", agent, strings.Join(slices.Sorted(maps.Keys(profiles)), ", "))
	}
	ps := r.running(parent)
	if ps == nil {
		return "", ErrSessionNotOwned
	}
	id := "ses_" + newSuffix()
	c, err := r.spawnChild(ctx, ps, id, agent)
	if err != nil {
		return "", err
	}
	if p.Model != "" {
		c.Model = r.resolve(p.Model)
	}
	c.Origin, c.AllowedTools = "task", r.available(c.Model, narrow(p.Tools, c.AllowedTools))
	first := eventlog.InputAdmitted{InputID: "input_" + newSuffix(), Delivery: eventlog.DeliveryQueue, Source: "parent",
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: task}}}
	ctx = context.WithoutCancel(ctx)
	if _, err := r.create(ctx, id, launch{created: &c, first: &first, profile: &p}); err != nil {
		return "", errors.Join(err, ps.a.Settle(ctx, eventlog.ChildSettled{ChildID: id, Outcome: eventlog.OutcomeFailed}, ""))
	}
	return id, nil
}

// spawnChild appends child.spawned to parent ps and returns the
// session.created record of the child, unless the tree of ps is at a limit.
// One lock for each tree makes the count of the unsettled children and the
// append one step. A child that ps already counts needs no new slot, so a
// second spawn of it, as two senders to one settled child make, passes.
func (r *Runtime) spawnChild(ctx context.Context, ps *Session, child, agent string) (eventlog.SessionCreated, error) {
	if depth := ps.depth + 1; depth > r.sup.depth {
		return eventlog.SessionCreated{}, fmt.Errorf("max_task_depth %d allows no child at depth %d", r.sup.depth, depth)
	}
	defer r.sup.lock(ps.root)()
	if !slices.Contains(ps.a.View().Unsettled, child) {
		if n := r.unsettled(ps.root); n >= r.sup.running {
			return eventlog.SessionCreated{}, fmt.Errorf("max_concurrent_tasks %d: %d children of this session tree have not settled", r.sup.running, n)
		}
		if err := r.withinBudget(ctx, ps.root); err != nil {
			return eventlog.SessionCreated{}, err
		}
	}
	return ps.a.Spawn(ctx, child, agent)
}

// unsettled returns how many children of the tree of root have not settled,
// over the sessions that this runtime runs.
func (r *Runtime) unsettled(root string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.sessions {
		select {
		case <-e.ready:
			if e.s != nil && e.s.root == root {
				n += len(e.s.a.View().Unsettled)
			}
		default:
		}
	}
	return n
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

// available drops each name that the child has no tool for, because a
// profile can name the tools of every backend.
func (r *Runtime) available(model string, names []string) []string {
	if names == nil {
		return nil
	}
	out := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !r.known(model, n) })
	if len(out) == 0 && len(names) > 0 {
		slog.Warn("harness: the child has no tool of its profile", "model", model, "tools", names)
	}
	return out
}

// tree returns the root of the tree of session id, whose parent is parent,
// and the depth of id below it.
func (r *Runtime) tree(ctx context.Context, id, parent string) (string, int, error) {
	if parent == "" {
		return id, 0, nil
	}
	if ps := r.running(parent); ps != nil {
		return ps.root, ps.depth + 1, nil
	}
	up, err := r.ancestors(ctx, parent)
	if err != nil {
		return "", 0, err
	}
	root := parent
	if len(up) > 0 {
		root = up[len(up)-1]
	}
	return root, len(up) + 1, nil
}

// ancestors returns the parents of session id, nearest first, up to the root.
func (r *Runtime) ancestors(ctx context.Context, id string) ([]string, error) {
	var up []string
	for {
		parent, err := r.parent(ctx, id)
		if err != nil {
			return nil, err
		}
		if parent == "" {
			break
		}
		id = parent
		up = append(up, id)
	}
	return up, nil
}

// parent returns the parent of session id. A session that this runtime runs
// answers from its view; any other session answers from its session.created
// record.
func (r *Runtime) parent(ctx context.Context, id string) (string, error) {
	if s := r.running(id); s != nil {
		return s.a.View().Session.ParentID, nil
	}
	c, err := r.created(ctx, id)
	return c.ParentID, err
}

// created returns the session.created record of session id, which is its
// first record.
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
	c, ok := env.Event.(eventlog.SessionCreated)
	if !ok {
		return c, fmt.Errorf("harness: session %s starts with %s, not session.created", id, env.Event.Kind())
	}
	return c, nil
}

// read runs f with the state of session id, which f must not keep. A session
// that this runtime runs answers through its actor; any other session
// replays from the store.
func (r *Runtime) read(ctx context.Context, id string, f func(*eventlog.State)) error {
	if s := r.running(id); s != nil {
		if err := s.a.Read(ctx, f); !errors.Is(err, ErrSessionNotOwned) {
			return err
		}
	}
	st, err := session.Load(ctx, id, storeLog{r.store, id})
	if err != nil {
		return err
	}
	f(st)
	return nil
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
	var n int64
	var kids []string
	err := r.read(ctx, id, func(st *eventlog.State) {
		u := st.Usage()
		n, kids = u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens, st.Children()
	})
	if errors.Is(err, ErrSessionNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, kid := range kids {
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
		if p, err := r.Open(r.base, parent); err == nil {
			_ = p.a.Settle(r.base, s, text)
		}
	})
}

// recoverChildren settles each unsettled child of a that has ended, or
// that never started, and opens each other one, which reports when its
// turn ends. A crash can come between the end of a child turn and its
// child.settled record.
func (r *Runtime) recoverChildren(a *session.Actor) {
	for _, id := range a.View().Unsettled {
		var s eventlog.ChildSettled
		var text string
		var ended bool
		err := r.read(r.base, id, func(st *eventlog.State) { s, text, ended = session.Settlement(id, st) })
		switch {
		case errors.Is(err, ErrSessionNotFound):
			_ = a.Settle(r.base, eventlog.ChildSettled{ChildID: id, Outcome: eventlog.OutcomeFailed}, "")
		case err != nil:
		case ended:
			_ = a.Settle(r.base, s, text)
		default:
			_, _ = r.Open(r.base, id)
		}
	}
}

// supervisor holds the limits of each session tree: the depth of a child,
// the unsettled children of each root, and the token budget. It also holds
// the children that a tree interrupt stops.
type supervisor struct {
	depth, running, tokens int

	mu    sync.Mutex
	locks map[string]*treeLock
	quiet map[string]int
}

// treeLock is a channel, not a sync.Mutex, so that a spawner that waits for
// it is durably blocked in a synctest bubble.
type treeLock struct {
	held chan struct{}
	n    int
}

// lock holds the lock of the tree of root, and returns its unlock.
func (s *supervisor) lock(root string) (unlock func()) {
	s.mu.Lock()
	l := s.locks[root]
	if l == nil {
		l = &treeLock{held: make(chan struct{}, 1)}
		s.locks[root] = l
	}
	l.n++
	s.mu.Unlock()
	l.held <- struct{}{}
	return func() {
		<-l.held
		s.mu.Lock()
		defer s.mu.Unlock()
		if l.n--; l.n == 0 {
			delete(s.locks, root)
		}
	}
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
