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
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

var (
	// ErrConflict reports an append whose expected seq is not the log head.
	ErrConflict = errors.New("harness: append conflict")
	// ErrNotFound reports a session with an empty log.
	ErrNotFound = errors.New("harness: session not found")
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
	Log       Log
	Blobs     Blobs
	Ownership Ownership
	// Owner names this process in owner.acquired.
	Owner   string
	Backend turn.Backend
	Tools   []turn.Tool
	// Sync receives every durable record. nil: no replication.
	Sync Sync
	// Retries bounds the new attempts of a turn after a retryable error.
	Retries int
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
	Stopped bool
	changed chan struct{}
}

// Actor is the one goroutine that runs a session.
type Actor struct {
	cfg  Config
	mail chan func()
	quit chan struct{}
	done chan struct{}
	view atomic.Pointer[View]
	live live

	stale   chan struct{}
	flushed chan struct{}
	synced  atomic.Uint64
	syncErr error

	state *eventlog.State
	// fenced is the seq of the owner.acquired record of this actor.
	fenced    uint64
	run       *running
	releasing []func(struct{}, error)
	stopped   bool
}

func newActor(cfg Config, s *eventlog.State) *Actor {
	a := &Actor{cfg: cfg, mail: make(chan func()), quit: make(chan struct{}), done: make(chan struct{}),
		stale: make(chan struct{}), flushed: make(chan struct{}), state: s}
	a.view.Store(&View{changed: make(chan struct{})})
	a.publish(false)
	return a
}

// Create appends session.created and owner.acquired to an empty log and runs the session.
func Create(ctx context.Context, cfg Config, c eventlog.SessionCreated) (*Actor, error) {
	a := newActor(cfg, &eventlog.State{})
	fence := eventlog.OwnerAcquired{Epoch: cfg.Ownership.Epoch(), Owner: cfg.Owner}
	if err := a.appendCtx(ctx, c, fence); err != nil {
		cfg.Ownership.Release()
		if errors.Is(err, ErrConflict) {
			return nil, fmt.Errorf("%w: %s", ErrExists, cfg.ID)
		}
		return nil, err
	}
	a.fenced = a.state.Head()
	a.launch()
	return a, nil
}

// Open fences every earlier owner, replays the log through the fence, ends a
// crashed turn or resumes a suspended one, and runs the session.
func Open(ctx context.Context, cfg Config) (*Actor, error) {
	a, err := open(ctx, cfg)
	if err != nil {
		cfg.Ownership.Release()
		return nil, err
	}
	a.launch()
	return a, nil
}

func (a *Actor) launch() {
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
	if err := replay(ctx, cfg.Log, s, head); err != nil {
		return nil, err
	}
	a := newActor(cfg, s)
	a.fenced = head
	t, ok := s.Turn()
	switch {
	case ok && t.Suspended:
		err = a.appendCtx(ctx, eventlog.TurnResumed{TurnID: t.ID, Count: t.Resumes + 1})
		if err == nil {
			a.start(t.ID, t.InputIDs, t.Resumes+1)
		}
	case ok:
		err = a.endTurn(ctx, t.ID, eventlog.StopInterrupted, string(eventlog.CauseCrashed), cutOff, eventlog.Usage{})
	}
	return a, err
}

// fence appends owner.acquired at the head. An append of an earlier owner
// that lands first is ordered before the fence.
func fence(ctx context.Context, cfg Config) (uint64, error) {
	for {
		head, err := cfg.Log.Head(ctx)
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
		err = cfg.Log.Append(ctx, head, data)
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
			return fmt.Errorf("harness: log ends at %d before seq %d", s.Head(), through)
		}
		for _, r := range recs {
			if err := s.Apply(r); err != nil {
				return err
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
		case <-a.stale:
			a.stopped = true
		}
	}
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

// revoked reports whether Lost closed, Base ended, or Sync reported a stale epoch.
func (a *Actor) revoked() bool {
	select {
	case <-a.cfg.Ownership.Lost():
	case <-a.cfg.Base.Done():
	case <-a.stale:
	default:
		return false
	}
	return true
}

func (a *Actor) finish() {
	if a.run != nil {
		a.run.cancel(ErrNotOwned)
	}
	a.publish(true)
	close(a.quit)
	<-a.flushed
	a.cfg.Ownership.Release()
	close(a.done)
	a.cfg.Done()
}

// Done closes after the actor stops, Sync acknowledges its last record or
// the ownership ends, and the actor releases its Ownership.
func (a *Actor) Done() <-chan struct{} { return a.done }

// View returns the newest published view.
func (a *Actor) View() *View { return a.view.Load() }

func (a *Actor) publish(stopped bool) {
	next := &View{Session: Describe(a.cfg.ID, a.state), Stopped: stopped, changed: make(chan struct{})}
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
	if err := a.cfg.Log.Append(ctx, head, recs...); err != nil {
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
	a.publish(false)
	return nil
}

func newID(prefix string) string { return prefix + "_" + strings.ToLower(rand.Text()) }
