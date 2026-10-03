// Package turn runs one agent turn against a Backend.
package turn

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
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

// Tool is a tool that the loop runs for a backend that does not own the loop.
type Tool interface {
	Spec() protocol.ToolSpec
	Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error)
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
	// Tools describes the tools that the model may call.
	Tools []protocol.ToolSpec
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

// Run runs req on b and reports its items and its end to to. Only tools
// reach the model. When b does not own the loop, Run runs the tool calls of
// each model call in order, then calls b again, until a call asks for no
// tool. Model calls run under step and tools under ctx: when only step
// ends, a running tool finishes and no new tool starts.
func Run(ctx, step context.Context, b Backend, req Request, tools []Tool, to Reporter, retries int) {
	to.Ended(req.TurnID, run(ctx, step, b, req, tools, to, retries))
}

func run(ctx, step context.Context, b Backend, req Request, tools []Tool, to Reporter, retries int) error {
	for _, t := range tools {
		req.Tools = append(req.Tools, t.Spec())
	}
	loop := !b.Capabilities(req.Model).OwnsLoop
	for {
		if step.Err() != nil {
			return context.Cause(step)
		}
		s := &sink{turnID: req.TurnID, to: to}
		if err := callModel(step, b, req, s, retries); err != nil {
			return err
		}
		calls := toolCalls(s.items)
		if !loop || len(calls) == 0 {
			return nil
		}
		req.History = append(req.History, s.items...)
		for _, c := range calls {
			if step.Err() != nil {
				return context.Cause(step)
			}
			m := result(c, runTool(ctx, tools, c))
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			if err := to.Item(req.TurnID, m); err != nil {
				return err
			}
			req.History = append(req.History, m)
		}
	}
}

// callModel runs one model call. It calls b again, at most retries times,
// after an ErrRetryable error that came before any item.
func callModel(ctx context.Context, b Backend, req Request, s *sink, retries int) error {
	_, err := b.Run(ctx, req, s)
	for n := 0; n < retries && len(s.items) == 0 && errors.Is(err, ErrRetryable); n++ {
		if err = wait(ctx, n); err == nil {
			_, err = b.Run(ctx, req, s)
		}
	}
	return err
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

// sink keeps the items and steer inputs of one model call in order.
type sink struct {
	turnID string
	to     Reporter
	items  []eventlog.Message
}

func (s *sink) Item(m eventlog.Message) error {
	if err := s.to.Item(s.turnID, m); err != nil {
		return err
	}
	s.items = append(s.items, m)
	return nil
}

// Delta drops d: no subscriber reads ephemeral frames.
func (*sink) Delta(string, Delta) {}

func (s *sink) Telemetry(t Telemetry) { s.to.Telemetry(s.turnID, t) }

func (s *sink) Steer() ([]eventlog.Message, error) {
	in, err := s.to.Steer(s.turnID)
	s.items = append(s.items, in...)
	return in, err
}
