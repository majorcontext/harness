// Package turn runs one agent turn against a Backend.
package turn

import (
	"context"
	"encoding/json"
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
	OwnsLoop    bool
	OwnsContext bool
	// OwnsMCP reports a backend that connects the configured MCP servers
	// itself on a turn with no tool restriction.
	OwnsMCP bool
	// Steering is a backend that owns the loop and calls Sink.Steer.
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
	// MaxTokens caps the response of a call. Zero: the backend default.
	MaxTokens int
	// AllowedTools restricts the tools of the turn. nil keeps every tool;
	// an empty, non-nil list keeps none.
	AllowedTools []string
	// Steered receives a value when a steer input waits for Sink.Steer. It
	// is nil when the turn takes no steer input.
	Steered <-chan struct{}
	// Questions reports that the backend may ask the user a question.
	Questions bool
	// Foreign reports History messages that another provider recorded.
	Foreign bool
	// Banner is engine context that each model call sends after History[:BannerAt].
	Banner   string
	BannerAt int
	// Blob reads the bytes of a blob part.
	Blob func(ctx context.Context, key string) ([]byte, error)
}

// Delta is a piece of an item that is not complete yet.
type Delta struct {
	// Type is eventlog.PartText or eventlog.PartReasoning.
	Type string
	Text string
}

// Telemetry is what a backend measured during one model call.
type Telemetry struct {
	Usage eventlog.Usage
	// Context is a context reading; an empty Source is none.
	Context eventlog.ContextMeasured
	// SubscriptionUsage is the subscription limit snapshot of the call, or nil.
	SubscriptionUsage *eventlog.SubscriptionUsage
	CostUSD           *float64
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
	// Ask opens a request on the open tool call callID; Resolution reads the record that closed it.
	Ask(callID, kind string, payload json.RawMessage) error
	Resolution(id string) (eventlog.RequestResolved, bool)
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

// Turn is what a running turn reports through, bound to its turn. Started
// announces an item and returns the ID that its Delta calls name and its Item
// records; it replaces an item that a failed call streamed. CompactTurn
// returns ok false when no turn can fold. Only CompactTurn takes a ctx.
type Turn interface {
	Sink
	Started() string
	Status(f protocol.StatusFrame)
	// Settings returns the model and settings of the session now, or "".
	Settings() (string, eventlog.Settings)
	CompactTurn(ctx context.Context) (history []eventlog.Message, ok bool, err error)
	Ended(err error)
}

// A wait before a new attempt starts at retryBackoff and doubles up to retryBackoffMax.
const (
	retryBackoff    = time.Second
	retryBackoffMax = 8 * time.Second
)

const (
	// notRun is the result of each tool call of a response that max_tokens cut off.
	notRun       = "not run: the response was cut off at its output limit"
	continuation = "[continuation: your previous turn was cut off because it reached the max_tokens output limit (auto-continue %d of %d). Continue exactly where you left off. Produce your output in smaller pieces so this does not happen again.]"
)

// Run runs req on b and reports its items and its end to to. Only the tools
// that src gives each model call reach the model. When b
// does not own the loop, Run runs the tool calls of each model call in
// order, takes the steer inputs, and calls b again until a call asks for no
// tool. Model calls run under step and tools under ctx: when only step ends,
// a running tool finishes and no new tool starts.
func Run(ctx, step context.Context, b Backend, req Request, src Source, to Turn, lim Limits) {
	to.Ended(run(ctx, step, b, req, src, to, lim))
}

// run compacts and calls the model again after a context overflow, when b
// does not own its context. A response that max_tokens cut off runs
// none of its tool calls, and the next call asks the model to continue, at
// most lim.Continuations times.
func run(ctx, step context.Context, b Backend, req Request, src Source, to Turn, lim Limits) error {
	caps := b.Capabilities(req.Model)
	if caps.OwnsLoop {
		lim.Idle, lim.Retries = 0, 0
	}
	continued := 0
	var nudge []eventlog.Message
	for {
		if step.Err() != nil {
			return context.Cause(step)
		}
		if m, set := to.Settings(); m != "" {
			if b.Capabilities(m).OwnsLoop != caps.OwnsLoop {
				return fmt.Errorf("turn: the model changed to %s, which another kind of backend runs, so this turn cannot call it", m)
			}
			req.Model, req.Settings = m, set
		}
		s, call := &sink{Turn: to}, req
		runTool := describe(step, &call, src, caps.OwnsLoop)
		call.History = append(slices.Clip(req.History), nudge...)
		res, err := callModel(step, b, call, s, lim)
		if errors.Is(err, ErrContextOverflow) && !caps.OwnsContext && len(s.items) == 0 {
			if h, ok, cerr := to.CompactTurn(step); cerr == nil && ok {
				req.History, req.BannerAt = h, min(req.BannerAt, len(h))
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
				r = runTool(ctx, c)
			}
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			m := result(c, r)
			if err := to.Item(m); err != nil {
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
				Parts: []eventlog.Part{{Type: eventlog.PartEngineContext, Text: fmt.Sprintf(continuation, continued, lim.Continuations)}}}}
		case lim.Continuations <= 0:
			return nil
		default:
			return fmt.Errorf("turn: the response reached max_tokens after %d continuations", continued)
		}
		in, err := to.Steer()
		if err != nil {
			return err
		}
		req.History = append(req.History, in...)
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
	d := min(retryBackoff<<min(attempt, 3), retryBackoffMax)
	d += rand.N(d/5) - d/10
	s.Status(protocol.StatusFrame{Status: protocol.StatusRetrying, Attempt: attempt + 1, NextAt: time.Now().Add(d)})
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		s.Status(protocol.StatusFrame{Status: protocol.StatusRunning})
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// sink collects the items of one model call and reports the rest to its Turn.
type sink struct {
	Turn
	items []eventlog.Message
	// item is the item that the deltas since the last Item build, or "".
	item string
}

// Delta streams d into the next item. The backend item ID is not used: one
// harness item can join several backend items, such as reasoning and text.
func (s *sink) Delta(_ string, d Delta) {
	if s.item == "" {
		s.item = s.Started()
	}
	s.Turn.Delta(s.item, d)
}

func (s *sink) Item(m eventlog.Message) error {
	s.item = ""
	if err := s.Turn.Item(m); err != nil {
		return err
	}
	s.items = append(s.items, m)
	return nil
}

func (s *sink) Steer() ([]eventlog.Message, error) {
	in, err := s.Turn.Steer()
	s.items = append(s.items, in...)
	return in, err
}
