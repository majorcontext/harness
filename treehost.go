package harness

import (
	"context"
	"fmt"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/tree"
)

// host is the Runtime as the child tree sees it.
type host struct{ r *Runtime }

func node(s *Session) tree.Node {
	return tree.Node{Actor: s.a, Root: s.root, Depth: s.depth, Recovered: s.recovered}
}

func (h host) Running(id string) (tree.Node, bool) {
	if s := h.r.running(id); s != nil {
		return node(s), true
	}
	return tree.Node{}, false
}

func (h host) Loaded(ctx context.Context, id string) (tree.Node, bool) {
	if s := h.r.loaded(ctx, id); s != nil {
		return node(s), true
	}
	return tree.Node{}, false
}

func (h host) Open(ctx context.Context, id string) (tree.Node, error) {
	s, err := h.r.Open(ctx, id)
	if err != nil {
		return tree.Node{}, err
	}
	return node(s), nil
}

func (h host) Create(ctx context.Context, id string, c eventlog.SessionCreated, first eventlog.InputAdmitted, p prompt.Profile) error {
	_, err := h.r.create(ctx, id, launch{created: &c, first: &first, profile: &p})
	return err
}

func (h host) Read(ctx context.Context, id string, f func(*eventlog.State)) error {
	if err := h.valid(id); err != nil {
		return err
	}
	return h.r.read(ctx, id, f)
}

func (h host) Created(ctx context.Context, id string) (eventlog.SessionCreated, error) {
	if err := h.valid(id); err != nil {
		return eventlog.SessionCreated{}, err
	}
	return h.r.created(ctx, id)
}

// valid reports a session ID that no store holds as a session with no log.
func (h host) valid(id string) error {
	if checkName("session", id) != nil {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return nil
}

func (h host) Members(root string) []tree.Node {
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	var out []tree.Node
	for _, e := range h.r.sessions {
		select {
		case <-e.ready:
			if e.s != nil && e.s.root == root {
				out = append(out, node(e.s))
			}
		default:
		}
	}
	return out
}

func (h host) Known(model, name string) bool { return h.r.known(model, name) }
