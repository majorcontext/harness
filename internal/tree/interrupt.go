package tree

import (
	"context"
	"errors"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
)

// cancelTurn withdraws the queued inputs of session id and stops its turn,
// when this runtime runs it.
func (t *Tree) cancelTurn(ctx context.Context, id string) error {
	return t.cancelWith(ctx, id, (*session.Actor).Cancel)
}

// endTurn is cancelTurn for a session that the end of an ancestor stops: the
// turn ends with cause ended.
func (t *Tree) endTurn(ctx context.Context, id string) error {
	return t.cancelWith(ctx, id, (*session.Actor).CancelForEnd)
}

func (t *Tree) cancelWith(ctx context.Context, id string, cancel func(*session.Actor, context.Context) error) error {
	s, ok := t.s.Loaded(ctx, id)
	if !ok {
		return nil
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
	return t.walk(ctx, id, stop, t.cancelTurn)
}

// End is Interrupt for the end of session id: each descendant turn that it
// stops ends with cause ended, so no later open of a parent reports it.
func (t *Tree) End(ctx context.Context, id string, stop func(context.Context) error) error {
	return t.walk(ctx, id, stop, t.endTurn)
}

func (t *Tree) walk(ctx context.Context, id string, stop func(context.Context) error, cancelTurn func(context.Context, string) error) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	defer context.AfterFunc(t.cfg.Base, cancel)()
	quiet := map[string]bool{}
	defer func() {
		for kid := range quiet {
			t.silence(kid, -1)
		}
	}()
	return t.stopTree(ctx, id, stop, quiet, cancelTurn)
}

func (t *Tree) stopTree(ctx context.Context, id string, stop func(context.Context) error, quiet map[string]bool, cancelTurn func(context.Context, string) error) error {
	silence := func() ([]string, error) {
		var kids []string
		err := t.s.Read(ctx, id, func(st *eventlog.State) { kids = st.Children() })
		if errors.Is(err, session.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, kid := range kids {
			if !quiet[kid] {
				quiet[kid] = true
				t.silence(kid, 1)
			}
		}
		return kids, nil
	}
	if _, err := silence(); err != nil {
		return err
	}
	if err := stop(ctx); err != nil {
		return err
	}
	kids, err := silence()
	for _, kid := range kids {
		err = errors.Join(err, t.stopTree(ctx, kid, func(ctx context.Context) error { return cancelTurn(ctx, kid) }, quiet, cancelTurn))
	}
	return err
}
