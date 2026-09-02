package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/majorcontext/harness/provider"
)

const startupPrewarmTimeout = 15 * time.Second

var errStartupPrewarmProviderIneligible = errors.New("startup prewarm provider became ineligible during assembly")

// startupPrewarm is the one-shot, session-owned startup task. Its deadline is
// fixed when construction finishes; the first native prompt can consume it
// once, but cannot extend its lifetime.
type startupPrewarm struct {
	startedAt    time.Time
	deadline     time.Time
	cancel       context.CancelFunc
	deadlineDone <-chan struct{}
	done         chan struct{}

	consumeOnce sync.Once
	outcomeOnce sync.Once

	mu              sync.Mutex
	workerCompleted bool
	completedAt     time.Time
	resultErr       error
}

type startupPrewarmResolution struct {
	startedAt time.Time
	readyAt   time.Time
}

func (s *Session) startStartupPrewarm() {
	// Avoid all early discovery and hook work when the configured provider has
	// no startup capability. Hooks still run inside the shared assembly helper
	// for capable providers.
	configured, err := s.cfg.Providers.For(s.Model())
	if err != nil {
		return
	}
	prewarmer, ok := configured.(provider.StartupPrewarmer)
	if !ok || !prewarmer.StartupPrewarmEnabled() {
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
		startedAt:    startedAt,
		deadline:     startedAt.Add(startupPrewarmTimeout),
		cancel:       cancel,
		deadlineDone: ctx.Done(),
		done:         make(chan struct{}),
	}
	s.startupPrewarm = h
	s.mu.Unlock()

	s.emitStartupPrewarmMetrics(h, StartupPrewarmStarted, startedAt)

	// Context completion is a broadcast signal independent of worker return.
	// Only the original deadline detaches ownership here. Worker completion
	// cancels the timer but leaves the result available to the first prompt.
	go func() {
		<-h.deadlineDone
		if ctx.Err() == context.DeadlineExceeded {
			s.emitStartupPrewarmOutcome(h, StartupPrewarmTimedOut, time.Now())
			s.detachStartupPrewarm(h)
		}
	}()

	go func() {
		err := s.runStartupPrewarm(ctx)
		completedAt := time.Now()
		h.mu.Lock()
		h.workerCompleted = true
		h.completedAt = completedAt
		h.resultErr = err
		h.mu.Unlock()

		status := StartupPrewarmReady
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			status = StartupPrewarmTimedOut
		case ctx.Err() == context.Canceled:
			status = StartupPrewarmCancelled
		case err != nil:
			status = StartupPrewarmFailed
		}
		s.emitStartupPrewarmOutcome(h, status, completedAt)
		close(h.done)
		cancel()
	}()
}

func (s *Session) runStartupPrewarm(ctx context.Context) error {
	// Populate both load-once caches even when one discovery fails. The first
	// prompt reports deterministic errors through its normal checks.
	instrErr := s.ensureInstructions()
	skillsErr := s.ensureSkills()
	if instrErr != nil {
		return instrErr
	}
	if skillsErr != nil {
		return skillsErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	assembled, err := s.assembleRequest(ctx)
	if err != nil {
		return err
	}
	prewarmer, ok := assembled.provider.(provider.StartupPrewarmer)
	if !ok || !prewarmer.StartupPrewarmEnabled() {
		return errStartupPrewarmProviderIneligible
	}
	return prewarmer.Prewarm(ctx, assembled.request)
}

func (s *Session) consumeStartupPrewarm(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		s.cancelStartupPrewarmWithStatus(StartupPrewarmCancelled)
		return err
	}

	s.mu.Lock()
	h := s.startupPrewarm
	s.mu.Unlock()
	if h == nil {
		return nil
	}

	consumed := false
	h.consumeOnce.Do(func() { consumed = true })
	if !consumed {
		return ctx.Err()
	}

	select {
	case <-h.done:
		return s.consumeCompletedStartupPrewarm(ctx, h)
	case <-h.deadlineDone:
		if h.completed() {
			return s.consumeCompletedStartupPrewarm(ctx, h)
		}
		h.cancel()
		s.emitStartupPrewarmOutcome(h, StartupPrewarmTimedOut, time.Now())
		s.detachStartupPrewarm(h)
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		h.cancel()
		s.emitStartupPrewarmOutcome(h, StartupPrewarmCancelled, time.Now())
		s.detachStartupPrewarm(h)
		return ctx.Err()
	}
}

func (s *Session) consumeCompletedStartupPrewarm(ctx context.Context, h *startupPrewarm) error {
	if err := ctx.Err(); err != nil {
		h.cancel()
		s.emitStartupPrewarmOutcome(h, StartupPrewarmCancelled, time.Now())
		s.detachStartupPrewarm(h)
		return err
	}
	h.mu.Lock()
	completedAt := h.completedAt
	resultErr := h.resultErr
	h.mu.Unlock()
	if resultErr == nil {
		s.mu.Lock()
		s.startupPrewarmResolution = &startupPrewarmResolution{startedAt: h.startedAt, readyAt: completedAt}
		s.mu.Unlock()
	}
	s.detachStartupPrewarm(h)
	if err := ctx.Err(); err != nil {
		s.clearStartupPrewarmResolution()
		return err
	}
	return nil
}

func (h *startupPrewarm) completed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workerCompleted
}

func (s *Session) detachStartupPrewarm(h *startupPrewarm) {
	s.mu.Lock()
	if s.startupPrewarm == h {
		s.startupPrewarm = nil
	}
	s.mu.Unlock()
}

func (s *Session) cancelStartupPrewarm() {
	s.cancelStartupPrewarmWithStatus(StartupPrewarmCancelled)
}

func (s *Session) cancelStartupPrewarmWithStatus(status StartupPrewarmStatus) {
	s.mu.Lock()
	h := s.startupPrewarm
	s.mu.Unlock()
	if h != nil {
		s.emitStartupPrewarmOutcome(h, status, time.Now())
		h.cancel()
		s.detachStartupPrewarm(h)
	}
	s.clearStartupPrewarmResolution()
}

func (s *Session) emitStartupPrewarmOutcome(h *startupPrewarm, status StartupPrewarmStatus, at time.Time) {
	h.outcomeOnce.Do(func() {
		s.emitStartupPrewarmMetrics(h, status, at)
	})
}

func (s *Session) resolveStartupPrewarm(metadata *provider.RequestMetadata) {
	s.mu.Lock()
	resolution := s.startupPrewarmResolution
	s.startupPrewarmResolution = nil
	s.mu.Unlock()
	if resolution == nil {
		return
	}
	status := StartupPrewarmStale
	if metadata != nil && metadata.Mode == provider.RequestModeIncremental && metadata.PreviousResponseUsed && !metadata.ChainRecovered {
		status = StartupPrewarmConsumed
	}
	s.emitStartupPrewarmResolution(resolution, status, time.Now())
}

func (s *Session) clearStartupPrewarmResolution() {
	s.mu.Lock()
	s.startupPrewarmResolution = nil
	s.mu.Unlock()
}
