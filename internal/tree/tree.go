// Package tree runs the child sessions of a session: the limits of a session
// tree, the spawn and the report of a child, the recovery of unsettled
// children, the tree interrupt, and the task tool.
package tree

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
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/session"
)

// Node is a session that the runtime runs, as the tree sees it.
type Node struct {
	Actor *session.Actor
	// Root is the first ancestor of the session, or its own ID, and Depth is
	// the number of its ancestors.
	Root  string
	Depth int
	// Recovered closes once the session has settled or opened each unsettled child.
	Recovered <-chan struct{}
}

// Sessions is the runtime that hosts the sessions of the trees.
type Sessions interface {
	// Running returns session id when the runtime runs it.
	Running(id string) (Node, bool)
	// Loaded returns session id once the runtime has loaded it. It reports
	// false when the runtime does not run id, or when ctx ends first.
	Loaded(ctx context.Context, id string) (Node, bool)
	// Open acquires and runs session id, and returns it as is when the
	// runtime already runs it.
	Open(ctx context.Context, id string) (Node, error)
	// Create starts session id with its first input. profile is the agent
	// profile that the caller has read for it.
	Create(ctx context.Context, id string, created eventlog.SessionCreated, first eventlog.InputAdmitted, profile prompt.Profile) error
	// Read runs f with the state of session id, which f must not keep. It
	// fails with session.ErrNotFound for a session with no log, or no valid ID.
	Read(ctx context.Context, id string, f func(*eventlog.State)) error
	// Created returns the session.created record of session id.
	Created(ctx context.Context, id string) (eventlog.SessionCreated, error)
	// Members returns the sessions of the tree of root that the runtime runs.
	Members(root string) []Node
	// Known reports whether a session at model can have a tool named name.
	Known(model, name string) bool
}

// Config sets the limits of a tree and what it takes from the runtime.
type Config struct {
	// MaxDepth is the deepest child. MaxRunning is the most unsettled
	// children of one tree. MaxTokens bounds the tokens of a tree; zero or
	// less turns the bound off.
	MaxDepth, MaxRunning, MaxTokens int
	// Base ends when the runtime closes. Go starts a goroutine that the
	// runtime waits for.
	Base context.Context
	Go   func(func())
	// Profiles reads the agent profiles. Resolve maps a model alias to its ref.
	// CheckModel reports why no child can run the model ref.
	Profiles   func() (map[string]prompt.Profile, error)
	Resolve    func(string) string
	CheckModel func(model string) error
	// Suffix returns a fresh random suffix for an ID.
	Suffix func() string
}

// Tree runs the child sessions of the sessions of one runtime.
type Tree struct {
	s   Sessions
	cfg Config

	mu     sync.Mutex
	locks  map[string]*treeLock
	quiet  map[string]int
	ending map[string]int
	muted  map[string]int
}

// New returns the Tree of runtime s.
func New(s Sessions, cfg Config) *Tree {
	return &Tree{s: s, cfg: cfg, locks: map[string]*treeLock{}, quiet: map[string]int{}, ending: map[string]int{}, muted: map[string]int{}}
}

// Choice is what a spawn takes from its caller beside the agent and the task:
// the model of the child, or "" for the model of its profile and then of its
// parent, and the effort of the child, or "" for the default of its provider.
type Choice struct{ Model, Effort string }

// spawn appends child.spawned to parent, then creates the child with task
// as its first input. A child that fails to start settles failed at once.
func (t *Tree) spawn(ctx context.Context, parent, agent, task string, ch Choice) (string, error) {
	profiles, err := t.cfg.Profiles()
	if err != nil {
		return "", fmt.Errorf("loading agent definitions: %w", err)
	}
	p, ok := profiles[agent]
	if !ok {
		return "", fmt.Errorf("unknown agent %q; the agents are %s", agent, strings.Join(slices.Sorted(maps.Keys(profiles)), ", "))
	}
	effort, err := message.ParseEffort(ch.Effort)
	if err != nil {
		return "", fmt.Errorf("invalid effort %q: %w", ch.Effort, err)
	}
	model := ch.Model
	if model == "" && p.Model != "" {
		model = t.cfg.Resolve(p.Model)
	}
	if model != "" {
		if err := t.cfg.CheckModel(model); err != nil {
			return "", err
		}
	}
	ps, ok := t.s.Running(parent)
	if !ok {
		return "", session.ErrNotOwned
	}
	id := "ses_" + t.cfg.Suffix()
	c, err := t.SpawnChild(ctx, ps, id, agent)
	if err != nil {
		return "", err
	}
	if model != "" {
		c.Model = model
	}
	c.Settings.Effort = string(effort)
	c.Origin, c.AllowedTools = "task", t.available(c.Model, narrow(p.Tools, c.AllowedTools))
	first := eventlog.InputAdmitted{InputID: "input_" + t.cfg.Suffix(), Delivery: eventlog.DeliveryQueue, Source: "parent",
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: task}}}
	ctx = context.WithoutCancel(ctx)
	if err := t.s.Create(ctx, id, c, first, p); err != nil {
		return "", errors.Join(err, ps.Actor.Settle(ctx, eventlog.ChildSettled{ChildID: id, Outcome: eventlog.OutcomeFailed}, nil))
	}
	return id, nil
}

// SpawnChild appends child.spawned to parent ps and returns the
// session.created record of the child, unless the tree of ps is at a limit.
// One lock for each tree makes the count of the unsettled children and the
// append one step. A child that ps already counts needs no new slot, so a
// second spawn of it, as two senders to one settled child make, passes.
func (t *Tree) SpawnChild(ctx context.Context, ps Node, child, agent string) (eventlog.SessionCreated, error) {
	if depth := ps.Depth + 1; depth > t.cfg.MaxDepth {
		return eventlog.SessionCreated{}, fmt.Errorf("max_task_depth %d allows no child at depth %d", t.cfg.MaxDepth, depth)
	}
	defer t.lock(ps.Root)()
	if !slices.Contains(ps.Actor.View().Unsettled, child) {
		if n := t.unsettled(ps.Root); n >= t.cfg.MaxRunning {
			return eventlog.SessionCreated{}, fmt.Errorf("max_concurrent_tasks %d: %d children of this session tree have not settled", t.cfg.MaxRunning, n)
		}
		if err := t.withinBudget(ctx, ps.Root); err != nil {
			return eventlog.SessionCreated{}, err
		}
	}
	return ps.Actor.Spawn(ctx, child, agent)
}

// unsettled returns how many children of the tree of root have not settled,
// over the sessions that the runtime runs.
func (t *Tree) unsettled(root string) int {
	n := 0
	for _, m := range t.s.Members(root) {
		n += len(m.Actor.View().Unsettled)
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
func (t *Tree) available(model string, names []string) []string {
	if names == nil {
		return nil
	}
	out := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !t.s.Known(model, n) })
	if len(out) == 0 && len(names) > 0 {
		slog.Warn("harness: the child has no tool of its profile", "model", model, "tools", names)
	}
	return out
}

// Lineage returns the root of the tree of session id, whose parent is parent,
// and the depth of id below it.
func (t *Tree) Lineage(ctx context.Context, id, parent string) (string, int, error) {
	if parent == "" {
		return id, 0, nil
	}
	if ps, ok := t.s.Running(parent); ok {
		return ps.Root, ps.Depth + 1, nil
	}
	up, err := t.ancestors(ctx, parent)
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
func (t *Tree) ancestors(ctx context.Context, id string) ([]string, error) {
	var up []string
	for {
		parent, err := t.parent(ctx, id)
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

// parent returns the parent of session id. A session that the runtime runs
// answers from its view; any other session answers from its session.created
// record.
func (t *Tree) parent(ctx context.Context, id string) (string, error) {
	if s, ok := t.s.Running(id); ok {
		return s.Actor.View().Session.ParentID, nil
	}
	c, err := t.s.Created(ctx, id)
	return c.ParentID, err
}

// withinBudget fails once the sessions of the tree of root have used
// max_tree_tokens. Each session counts the turns that ended in its log.
func (t *Tree) withinBudget(ctx context.Context, root string) error {
	if t.cfg.MaxTokens <= 0 {
		return nil
	}
	n, err := t.tokens(ctx, root)
	if err == nil && n >= int64(t.cfg.MaxTokens) {
		err = fmt.Errorf("max_tree_tokens %d: this session tree has used %d tokens", t.cfg.MaxTokens, n)
	}
	return err
}

// tokens returns the tokens that session id and its descendants have used.
func (t *Tree) tokens(ctx context.Context, id string) (int64, error) {
	var n int64
	var kids []string
	err := t.s.Read(ctx, id, func(st *eventlog.State) {
		u := st.Usage()
		n, kids = u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens, st.Children()
	})
	if errors.Is(err, session.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, kid := range kids {
		k, err := t.tokens(ctx, kid)
		if err != nil {
			return 0, err
		}
		n += k
	}
	return n, nil
}

// Report delivers the outcome of a child to its parent, which it opens
// when the runtime does not run it, even when the child has settled. It
// never waits for the child. A child that a tree interrupt stops settles
// with no report input. A child that an end walk marked reaches only a parent
// that the runtime runs, and never opens one: the end never opens a session,
// and a parent that has stopped settles the child when it opens.
func (t *Tree) Report(parent string, s eventlog.ChildSettled, report *session.Report) {
	quiet, ending := t.hushed(s.ChildID)
	if quiet {
		report = nil
		t.mute(s.ChildID, 1)
	}
	t.cfg.Go(func() {
		if quiet {
			defer t.mute(s.ChildID, -1)
		}
		if ending {
			if p, ok := t.s.Running(parent); ok {
				_ = p.Actor.Settle(t.cfg.Base, s, report)
			}
			return
		}
		if p, err := t.s.Open(t.cfg.Base, parent); err == nil {
			_ = p.Actor.Settle(t.cfg.Base, s, report)
		}
	})
}

// Recover settles each unsettled child of a that has ended, or that never
// started, and opens each other one, which reports when its turn ends. A
// crash can come between the end of a child turn and its child.settled
// record. A child whose last turn the end of a session stopped (cause ended),
// and a child whose silenced report Report is delivering, settle with no
// report input; every other child reports its outcome, a canceled one included.
func (t *Tree) Recover(a *session.Actor) {
	for _, id := range a.View().Unsettled {
		var s eventlog.ChildSettled
		var report *session.Report
		var ended bool
		err := t.s.Read(t.cfg.Base, id, func(st *eventlog.State) {
			s, report, ended = session.Settlement(id, st)
		})
		switch {
		case errors.Is(err, session.ErrNotFound):
			_ = a.Settle(t.cfg.Base, eventlog.ChildSettled{ChildID: id, Outcome: eventlog.OutcomeFailed}, nil)
		case err != nil:
		case ended:
			if t.muting(id) {
				report = nil
			}
			_ = a.Settle(t.cfg.Base, s, report)
		default:
			_, _ = t.s.Open(t.cfg.Base, id)
		}
	}
}

// treeLock is a channel, not a sync.Mutex, so that a spawner that waits for
// it is durably blocked in a synctest bubble.
type treeLock struct {
	held chan struct{}
	n    int
}

// lock holds the lock of the tree of root, and returns its unlock.
func (t *Tree) lock(root string) (unlock func()) {
	t.mu.Lock()
	l := t.locks[root]
	if l == nil {
		l = &treeLock{held: make(chan struct{}, 1)}
		t.locks[root] = l
	}
	l.n++
	t.mu.Unlock()
	l.held <- struct{}{}
	return func() {
		<-l.held
		t.mu.Lock()
		defer t.mu.Unlock()
		if l.n--; l.n == 0 {
			delete(t.locks, root)
		}
	}
}

// walkMark names the walks that stop a child: a quiet child settles with no
// report input; an ending child never opens its parent.
type walkMark struct{ quiet, ending bool }

// hush adds n to the walks that stop child.
func (t *Tree) hush(child string, h walkMark, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if h.quiet {
		count(t.quiet, child, n)
	}
	if h.ending {
		count(t.ending, child, n)
	}
}

func (t *Tree) hushed(child string) (quiet, ending bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.quiet[child] > 0, t.ending[child] > 0
}

// mute adds n to the silenced reports of child that Report has not settled yet.
// Report runs before any read can show the child as ended, so Recover that
// reads it ended also sees the hold.
func (t *Tree) mute(child string, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	count(t.muted, child, n)
}

func (t *Tree) muting(child string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.muted[child] > 0
}

func count(by map[string]int, child string, n int) {
	if by[child] += n; by[child] <= 0 {
		delete(by, child)
	}
}
