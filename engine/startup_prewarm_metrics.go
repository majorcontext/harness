package engine

import "time"

// StartupPrewarmStatus is one observable startup-prewarm lifecycle state.
type StartupPrewarmStatus string

const (
	StartupPrewarmStarted   StartupPrewarmStatus = "started"
	StartupPrewarmReady     StartupPrewarmStatus = "ready"
	StartupPrewarmConsumed  StartupPrewarmStatus = "consumed"
	StartupPrewarmFailed    StartupPrewarmStatus = "failed"
	StartupPrewarmTimedOut  StartupPrewarmStatus = "timed_out"
	StartupPrewarmCancelled StartupPrewarmStatus = "cancelled"
	StartupPrewarmStale     StartupPrewarmStatus = "stale"
)

// StartupPrewarmMetrics contains non-secret lifecycle timing for one startup
// prewarm. DurationMillis measures work through readiness or termination.
// AgeMillis measures time since scheduling when the status was resolved.
type StartupPrewarmMetrics struct {
	SessionID      string
	Status         StartupPrewarmStatus
	DurationMillis int64
	AgeMillis      int64
}

func (s *Session) emitStartupPrewarmMetrics(h *startupPrewarm, status StartupPrewarmStatus, at time.Time) {
	duration := at.Sub(h.startedAt)
	if duration < 0 {
		duration = 0
	}
	s.emitStartupPrewarmMetric(StartupPrewarmMetrics{
		SessionID:      s.ID,
		Status:         status,
		DurationMillis: duration.Milliseconds(),
		AgeMillis:      duration.Milliseconds(),
	})
}

func (s *Session) emitStartupPrewarmResolution(resolution *startupPrewarmResolution, status StartupPrewarmStatus, at time.Time) {
	duration := resolution.readyAt.Sub(resolution.startedAt)
	age := at.Sub(resolution.startedAt)
	if duration < 0 {
		duration = 0
	}
	if age < 0 {
		age = 0
	}
	s.emitStartupPrewarmMetric(StartupPrewarmMetrics{
		SessionID:      s.ID,
		Status:         status,
		DurationMillis: duration.Milliseconds(),
		AgeMillis:      age.Milliseconds(),
	})
}

func (s *Session) emitStartupPrewarmMetric(metric StartupPrewarmMetrics) {
	if s.cfg.OnStartupPrewarmMetrics != nil {
		s.cfg.OnStartupPrewarmMetrics(metric)
		return
	}
	defaultTurnMetricsStderr.Info("startup_prewarm",
		"session_id", metric.SessionID,
		"status", metric.Status,
		"duration_ms", metric.DurationMillis,
		"age_ms", metric.AgeMillis,
	)
}
