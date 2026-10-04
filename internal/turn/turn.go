// Package turn runs one agent turn against a Backend.
package turn

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// ErrRetryable marks a backend error that a new attempt of the turn can fix.
var ErrRetryable = errors.New("turn: retryable backend error")

// ErrContextOverflow marks a model call whose request passes the context window.
var ErrContextOverflow = errors.New("turn: context overflow")

// ErrExhausted marks a usage limit of the provider, such as a spent quota.
var ErrExhausted = errors.New("turn: provider usage limit reached")

// errStalled ends a model call that reports nothing for Limits.Idle.
var errStalled = fmt.Errorf("%w: turn: the model stream stalled", ErrRetryable)

// ErrHandoff is the cause of a turn context that a handoff ended. The next
// owner resumes the turn.
var ErrHandoff = errors.New("harness: turn handed off")

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
	// Tools names the built-in tools of a backend that owns the loop. A
	// session of that backend may allow only these and the embedder tools.
	Tools []string
}

// Tool is an embedder tool. The loop runs it, or Request.Call for a backend
// that owns the loop.
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
	// Instructions is the system prompt of the call.
	Instructions string
	// Input holds the inputs that started the turn, as user messages.
	Input []eventlog.Message
	// History is the conversation that the model sees, Input included.
	History []eventlog.Message
	// Tools describes the tools that the model may call.
	Tools []protocol.ToolSpec
	// Call runs a call to one of Tools. A backend that owns the loop calls it.
	Call func(ctx context.Context, c protocol.ToolCall) protocol.ToolResult
	// Resumed counts the resumes of a turn suspended by a handoff; 0 for a new turn.
	Resumed int
	// AllowedTools restricts the tools of the turn. nil keeps every tool;
	// an empty, non-nil list keeps none.
	AllowedTools []string
	// Steered receives a value when a steer input waits for Sink.Steer. It
	// is nil when the backend does not accept steering.
	Steered <-chan struct{}
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
	// Context is a context reading; the zero value is none.
	Context eventlog.ContextMeasured
}

// Sink receives the items of a running turn.
type Sink interface {
	// Item records one completed message. An error stops the turn.
	Item(m eventlog.Message) error
	// Delta streams a piece of the item that the backend calls itemID.
	Delta(itemID string, d Delta)
	// Alive reports model output that is not a delta yet, such as tool
	// arguments that still stream.
	Alive()
	// Telemetry adds a measurement to the turn.
	Telemetry(t Telemetry)
	// Steer takes the queued steer inputs into the turn. A backend with
	// Steering calls it at each item boundary and must use each message.
	Steer() ([]eventlog.Message, error)
	// State returns the newest state blob that backend saved, or nil.
	State(backend string) ([]byte, error)
	// SaveState records blob as the newest state of backend.
	SaveState(backend string, blob []byte) error
	// Compacted records that the backend compacted its own context.
	Compacted(summary string) error
}

// Result is the outcome of a Run that returned.
type Result struct {
	// MaxTokens reports a response that the output cap cut off.
	MaxTokens bool
}

// Limits bounds how a turn recovers from a failed or cut-off model call.
type Limits struct {
	// Retries bounds the new attempts of a model call after an ErrRetryable error.
	Retries int
	// Continuations bounds the model calls that continue a response cut off at max_tokens.
	Continuations int
	// Idle stops a model call that reports nothing to its Sink for this
	// long. Zero or less: no limit.
	Idle time.Duration
}

// Reporter takes no ctx, except CompactTurn: a backend reports items after
// the turn's ctx ends.
type Reporter interface {
	// Item records m under itemID, or under a new ID when itemID is empty.
	Item(turnID, itemID string, m eventlog.Message) error
	// Started announces a new item to live subscribers and returns its ID.
	Started(turnID string) string
	Delta(turnID, itemID string, d Delta)
	Status(turnID string, f protocol.StatusFrame)
	Telemetry(turnID string, t Telemetry)
	Steer(turnID string) ([]eventlog.Message, error)
	State(turnID, backend string) ([]byte, error)
	SaveState(turnID, backend string, blob []byte) error
	Compacted(turnID, summary string) error
	// CompactTurn folds the turns before the newest kept turns into a
	// summary while turnID runs, and returns the new history. ok is false
	// when no turn can fold.
	CompactTurn(ctx context.Context, turnID string) (history []eventlog.Message, ok bool, err error)
	Ended(turnID string, err error)
}

const retryBackoff = 200 * time.Millisecond

const (
	// notRun is the result of each tool call of a response that max_tokens cut off.
	notRun       = "not run: the response was cut off at its output limit"
	continuation = "[continuation: your previous turn was cut off because it reached the max_tokens output limit (auto-continue %d of %d). Continue exactly where you left off. Produce your output in smaller pieces so this does not happen again.]"
)

// Run runs req on b and reports its items and its end to to. Only tools,
// and the tools that src gives each model call, reach the model. When b
// does not own the loop, Run runs the tool calls of each model call in
// order, then calls b again, until a call asks for no tool. Model calls run
// under step and tools under ctx: when only step ends, a running tool
// finishes and no new tool starts.
func Run(ctx, step context.Context, b Backend, req Request, tools []Tool, src Source, to Reporter, lim Limits) {
	to.Ended(req.TurnID, run(ctx, step, b, req, tools, src, to, lim))
}

// run compacts and calls the model again after a context overflow, when b
// does not own its context. A response that max_tokens cut off runs
// none of its tool calls, and the next call asks the model to continue, at
// most lim.Continuations times.
func run(ctx, step context.Context, b Backend, req Request, tools []Tool, src Source, to Reporter, lim Limits) error {
	caps := b.Capabilities(req.Model)
	if caps.OwnsLoop {
		lim.Idle = 0
	}
	continued := 0
	var nudge []eventlog.Message
	for {
		if step.Err() != nil {
			return context.Cause(step)
		}
		s, call := &sink{turnID: req.TurnID, to: to}, req
		runnable := describe(step, &call, tools, src, caps.OwnsLoop)
		call.History = append(slices.Clip(req.History), nudge...)
		res, err := callModel(step, b, call, s, lim)
		if errors.Is(err, ErrContextOverflow) && !caps.OwnsContext && len(s.items) == 0 {
			if h, ok, cerr := to.CompactTurn(step, req.TurnID); cerr == nil && ok {
				req.History = h
				continue
			}
		}
		if err != nil {
			return err
		}
		calls := toolCalls(s.items)
		if caps.OwnsLoop || len(calls) == 0 && !res.MaxTokens {
			return nil
		}
		req.History = append(req.History, s.items...)
		for _, c := range calls {
			if step.Err() != nil {
				return context.Cause(step)
			}
			r := protocol.ToolResult{Text: notRun, IsError: true}
			if !res.MaxTokens {
				r = runTool(ctx, runnable, c)
			}
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			m := result(c, r)
			if err := to.Item(req.TurnID, "", m); err != nil {
				return err
			}
			req.History = append(req.History, m)
		}
		switch {
		case !res.MaxTokens:
			nudge = nil
		case continued < lim.Continuations:
			continued++
			nudge = []eventlog.Message{{Role: eventlog.RoleUser,
				Parts: []eventlog.Part{{Type: eventlog.PartText, Text: fmt.Sprintf(continuation, continued, lim.Continuations)}}}}
		case lim.Continuations <= 0:
			return nil
		default:
			return fmt.Errorf("turn: the response reached max_tokens after %d continuations", continued)
		}
	}
}

// callModel runs one model call. It calls b again, at most lim.Retries
// times, after an ErrRetryable error that came before any item.
func callModel(ctx context.Context, b Backend, req Request, s *sink, lim Limits) (Result, error) {
	res, err := watch(ctx, b, req, s, lim.Idle)
	for n := 0; n < lim.Retries && len(s.items) == 0 && errors.Is(err, ErrRetryable); n++ {
		s.item = ""
		if err = s.wait(ctx, n); err == nil {
			res, err = watch(ctx, b, req, s, lim.Idle)
		}
	}
	return res, err
}

// watch calls b once. With a positive idle, a call that reports nothing to
// s for idle ends with errStalled.
func watch(ctx context.Context, b Backend, req Request, s Sink, idle time.Duration) (Result, error) {
	if idle <= 0 {
		return b.Run(ctx, req, s)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	t := time.AfterFunc(idle, func() { cancel(errStalled) })
	defer t.Stop()
	res, err := b.Run(ctx, req, watched{s, func() { t.Reset(idle) }})
	if err != nil && errors.Is(context.Cause(ctx), errStalled) {
		err = errStalled
	}
	return res, err
}

// watched is a Sink that calls alive on each delta, item, and Alive.
type watched struct {
	Sink
	alive func()
}

func (w watched) Item(m eventlog.Message) error {
	w.alive()
	return w.Sink.Item(m)
}

func (w watched) Delta(itemID string, d Delta) {
	w.alive()
	w.Sink.Delta(itemID, d)
}

func (w watched) Alive() { w.alive() }

func (s *sink) wait(ctx context.Context, attempt int) error {
	d := retryBackoff << attempt
	d += rand.N(d/5) - d/10
	s.to.Status(s.turnID, protocol.StatusFrame{Status: protocol.StatusRetrying, Attempt: attempt + 1, NextAt: time.Now().Add(d)})
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		s.to.Status(s.turnID, protocol.StatusFrame{Status: protocol.StatusRunning})
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
	// item is the ID of the item that the deltas since the last Item build.
	item string
}

func (s *sink) Item(m eventlog.Message) error {
	id := s.item
	s.item = ""
	if err := s.to.Item(s.turnID, id, m); err != nil {
		return err
	}
	s.items = append(s.items, m)
	return nil
}

// Delta adds d to the next item. The backend item ID is not used: one
// harness item can join several backend items, such as reasoning and text.
func (s *sink) Delta(_ string, d Delta) {
	if s.item == "" {
		s.item = s.to.Started(s.turnID)
	}
	s.to.Delta(s.turnID, s.item, d)
}

func (*sink) Alive() {}

func (s *sink) Telemetry(t Telemetry) { s.to.Telemetry(s.turnID, t) }

func (s *sink) Steer() ([]eventlog.Message, error) {
	in, err := s.to.Steer(s.turnID)
	s.items = append(s.items, in...)
	return in, err
}

func (s *sink) State(backend string) ([]byte, error) { return s.to.State(s.turnID, backend) }

func (s *sink) SaveState(backend string, blob []byte) error {
	return s.to.SaveState(s.turnID, backend, blob)
}

func (s *sink) Compacted(summary string) error { return s.to.Compacted(s.turnID, summary) }
