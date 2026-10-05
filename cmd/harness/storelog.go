package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
)

const (
	// slowPhaseThreshold names the store operations slower than this in the log.
	slowPhaseThreshold = time.Second
	// inFlightThreshold is how long a store operation may run before the
	// watchdog warns about it, and inFlightTick how often it looks again. A
	// wedged volume hangs a file operation with no error, so a log line at
	// completion never comes.
	inFlightThreshold = 5 * time.Second
	inFlightTick      = 5 * time.Second
)

// observedStore logs the slow and the stuck operations of a Store, and the
// records that name a new session or a spawned task.
type observedStore struct {
	harness.Store
	log *slog.Logger

	mu      sync.Mutex
	next    int
	flying  map[int]flight
	spawned int
}

type flight struct {
	op      string
	session string
	since   time.Time
}

func newObservedStore(st harness.Store, logger *slog.Logger) *observedStore {
	return &observedStore{Store: st, log: logger, flying: map[int]flight{}}
}

// track records that op started, and returns the call that records its end.
func (s *observedStore) track(op, session string) func() time.Duration {
	start := time.Now()
	s.mu.Lock()
	s.next++
	id := s.next
	s.flying[id] = flight{op: op, session: session, since: start}
	s.mu.Unlock()
	return func() time.Duration {
		s.mu.Lock()
		delete(s.flying, id)
		s.mu.Unlock()
		elapsed := time.Since(start)
		if elapsed > slowPhaseThreshold {
			s.log.Warn("slow store phase", "op", op, "session", session, "elapsed_ms", elapsed.Milliseconds())
		}
		return elapsed
	}
}

// watch warns about each operation that has run longer than inFlightThreshold,
// on every tick for as long as it stays stuck, until ctx ends.
func (s *observedStore) watch(ctx context.Context) {
	t := time.NewTicker(inFlightTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.check(now)
		}
	}
}

func (s *observedStore) check(now time.Time) {
	s.mu.Lock()
	var stuck []flight
	for _, f := range s.flying {
		if now.Sub(f.since) >= inFlightThreshold {
			stuck = append(stuck, f)
		}
	}
	s.mu.Unlock()
	for _, f := range stuck {
		s.log.Warn("store phase in flight", "op", f.op, "session", f.session, "in_flight_ms", now.Sub(f.since).Milliseconds())
	}
}

func (s *observedStore) Append(ctx context.Context, session string, expectedSeq uint64, records ...[]byte) error {
	done := s.track("append", session)
	err := s.Store.Append(ctx, session, expectedSeq, records...)
	elapsed := done()
	if err == nil {
		s.observe(session, elapsed, records)
	}
	return err
}

// observe logs the creation of a session, with the time that its first append
// took, and each task that a session spawns, with the count of the process.
func (s *observedStore) observe(session string, elapsed time.Duration, records [][]byte) {
	for _, r := range records {
		env, err := eventlog.Decode(r)
		if err != nil {
			continue
		}
		switch e := env.Event.(type) {
		case eventlog.SessionCreated:
			s.log.Info("session created", "session", session, "persist_ms", elapsed.Milliseconds())
		case eventlog.ChildSpawned:
			s.mu.Lock()
			s.spawned++
			n := s.spawned
			s.mu.Unlock()
			s.log.Info("task spawned", "event", "spawned", "parent", session, "child", e.ChildID, "count", n)
		}
	}
}

func (s *observedStore) Read(ctx context.Context, session string, afterSeq uint64, limit int) ([]harness.Record, error) {
	defer s.track("read", session)()
	return s.Store.Read(ctx, session, afterSeq, limit)
}

func (s *observedStore) Head(ctx context.Context, session string) (uint64, error) {
	defer s.track("head", session)()
	return s.Store.Head(ctx, session)
}

func (s *observedStore) Sessions(ctx context.Context, after string, limit int) ([]string, error) {
	defer s.track("sessions", "")()
	return s.Store.Sessions(ctx, after, limit)
}

func (s *observedStore) PutBlob(ctx context.Context, session, key string, r io.Reader) error {
	defer s.track("put_blob", session)()
	return s.Store.PutBlob(ctx, session, key, r)
}

func (s *observedStore) GetBlob(ctx context.Context, session, key string) (io.ReadCloser, error) {
	defer s.track("get_blob", session)()
	return s.Store.GetBlob(ctx, session, key)
}
