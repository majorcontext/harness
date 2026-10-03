// Package turn runs one agent turn against a Backend.
package turn

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
)

// ErrRetryable marks a backend error that a new attempt of the turn can fix.
var ErrRetryable = errors.New("turn: retryable backend error")

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
	Settings  eventlog.Settings
	// Input holds the inputs that started the turn, as user messages.
	Input []eventlog.Message
	// History is the conversation that the model sees, Input included.
	History []eventlog.Message
	// Resumed counts the resumes of a turn suspended by a handoff; 0 for a new turn.
	Resumed int
}

// Delta is a piece of an item that is not complete yet.
type Delta struct {
	// Type is eventlog.PartText or eventlog.PartReasoning.
	Type string
	Text string
}

// Telemetry is what a backend measured during a turn.
type Telemetry struct {
	Usage eventlog.Usage
}

// Sink receives the items of a running turn.
type Sink interface {
	// Item records one completed message. An error stops the turn.
	Item(m eventlog.Message) error
	// Delta streams a piece of the item that the backend calls itemID.
	Delta(itemID string, d Delta)
	// Telemetry adds a measurement to the turn.
	Telemetry(t Telemetry)
	// Steer takes the queued steer inputs into the turn. A backend with
	// Steering calls it at each item boundary and must use each message.
	Steer() ([]eventlog.Message, error)
}

// Result is the outcome of a turn that returned.
type Result struct{}

// Reporter takes no ctx: a backend reports items after the turn's ctx ends.
type Reporter interface {
	Item(turnID string, m eventlog.Message) error
	Telemetry(turnID string, t Telemetry)
	Steer(turnID string) ([]eventlog.Message, error)
	Ended(turnID string, err error)
}

const retryBackoff = 200 * time.Millisecond

// Run runs req on b and reports its items and its end to to. It runs the
// turn again, at most retries times, after an ErrRetryable error that came
// before any item.
func Run(ctx context.Context, b Backend, req Request, to Reporter, retries int) {
	s := &sink{turnID: req.TurnID, to: to}
	_, err := b.Run(ctx, req, s)
	for attempt := 0; attempt < retries && s.items == 0 && errors.Is(err, ErrRetryable); attempt++ {
		if err = wait(ctx, attempt); err == nil {
			_, err = b.Run(ctx, req, s)
		}
	}
	to.Ended(req.TurnID, err)
}

func wait(ctx context.Context, attempt int) error {
	d := retryBackoff << attempt
	t := time.NewTimer(d + rand.N(d/5) - d/10)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

type sink struct {
	turnID string
	to     Reporter
	items  int
}

func (s *sink) Item(m eventlog.Message) error {
	s.items++
	return s.to.Item(s.turnID, m)
}

// Delta drops d: no subscriber reads ephemeral frames.
func (*sink) Delta(string, Delta) {}

func (s *sink) Telemetry(t Telemetry) { s.to.Telemetry(s.turnID, t) }

func (s *sink) Steer() ([]eventlog.Message, error) { return s.to.Steer(s.turnID) }
