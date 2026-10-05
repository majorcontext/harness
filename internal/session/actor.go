// Package session runs each live session as one actor goroutine. The actor
// owns the session State, its Ownership, and every append to its log.
package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

var (
	// ErrConflict reports an append whose expected seq is not the log head,
	// or a SyncBatch whose records differ from the records of the receiver.
	ErrConflict = errors.New("harness: append conflict")
	// ErrNotFound reports a session with an empty log.
	ErrNotFound = errors.New("harness: session not found")
	// ErrUnreplayable reports a log that does not replay: a record that does not decode or break a rule of the log.
	ErrUnreplayable = errors.New("harness: log does not replay")
	// ErrExists reports a create of a session that has a log.
	ErrExists = errors.New("harness: session exists")
	// ErrNotOwned reports a session whose actor stopped: it was fenced, lost, or released.
	ErrNotOwned = errors.New("harness: session not owned")
	// ErrInputConflict reports an input ID admitted with another body.
	ErrInputConflict = errors.New("harness: input conflict")
	// ErrTurnMismatch reports a turn ID that is not the running turn.
	ErrTurnMismatch = errors.New("harness: turn mismatch")
)

// Log is the record log of one session. Append fails with an error that
// matches ErrConflict when expectedSeq is not the head.
type Log interface {
	Head(ctx context.Context) (uint64, error)
	Append(ctx context.Context, expectedSeq uint64, records ...[]byte) error
	Read(ctx context.Context, afterSeq uint64, limit int) ([]eventlog.Record, error)
}

// Blobs holds the blobs of one session.
type Blobs interface {
	PutBlob(ctx context.Context, key string, r io.Reader) error
	GetBlob(ctx context.Context, key string) (io.ReadCloser, error)
}

// Storage is the log and the blobs of one session.
type Storage interface {
	Log
	Blobs
}

// Ownership is the grant to run one session.
type Ownership interface {
	Epoch() uint64
	Lost() <-chan struct{}
	Release()
}

// Config is what an actor needs. The actor releases Ownership when it stops,
// and also when Create or Open fails.
type Config struct {
	ID        string
	Store     Storage
	Ownership Ownership
	// Owner names this process in owner.acquired.
	Owner   string
	Backend turn.Backend
	// Check reports why a session at model from that allows tools cannot
	// move to model to. nil accepts every model.
	Check func(from, to string, tools []string) error
	// AskUserQuestion lets a backend ask the user a question; the embedder
	// answers with Resolve.
	AskUserQuestion bool
	// Banner is the engine status that each model call of a turn sends as
	// engine context. Empty: none.
	Banner string
	// Evaluator is the model that judges goal turns.
	Evaluator string
	// Source gives the tools of each model call, and the hooks around each
	// tool call. nil: no tool.
	Source turn.Source
	// Retain gives a harness-loop turn read_tool_result and keeps each large
	// result out of the history, after the hooks of Source.
	Retain bool
	// Prompt returns the system prompt of a turn, when the turn starts.
	Prompt func() string
	// Appended receives the events of each append, and the state after it,
	// on the actor goroutine. It must not block, wait for the actor, or keep
	// the state. nil: none.
	Appended func([]eventlog.Event, *eventlog.State)
	// Sync receives every durable record. nil: no replication.
	Sync Sync
	// Limits bounds how each turn recovers from a failed model call.
	Limits turn.Limits
	// Threshold is the share of the context window at which the next turn compacts first.
	Threshold float64
	// KeepTurns is the number of newest turns that a compaction keeps.
	KeepTurns int
	// Base bounds the actor. When it ends, the actor stops without an append.
	Base context.Context
	// Go runs a goroutine that the runtime waits for at shutdown.
	Go func(func())
	// Done runs once after the actor stops.
	Done func()
}

// View is an immutable snapshot that the actor publishes after each append.
type View struct {
	Session protocol.Session
	// Agent is the profile of a child session, or "".
	Agent string
	// Unsettled are the spawned children that have not settled, sorted.
	Unsettled []string
	Stopped   bool
	changed   chan struct{}
}

// Actor is the one goroutine that runs a session.
type Actor struct {
	cfg  Config
	mail chan func()
	quit chan struct{}
	done chan struct{}
	view atomic.Pointer[View]
	live live

	// rejected closes when Sync rejects a batch for good.
	rejected chan struct{}
	flushed  chan struct{}
	synced   atomic.Uint64
	syncErr  error

	// launched is set by Run. Until then, spawn holds each run in pending.
	launched bool
	pending  []func()
	// runs counts the turn, compaction, and judge goroutines.
	runs sync.WaitGroup
	// warming closes when the warm-up that Run started ends. nil: none.
	warming chan struct{}

	state *eventlog.State
	// fenced is the seq of the owner.acquired record of this actor.
	fenced    uint64
	run       *running
	releasing []func(struct{}, error)
	stopped   bool
	retryStop context.CancelFunc
	retryAt   time.Time
	// bannered is set once bannerPin holds the place of the banner.
	bannered  bool
	bannerPin int
}

func newActor(cfg Config, s *eventlog.State) *Actor {
	a := &Actor{cfg: cfg, mail: make(chan func()), quit: make(chan struct{}), done: make(chan struct{}),
		rejected: make(chan struct{}), flushed: make(chan struct{}), state: s}
	a.view.Store(&View{changed: make(chan struct{})})
	a.publish(false)
	return a
}

// Create appends session.created and owner.acquired to an empty log. A
// first input starts the first turn in the same append. Run runs the session.
func Create(ctx context.Context, cfg Config, c eventlog.SessionCreated, first *eventlog.InputAdmitted) (*Actor, error) {
	a := newActor(cfg, &eventlog.State{})
	events := []eventlog.Event{c, eventlog.OwnerAcquired{Epoch: cfg.Ownership.Epoch(), Owner: cfg.Owner}}
	a.fenced = uint64(len(events))
	turnID := newID("turn")
	if first != nil {
		events = append(events, *first, eventlog.TurnStarted{TurnID: turnID, InputIDs: []string{first.InputID}})
	}
	if err := a.appendCtx(ctx, events...); err != nil {
		cfg.Ownership.Release()
		if errors.Is(err, ErrConflict) {
			return nil, fmt.Errorf("%w: %s", ErrExists, cfg.ID)
		}
		return nil, err
	}
	if first != nil {
		a.start(turnID, []string{first.InputID})
	}
	return a, nil
}

// Open fences every earlier owner, replays the log through the fence,
// resumes a suspended turn or ends a crashed one, and starts the next
// queued input when no turn resumes. Run runs the session.
func Open(ctx context.Context, cfg Config) (*Actor, error) {
	a, err := open(ctx, cfg)
	if err != nil {
		cfg.Ownership.Release()
		return nil, err
	}
	return a, nil
}

// Run starts the actor goroutine, the Sync sender, and the run that Create
// or Open started. Call it once, after the actor is published: a tool of
// that run can look up its own session.
func (a *Actor) Run() {
	a.warm()
	a.launched = true
	for _, f := range a.pending {
		a.cfg.Go(f)
	}
	a.pending = nil
	a.cfg.Go(a.loop)
	if a.cfg.Sync == nil {
		close(a.flushed)
		return
	}
	a.cfg.Go(func() {
		a.syncErr = a.replicate()
		close(a.flushed)
	})
}

func open(ctx context.Context, cfg Config) (*Actor, error) {
	head, err := fence(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &eventlog.State{}
	if err := replay(ctx, cfg.Store, s, head); err != nil {
		return nil, err
	}
	a := newActor(cfg, s)
	a.fenced = head
	if err := a.endCommands(ctx); err != nil {
		return nil, err
	}
	t, ok := s.Turn()
	switch {
	case ok && t.Suspended:
		err = a.appendCtx(ctx, eventlog.TurnResumed{TurnID: t.ID, Count: t.Resumes + 1})
		if err == nil {
			a.start(t.ID, t.InputIDs)
		}
	case ok:
		err = a.endTurn(ctx, eventlog.TurnEnded{TurnID: t.ID, StopReason: eventlog.StopInterrupted, Cause: eventlog.CauseCrashed}, cutOff)
		if err == nil {
			err = a.settle(true)
		}
	case !a.waitsForInput():
		err = a.settle(true)
	}
	return a, err
}

// waitsForInput reports whether the last turn failed at a usage limit of
// the provider. Its queued inputs then wait for the next input, live and
// after a restart.
func (a *Actor) waitsForInput() bool {
	last := a.state.LastEnded()
	return last.Cause == eventlog.CauseProviderExhausted
}

// fence appends owner.acquired at the head. An append of an earlier owner
// that lands first is ordered before the fence.
func fence(ctx context.Context, cfg Config) (uint64, error) {
	for {
		head, err := cfg.Store.Head(ctx)
		if err != nil {
			return 0, err
		}
		if head == 0 {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, cfg.ID)
		}
		ev := eventlog.OwnerAcquired{Epoch: cfg.Ownership.Epoch(), Owner: cfg.Owner}
		data, err := eventlog.Envelope{Seq: head + 1, Time: time.Now(), Event: ev}.Encode()
		if err != nil {
			return 0, err
		}
		err = cfg.Store.Append(ctx, head, data)
		if !errors.Is(err, ErrConflict) {
			return head + 1, err
		}
	}
}

const page = 512

func replay(ctx context.Context, log Log, s *eventlog.State, through uint64) error {
	for s.Head() < through {
		recs, err := log.Read(ctx, s.Head(), int(min(through-s.Head(), page)))
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return fmt.Errorf("%w: log ends at %d before seq %d", ErrUnreplayable, s.Head(), through)
		}
		for _, r := range recs {
			if err := s.Apply(r); err != nil {
				return fmt.Errorf("%w: %w", ErrUnreplayable, err)
			}
		}
	}
	return nil
}

// Load replays a whole log without owning it.
func Load(ctx context.Context, id string, log Log) (*eventlog.State, error) {
	head, err := log.Head(ctx)
	if err != nil {
		return nil, err
	}
	if head == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	s := &eventlog.State{}
	return s, replay(ctx, log, s, head)
}

func (a *Actor) loop() {
	defer a.finish()
	a.retryLater()
	for !a.stopped {
		select {
		case f := <-a.mail:
			if !a.lost() {
				f()
			}
		case <-a.cfg.Ownership.Lost():
			a.stopped = true
		case <-a.cfg.Base.Done():
			a.stopped = true
		case <-a.rejected:
			a.stopped = true
		}
	}
}

// spawn runs f as a run of the actor on a goroutine that the runtime waits
// for. Before Run, it holds f.
func (a *Actor) spawn(f func()) {
	a.runs.Add(1)
	g := func() {
		defer a.runs.Done()
		f()
	}
	if !a.launched {
		a.pending = append(a.pending, g)
		return
	}
	a.cfg.Go(g)
}

// lost reports whether the ownership ended. A select picks among ready
// cases at random, so the actor checks this before each command and append.
func (a *Actor) lost() bool {
	if !a.revoked() {
		return false
	}
	a.stopped = true
	return true
}

// revoked reports whether Lost closed, Base ended, or Sync rejected a batch.
func (a *Actor) revoked() bool {
	select {
	case <-a.cfg.Ownership.Lost():
	case <-a.cfg.Base.Done():
	case <-a.rejected:
	default:
		return false
	}
	return true
}

// finish waits for every run to exit before it releases the ownership, so
// the next owner never runs beside a run of this actor. quit is closed, so
// each call of a run returns ErrNotOwned.
func (a *Actor) finish() {
	if a.run != nil {
		a.run.cancel(ErrNotOwned)
	}
	a.publish(true)
	close(a.quit)
	a.runs.Wait()
	<-a.flushed
	a.cfg.Ownership.Release()
	a.cfg.Done()
	close(a.done)
}

// Done closes after the actor stops, each of its runs exits, Sync
// acknowledges its last record or the ownership ends, and the actor
// releases its Ownership.
func (a *Actor) Done() <-chan struct{} { return a.done }

// View returns the newest published view.
func (a *Actor) View() *View { return a.view.Load() }

// Read runs f on the actor goroutine with the state of the session, which f
// must not keep. It fails with ErrNotOwned once the actor stops. When Read
// returns, f is not running and never runs, so a caller may read what f wrote
// whatever the error.
func (a *Actor) Read(ctx context.Context, f func(*eventlog.State)) error {
	var claimed atomic.Bool
	ended := make(chan struct{})
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		if claimed.CompareAndSwap(false, true) {
			f(a.state)
		}
		close(ended)
		reply(struct{}{}, nil)
	})
	if err != nil && !claimed.CompareAndSwap(false, true) {
		<-ended
	}
	return err
}

func (a *Actor) publish(stopped bool) {
	window := a.cfg.Backend.Capabilities(a.state.Model()).ContextWindow
	next := &View{Session: Describe(a.cfg.ID, a.state, window), Agent: a.state.Agent(), Unsettled: a.state.Unsettled(),
		Stopped: stopped, changed: make(chan struct{})}
	a.live.mu.Lock()
	defer a.live.mu.Unlock()
	close(a.view.Swap(next).changed)
}

type reply[T any] struct {
	v   T
	err error
}

// call runs f on the actor goroutine. f calls its reply func exactly once,
// at once or later from another command.
func call[T any](ctx context.Context, a *Actor, f func(func(T, error))) (T, error) {
	ch := make(chan reply[T], 1)
	cmd := func() { f(func(v T, err error) { ch <- reply[T]{v, err} }) }
	var zero T
	select {
	case a.mail <- cmd:
	case <-a.quit:
		return zero, ErrNotOwned
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	select {
	case r := <-ch:
		return r.v, r.err
	case <-a.quit:
		select {
		case r := <-ch:
			return r.v, r.err
		default:
			return zero, ErrNotOwned
		}
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func (a *Actor) append(events ...eventlog.Event) error {
	return a.appendCtx(a.cfg.Base, events...)
}

// appendCtx checks, appends, and applies events. Any store error stops the
// actor: the next Open fences and replays from the store. Only a conflict
// means that another owner holds the session.
func (a *Actor) appendCtx(ctx context.Context, events ...eventlog.Event) error {
	if err := eventlog.Check(a.state, events); err != nil {
		return err
	}
	head, now := a.state.Head(), time.Now()
	recs := make([][]byte, len(events))
	for i, e := range events {
		data, err := eventlog.Envelope{Seq: head + uint64(i) + 1, Time: now, Event: e}.Encode()
		if err != nil {
			return err
		}
		recs[i] = data
	}
	if a.lost() {
		return ErrNotOwned
	}
	if err := a.cfg.Store.Append(ctx, head, recs...); err != nil {
		a.stopped = true
		if errors.Is(err, ErrConflict) {
			return fmt.Errorf("%w: %w", ErrNotOwned, err)
		}
		return err
	}
	for i, data := range recs {
		if err := a.state.Apply(eventlog.Record{Seq: head + uint64(i) + 1, Data: data}); err != nil {
			a.stopped = true
			return err
		}
	}
	if g, _ := a.state.Goal(); g.State != eventlog.GoalPaused || !g.RetryAt.Equal(a.retryAt) {
		a.stopRetry()
	}
	if a.cfg.Appended != nil {
		a.cfg.Appended(events, a.state)
	}
	a.publish(false)
	return nil
}

func newID(prefix string) string { return prefix + "_" + strings.ToLower(rand.Text()) }
