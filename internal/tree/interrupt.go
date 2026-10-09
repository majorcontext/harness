package tree

import (
	"context"
	"errors"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
)

// cancelTurn withdraws the queued inputs of session id and stops its turn,
// when this runtime runs it. For the end of an ancestor the turn ends with
// cause ended, and for a task cancel with the task_cancel mark.
func (t *Tree) cancelTurn(ctx context.Context, id string, k walkKind) error {
	s, ok := t.s.Loaded(ctx, id)
	if !ok {
		return nil
	}
	cancel := (*session.Actor).Cancel
	switch {
	case k.end:
		cancel = (*session.Actor).CancelForEnd
	case k.route != nil:
		cancel = (*session.Actor).CancelForTask
	}
	if err := cancel(s.Actor, ctx); !errors.Is(err, session.ErrNotOwned) {
		return err
	}
	return nil
}

// Interrupt stops session id by stop, then each descendant that this
// runtime runs, and withdraws the queued inputs of each descendant. It
// silences the children of each session before it stops that session, so
// no parent inside the tree starts a turn on a report. The walk outlives
// ctx, so a caller that leaves does not stop half of a tree.
func (t *Tree) Interrupt(ctx context.Context, id string, stop func(context.Context) error) error {
	return t.walk(ctx, id, stop, walkKind{})
}

// Cancel stops session id and its descendants as Interrupt does, for the
// cancel action of the task tool. up holds the ancestors of id, nearest first.
// The report of id and of each descendant goes to the first of them whose turn
// has not ended with an outcome when the report is delivered, else to
// fallback. A parent that is not that session settles the child with no report
// input.
func (t *Tree) Cancel(ctx context.Context, id string, up []string, fallback string) error {
	k := walkKind{route: &route{up: up, fallback: fallback}}
	return t.walk(ctx, id, func(ctx context.Context) error { return t.cancelTurn(ctx, id, k) }, k)
}

// End is Interrupt for the end of session id: each descendant turn that it
// stops ends with cause ended. It silences no child of id, so a report that
// lands while stop refuses the end still reaches id; it marks them, so none
// opens id again once stop has released it.
func (t *Tree) End(ctx context.Context, id string, stop func(context.Context) error) error {
	return t.walk(ctx, id, stop, walkKind{end: true})
}

// walkKind is how a walk stops a tree: an end walk stops each turn with cause
// ended, and a walk with a route sends the report of the session that it
// stops, and of each descendant, along it.
type walkKind struct {
	end   bool
	route *route
}

func (t *Tree) walk(ctx context.Context, id string, stop func(context.Context) error, k walkKind) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	defer context.AfterFunc(t.cfg.Base, cancel)()
	marked := map[string]walkMark{}
	defer func() {
		for kid, m := range marked {
			t.hush(kid, m, -1)
		}
	}()
	if k.route != nil {
		marked[id] = walkMark{route: k.route}
		t.hush(id, marked[id], 1)
	}
	return t.stopTree(ctx, id, stop, marked, k, true)
}

func (t *Tree) stopTree(ctx context.Context, id string, stop func(context.Context) error, marked map[string]walkMark, k walkKind, top bool) error {
	mark := func() ([]string, error) {
		var kids []string
		err := t.s.Read(ctx, id, func(st *eventlog.State) { kids = st.Children() })
		if errors.Is(err, session.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, kid := range kids {
			if _, ok := marked[kid]; !ok {
				m := walkMark{quiet: !k.end || !top, ending: k.end, route: k.route}
				marked[kid] = m
				t.hush(kid, m, 1)
			}
		}
		return kids, nil
	}
	if _, err := mark(); err != nil {
		return err
	}
	if err := stop(ctx); err != nil {
		return err
	}
	kids, err := mark()
	for _, kid := range kids {
		err = errors.Join(err, t.stopTree(ctx, kid, func(ctx context.Context) error { return t.cancelTurn(ctx, kid, k) }, marked, k, false))
	}
	return err
}
