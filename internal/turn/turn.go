// Package turn runs one agent turn against a Backend.
package turn

import (
	"context"

	"github.com/majorcontext/harness/internal/eventlog"
)

// Backend runs turns against one kind of model or agent harness.
type Backend interface {
	Capabilities(model string) Capabilities
	Run(ctx context.Context, req Request, out Sink) (Result, error)
}

// Capabilities says what a backend does for a model.
type Capabilities struct {
	OwnsLoop      bool
	OwnsContext   bool
	Steering      bool
	ContextWindow int
}

// Request is one turn to run.
type Request struct {
	SessionID string
	TurnID    string
	Model     string
	// Input holds the inputs that started the turn, as user messages.
	Input []eventlog.Message
	// Resumed counts the resumes of a turn suspended by a handoff; 0 for a new turn.
	Resumed int
	// Steer carries the steer inputs folded into the turn. It is nil unless
	// the backend has Steering.
	Steer <-chan eventlog.Message
}

// Sink receives the items of a running turn.
type Sink interface {
	// Item records one completed message. An error stops the turn.
	Item(m eventlog.Message) error
}

// Result is the outcome of a turn that returned.
type Result struct {
	Usage eventlog.Usage
}

// Reporter receives what one turn produces. The session actor implements it.
type Reporter interface {
	Item(ctx context.Context, turnID string, m eventlog.Message) error
	Ended(turnID string, r Result, err error)
}

// Run runs req on b and reports each item and the end of the turn to to.
func Run(ctx context.Context, b Backend, req Request, to Reporter) {
	r, err := b.Run(ctx, req, sink{ctx, req.TurnID, to})
	to.Ended(req.TurnID, r, err)
}

type sink struct {
	ctx    context.Context
	turnID string
	to     Reporter
}

func (s sink) Item(m eventlog.Message) error { return s.to.Item(s.ctx, s.turnID, m) }
