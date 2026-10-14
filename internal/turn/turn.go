// Package turn runs one agent turn against a Backend.
package turn

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/majorcontext/harness/internal/backoff"
	"github.com/majorcontext/harness/protocol"
)

// MCPServer is an MCP server that one session adds. Command is the argv of a stdio server and URL the
// endpoint of an HTTP server.
type MCPServer struct {
	Command []string
	Env     []string
	Dir     string
	URL     string
	Headers map[string]string
}

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
	Run(ctx context.Context, req Request, out Sink) error
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
	// WindowEstimated marks ContextWindow as the default for an unknown model.
	WindowEstimated bool
	// Efforts are the levels the model lists; none: unknown.
	Efforts []string
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
	Settings  protocol.Settings
	// Instructions is the system prompt of the call.
	Instructions string
	// Input holds the inputs that started the turn, as user messages.
	Input []protocol.LogMessage
	// History is the conversation that the model sees, Input included.
	History []protocol.LogMessage
	// Tools describes the tools that the model may call.
	Tools []protocol.ToolSpec
	// Call runs a call to one of Tools. A backend that owns the loop calls it.
	Call func(ctx context.Context, c protocol.ToolCall) protocol.ToolResult
	// MaxTokens caps the response of a call. Zero: the backend default.
	MaxTokens int
	// AcceptCut makes a response that MaxTokens cut off the answer. Otherwise
	// the backend records no item for it and fails with a retryable error.
	AcceptCut bool
	// AllowedTools restricts the tools of the turn. nil keeps every tool;
	// an empty, non-nil list keeps none.
	AllowedTools []string
	// Steered receives a value when a steer input waits for Sink.Steer. It
	// is nil when the turn takes no steer input.
	Steered <-chan struct{}
	// MCPServers are the session MCP servers, for a backend that owns MCP. Never log their env or headers.
	MCPServers map[string]MCPServer
	// Questions reports that the backend may ask the user a question.
	Questions bool
	// Foreign reports History messages that another provider recorded.
	Foreign bool
	// Banner is the engine status that the first model call of a session
	// pins. Empty: none.
	Banner string
	// Pins is a copy of the engine context that the model reads inside the conversation.
	Pins []Pin
	// Blob reads the bytes of a blob part.
	Blob func(ctx context.Context, key string) ([]byte, error)
	// Params changes the model, the output cap, and the sampling of each
	// model call of a backend that does not own the loop. nil: none.
	Params func(ctx context.Context, p CallParams) CallParams
	// Temperature and TopP are the sampling values of a call. nil: the provider default.
	Temperature, TopP *float64
}

// CallParams are the parameters of one model call that a Params hook changes.
// An empty Model keeps the model of the call.
type CallParams struct {
	Model       string
	MaxTokens   int
	Temperature *float64
	TopP        *float64
}

// Delta is a piece of an item that is not complete yet.
type Delta struct {
	// Type is protocol.LogPartText or protocol.LogPartReasoning.
	Type string
	Text string
}

// Telemetry is what a backend measured during one model call.
type Telemetry struct {
	Usage protocol.Usage
	// Context is a context reading; an empty Source is none.
	Context protocol.ContextMeasured
	// SubscriptionUsage is the subscription limit snapshot of the call, or nil.
	SubscriptionUsage *protocol.SubscriptionUsage
	CostUSD           *float64
	// Call is the transport timing and shape of the call; nil: none measured.
	Call *CallMetrics
}

// Snapshot is the private state of a backend between turns: a small Head that
// each save replaces, and Entries that grow by appending.
type Snapshot struct {
	Head    json.RawMessage
	Entries []json.RawMessage
}

// Sink receives the items of a running turn.
type Sink interface {
	// Item records one completed message. An error stops the turn.
	Item(m protocol.LogMessage) error
	// Delta streams a piece of the item that the backend calls itemID.
	Delta(itemID string, d Delta)
	// Discard tells subscribers to remove the live item itemID.
	Discard(itemID string)
	// Alive reports model output that is not a delta yet, such as tool
	// arguments that still stream.
	Alive()
	// Telemetry adds a measurement to the turn.
	Telemetry(t Telemetry)
	// Steer takes the queued steer inputs into the turn. A backend with
	// Steering calls it at each item boundary and must use each message.
	Steer() ([]protocol.LogMessage, error)
	// State returns the saved state of backend, or a zero Snapshot.
	State(backend string) (Snapshot, error)
	// SaveState records s, and stores only the entries that it adds.
	SaveState(backend string, s Snapshot) error
	// Compacted records that the backend compacted its own context.
	Compacted(summary string) error
	// Status sends an ephemeral status frame of the running turn.
	Status(f protocol.StatusFrame)
	// Ask opens a request on the open tool call callID; Resolution reads the record that closed it.
	Ask(callID, kind string, payload json.RawMessage) error
	Resolution(id string) (protocol.RequestResolved, bool)
	// Asked returns the payload of request id as it opened.
	Asked(id string) (json.RawMessage, bool)
}

// Threaded is a Sink that gives each key a live item of its own. A backend
// that runs several threads of output at once, such as a main thread and a
// subagent, gives each thread a key, so the deltas of one never join the item
// of another.
type Threaded interface {
	Thread(key string) Thread
}

// Thread is a Sink whose Delta and Item use the live item of one key. The
// live item of the key starts with its first Delta and completes with its next
// Item.
type Thread interface {
	Sink
	// Drop forgets the live item without completing it: the next Delta starts
	// a new item, and the dropped item never completes, as the item of a
	// failed attempt.
	Drop()
}

// Limits bounds how a turn recovers from a failed model call.
type Limits struct {
	// Retries bounds the new attempts of a model call after an ErrRetryable error.
	Retries int
	// Idle stops a model call that reports nothing to its Sink for this
	// long. Zero or less: no limit.
	Idle time.Duration
	// Log receives the turn_metrics line of each model call. nil: slog.Default().
	Log *slog.Logger
}

// Turn is what a running turn reports through, bound to its turn. Started
// announces an item and returns the ID that its Delta calls name and its Item
// records; it replaces an item that a failed call streamed. CompactTurn
// returns ok false when no turn can fold. Only CompactTurn takes a ctx.
type Turn interface {
	Sink
	Started() string
	// Announce is Started for a keyed thread: it announces an item and returns
	// its ID, and Item does not use it.
	Announce() string
	// ItemAs records m under the ID that Announce returned.
	ItemAs(id string, m protocol.LogMessage) error
	// Attach stores a blob of a tool result and returns the blob part that names it.
	Attach(mediaType string, data []byte) (protocol.LogPart, error)
	// Settings returns the model and settings of the session now, or "".
	Settings() (string, protocol.Settings)
	// Pin pins the changed notices after at messages and returns every pin.
	Pin(ns []Notice, at int) []Pin
	CompactTurn(ctx context.Context) (history []protocol.LogMessage, ok bool, err error)
	Ended(err error)
}

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
// does not own its context.
func run(ctx, step context.Context, b Backend, req Request, src Source, to Turn, lim Limits) error {
	ctx, step = context.WithValue(ctx, turnIDKey{}, req.TurnID), context.WithValue(step, turnIDKey{}, req.TurnID)
	caps := b.Capabilities(req.Model)
	if caps.OwnsLoop {
		lim.Idle, lim.Retries = 0, 0
	}
	record := func(c protocol.ToolCall, r protocol.ToolResult) error {
		m, err := result(to, c, r)
		if err != nil {
			return err
		}
		if err := to.Item(m); err != nil {
			return err
		}
		req.History = append(req.History, m)
		return nil
	}
	for first := true; ; first = false {
		if step.Err() != nil {
			return context.Cause(step)
		}
		if err := adopt(b, caps, to, &req); err != nil {
			return err
		}
		if first && !caps.OwnsLoop {
			if err := answerAsks(ctx, src, to, &req, record); err != nil {
				return err
			}
		}
		s, call := &sink{Turn: to, log: cmp.Or(lim.Log, slog.Default())}, req
		if err := applyParams(step, b, caps, req, &call); err != nil {
			return err
		}
		t := describe(step, &call, src, to, caps.OwnsLoop)
		err := callModel(step, b, call, s, lim, caps.OwnsLoop)
		if errors.Is(err, ErrContextOverflow) && !caps.OwnsContext && len(s.items) == 0 {
			if h, ok, cerr := to.CompactTurn(step); cerr == nil && ok {
				req.History = h
				continue
			}
		}
		if err != nil {
			return err
		}
		calls := toolCalls(s.items)
		if caps.OwnsLoop || len(calls) == 0 {
			return nil
		}
		req.History = append(req.History, s.items...)
		asks, calls := splitAsks(t, calls)
		if err := runBatch(ctx, step, t, calls, func(i int, r protocol.ToolResult) error { return record(calls[i], r) }); err != nil {
			return err
		}
		if opened, err := openAsk(ctx, t, to, asks, record); opened || err != nil {
			return err
		}
		in, err := to.Steer()
		if err != nil {
			return err
		}
		req.History = append(req.History, in...)
	}
}

// callModel runs one model call. It calls b again, at most lim.Retries
// times, after an ErrRetryable error that came before any item. The wait is
// the advice of the provider, else backoff.Delay.
func callModel(ctx context.Context, b Backend, req Request, s *sink, lim Limits, ownsLoop bool) error {
	s.call = callOf(req, 0, ownsLoop)
	err := watch(ctx, b, req, s, lim.Idle)
	for n := 0; n < lim.Retries && len(s.items) == 0 && retryable(err); n++ {
		s.item, s.threads = "", nil
		if err = s.wait(ctx, n, err); err == nil {
			s.call = callOf(req, n+1, ownsLoop)
			err = watch(ctx, b, req, s, lim.Idle)
		}
	}
	return err
}

// watch calls b once. With a positive idle, a call that reports nothing to
// s for idle ends with errStalled.
func watch(ctx context.Context, b Backend, req Request, s Sink, idle time.Duration) error {
	if idle <= 0 {
		return b.Run(ctx, req, s)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	t := time.AfterFunc(idle, func() { cancel(errStalled) })
	defer t.Stop()
	err := b.Run(ctx, req, watched{s, func() { t.Reset(idle) }})
	if err != nil && errors.Is(context.Cause(ctx), errStalled) {
		err = errStalled
	}
	return err
}

// watched is a Sink that calls alive on each delta, item, and Alive.
type watched struct {
	Sink
	alive func()
}

func (w watched) Item(m protocol.LogMessage) error {
	w.alive()
	return w.Sink.Item(m)
}

func (w watched) Delta(itemID string, d Delta) {
	w.alive()
	w.Sink.Delta(itemID, d)
}

func (w watched) Alive() { w.alive() }

// Thread gives a sink that has no threads one live item for every key.
func (w watched) Thread(key string) Thread {
	if t, ok := w.Sink.(Threaded); ok {
		return watchedThread{t.Thread(key), w.alive}
	}
	return flatThread{w}
}

// flatThread is the thread of a sink that keeps one live item.
type flatThread struct{ Sink }

func (flatThread) Drop() {}

type liveThread Thread

type watchedThread struct {
	liveThread
	alive func()
}

func (w watchedThread) Item(m protocol.LogMessage) error {
	w.alive()
	return w.liveThread.Item(m)
}

func (w watchedThread) Delta(itemID string, d Delta) {
	w.alive()
	w.liveThread.Delta(itemID, d)
}

func (s *sink) wait(ctx context.Context, attempt int, cause error) error {
	d := retryDelay(attempt+1, cause)
	s.Status(protocol.StatusFrame{Status: protocol.StatusRetrying, Attempt: attempt + 1, NextAt: time.Now().Add(d)})
	if err := sleep(ctx, d); err != nil {
		return err
	}
	s.Status(protocol.StatusFrame{Status: protocol.StatusRunning})
	return nil
}

// advice is what a provider error says about its retry.
type advice interface {
	RetryAfter() (time.Duration, bool)
	AdviceOnly() bool
}

// retryable reports whether a model call runs again after err. An error that
// the provider retries only on advice is not retryable without it.
func retryable(err error) bool {
	if !errors.Is(err, ErrRetryable) {
		return false
	}
	var a advice
	if errors.As(err, &a) {
		_, advised := a.RetryAfter()
		return advised || !a.AdviceOnly()
	}
	return true
}

// retryDelay is the wait before retry number n: the advice of the provider, else backoff.Delay.
func retryDelay(n int, cause error) time.Duration {
	var a advice
	if errors.As(cause, &a) {
		if after, ok := a.RetryAfter(); ok {
			return after
		}
	}
	return backoff.Delay(n)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// sink collects the items of one model call and reports the rest to its Turn.
type sink struct {
	Turn
	items []protocol.LogMessage
	// item is the item that the deltas since the last Item build, or "".
	item string
	// threads holds the live item of each keyed thread.
	threads map[string]string
	// call describes the model call in flight, for its turn_metrics line.
	call callInfo
	log  *slog.Logger
}

// Delta streams d into the next item. The backend item ID is not used: one
// harness item can join several backend items, such as reasoning and text.
func (s *sink) Delta(_ string, d Delta) {
	if s.item == "" {
		s.item = s.Started()
	}
	s.Turn.Delta(s.item, d)
}

// thread is the view of a sink with the live item of one key.
type thread struct {
	*sink
	key string
}

func (s *sink) Thread(key string) Thread { return thread{s, key} }

func (t thread) Delta(_ string, d Delta) {
	id, ok := t.threads[t.key]
	if !ok {
		if t.threads == nil {
			t.threads = map[string]string{}
		}
		id = t.Announce()
		t.threads[t.key] = id
	}
	t.Turn.Delta(id, d)
}

func (t thread) Item(m protocol.LogMessage) error {
	id := t.threads[t.key]
	delete(t.threads, t.key)
	if err := t.ItemAs(id, m); err != nil {
		return err
	}
	t.items = append(t.items, m)
	return nil
}

func (t thread) Drop() {
	if id, ok := t.threads[t.key]; ok {
		delete(t.threads, t.key)
		t.Discard(id)
	}
}

func (s *sink) Item(m protocol.LogMessage) error {
	s.item = ""
	if err := s.Turn.Item(m); err != nil {
		return err
	}
	s.items = append(s.items, m)
	return nil
}

func (s *sink) Steer() ([]protocol.LogMessage, error) {
	in, err := s.Turn.Steer()
	s.items = append(s.items, in...)
	return in, err
}

// applyParams runs the Params hook of req and sets its result on call.
func applyParams(ctx context.Context, b Backend, caps Capabilities, req Request, call *Request) error {
	if req.Params == nil || caps.OwnsLoop {
		return nil
	}
	p := req.Params(ctx, CallParams{Model: req.Model, MaxTokens: req.MaxTokens})
	if p.Model != "" && p.Model != req.Model {
		if b.Capabilities(p.Model).OwnsLoop {
			return fmt.Errorf("turn: a chat.params hook set the model to %s, which another kind of backend runs, so this turn cannot call it", p.Model)
		}
		call.Model = p.Model
	}
	call.MaxTokens, call.Temperature, call.TopP = p.MaxTokens, p.Temperature, p.TopP
	return nil
}

// adopt sets the model and settings that the session has now on req. It fails
// when the model needs another kind of backend than caps.
func adopt(b Backend, caps Capabilities, to Turn, req *Request) error {
	if m, set := to.Settings(); m != "" {
		if b.Capabilities(m).OwnsLoop != caps.OwnsLoop {
			return fmt.Errorf("turn: the model changed to %s, which another kind of backend runs, so this turn cannot call it", m)
		}
		req.Model, req.Settings = m, set
	}
	return nil
}

type turnIDKey struct{}

// ID returns the ID of the turn whose model call or tool call runs under ctx, or "".
func ID(ctx context.Context) string {
	id, _ := ctx.Value(turnIDKey{}).(string)
	return id
}
