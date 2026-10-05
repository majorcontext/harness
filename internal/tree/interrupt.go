package tree

import (
	"context"
	"errors"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
)

// cancelTurn withdraws the queued inputs of session id and stops its turn,
// when this runtime runs it. For the end of an ancestor the turn ends with
// cause ended.
func (t *Tree) cancelTurn(ctx context.Context, id string, end bool) error {
	s, ok := t.s.Loaded(ctx, id)
	if !ok {
		return nil
	}
	cancel := (*session.Actor).Cancel
	if end {
		cancel = (*session.Actor).CancelForEnd
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
	return t.walk(ctx, id, stop, false)
}

// End is Interrupt for the end of session id: each descendant turn that it
// stops ends with cause ended. It silences no child of id, so a report that
// lands while stop refuses the end still reaches id; it marks them, so none
// opens id again once stop has released it.
func (t *Tree) End(ctx context.Context, id string, stop func(context.Context) error) error {
	return t.walk(ctx, id, stop, true)
}

func (t *Tree) walk(ctx context.Context, id string, stop func(context.Context) error, end bool) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	defer context.AfterFunc(t.cfg.Base, cancel)()
	marked := map[string]walkMark{}
	defer func() {
		for kid, m := range marked {
			t.hush(kid, m, -1)
		}
	}()
	return t.stopTree(ctx, id, stop, marked, end, true)
}

func (t *Tree) stopTree(ctx context.Context, id string, stop func(context.Context) error, marked map[string]walkMark, end, top bool) error {
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
				m := walkMark{quiet: !end || !top, ending: end}
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
		err = errors.Join(err, t.stopTree(ctx, kid, func(ctx context.Context) error { return t.cancelTurn(ctx, kid, end) }, marked, end, false))
	}
	return err
}
