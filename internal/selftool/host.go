package selftool

import (
	"context"

	"github.com/majorcontext/harness/protocol"
)

// Sessions is the runtime that runs the sessions of the tools.
type Sessions interface {
	Session(id string) (Session, bool)
}

// Session is a session that the runtime runs, as the tools see it.
// StartGoal fails with session.ErrGoalActive, and AdjustGoal with
// session.ErrNoGoal, as the session actor does.
type Session interface {
	Snapshot() protocol.Session
	Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)
	DeferModel(ctx context.Context, ref string) error
	PendingModel(ctx context.Context) (string, error)
	StartGoal(ctx context.Context, condition string) error
	AdjustGoal(ctx context.Context, condition string) error
}
