package harness

import (
	"context"

	"github.com/majorcontext/harness/internal/selftool"
)

// toolHost is the Runtime as the session tools see it.
type toolHost struct{ r *Runtime }

func (h toolHost) Session(id string) (selftool.Session, bool) {
	if s := h.r.running(id); s != nil {
		return ownedSession{s}, true
	}
	return nil, false
}

// ownedSession adds the goal commands of the session actor to a Session.
type ownedSession struct{ *Session }

func (s ownedSession) StartGoal(ctx context.Context, condition string) error {
	if err := s.r.checkEvaluator(); err != nil {
		return err
	}
	return s.a.StartGoal(ctx, condition)
}

func (s ownedSession) DeferModel(ctx context.Context, ref string) (string, string, error) {
	return s.a.DeferModel(ctx, ref)
}

func (s ownedSession) ModelState(ctx context.Context) (string, string, error) {
	return s.a.ModelState(ctx)
}

func (s ownedSession) AdjustGoal(ctx context.Context, condition string) error {
	if err := s.r.checkEvaluator(); err != nil {
		return err
	}
	return s.a.AdjustGoal(ctx, condition)
}
