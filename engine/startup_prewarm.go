package engine

import (
	"context"
	"sync"
	"time"

	"github.com/majorcontext/harness/provider"
)

const startupPrewarmTimeout = 15 * time.Second

// startupPrewarm is the one-shot, session-owned startup task. Its deadline is
// fixed when construction finishes; the first native prompt can consume it
// once, but cannot extend its lifetime.
type startupPrewarm struct {
	startedAt time.Time
	deadline  time.Time
	cancel    context.CancelFunc
	done      chan struct{}

	consumeOnce sync.Once
}

func (s *Session) startStartupPrewarm() {
	// Avoid all early discovery and hook work when the configured provider has
	// no startup capability. Hooks still run inside the shared assembly helper
	// for capable providers.
	configured, err := s.cfg.Providers.For(s.Model())
	if err != nil {
		return
	}
	if _, ok := configured.(provider.StartupPrewarmer); !ok {
		return
	}

	s.mu.Lock()
	if !s.startupPrewarmEligible || s.startupPrewarm != nil {
		s.mu.Unlock()
		return
	}
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), startupPrewarmTimeout)
	h := &startupPrewarm{
		startedAt: startedAt,
		deadline:  startedAt.Add(startupPrewarmTimeout),
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	s.startupPrewarm = h
	s.mu.Unlock()

	go func() {
		defer close(h.done)
		defer cancel()

		// Populate both load-once caches even when one discovery fails. The
		// first prompt reports those deterministic errors through its normal
		// checks; startup-only failures remain best effort.
		instrErr := s.ensureInstructions()
		skillsErr := s.ensureSkills()
		if instrErr != nil || skillsErr != nil || ctx.Err() != nil {
			return
		}

		assembled, err := s.assembleRequest(ctx)
		if err != nil {
			return
		}
		prewarmer, ok := assembled.provider.(provider.StartupPrewarmer)
		if !ok {
			return
		}
		_ = prewarmer.Prewarm(ctx, assembled.request)
	}()
}

func (s *Session) consumeStartupPrewarm(ctx context.Context) error {
	s.mu.Lock()
	h := s.startupPrewarm
	s.mu.Unlock()
	if h == nil {
		return nil
	}

	consumed := false
	h.consumeOnce.Do(func() { consumed = true })
	if !consumed {
		return nil
	}

	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		h.cancel()
		return ctx.Err()
	}
}

func (s *Session) cancelStartupPrewarm() {
	s.mu.Lock()
	h := s.startupPrewarm
	s.mu.Unlock()
	if h != nil {
		h.cancel()
	}
}
