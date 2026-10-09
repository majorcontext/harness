package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/majorcontext/harness/internal/turn"
)

// warmTimeout bounds a warm-up.
const warmTimeout = 15 * time.Second

// warmup is the warm-up that Run started. done closes when it ends; startedAt,
// readyAt, and ready are final from then on. resolved belongs to the actor.
type warmup struct {
	done      chan struct{}
	cancel    context.CancelFunc
	startedAt time.Time
	readyAt   time.Time
	ready     bool
	resolved  bool
}

// warm asks the backend to prepare its transport for the first turn, from a
// goroutine under the session context. It runs once when the actor runs, on
// create and on wake, before any turn starts, and ends with the actor. A
// failed warm-up costs only the speed of the first turn, so its error is
// logged on the startup_prewarm line and goes no further.
func (a *Actor) warm() {
	if !turn.CanWarm(a.cfg.Backend, a.state.Model()) {
		return
	}
	req := a.modelRequest()
	src := a.source(&running{})
	ctx, cancel := context.WithTimeout(a.cfg.Base, warmTimeout)
	w := &warmup{done: make(chan struct{}), cancel: cancel, startedAt: time.Now()}
	a.warming = w
	a.logWarm("started", 0, 0)
	a.cfg.Go(func() {
		defer close(w.done)
		defer cancel()
		go func() {
			select {
			case <-a.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		err := turn.Warm(ctx, a.cfg.Backend, req, src)
		w.readyAt = time.Now()
		status := "ready"
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			status = "timed_out"
		case errors.Is(ctx.Err(), context.Canceled):
			status = "cancelled"
		case err != nil:
			status = "failed"
		}
		w.ready = status == "ready"
		elapsed := w.readyAt.Sub(w.startedAt)
		a.logWarm(status, elapsed, elapsed)
	})
}

// awaitWarm waits for the warm-up in flight, so a turn never dials the
// transport while the warm-up is about to hold it. A warm-up ends within
// warmTimeout or with the actor. A turn that ends while it waits cancels the
// warm-up.
func (a *Actor) awaitWarm(ctx context.Context) {
	if a.warming == nil {
		return
	}
	select {
	case <-a.warming.done:
	case <-ctx.Done():
		a.warming.cancel()
	}
}

// resolveWarm logs, on the first model call that follows a warm-up that
// ended ready, whether the call chained from it ("consumed") or not
// ("stale"). It runs in the actor.
func (a *Actor) resolveWarm(c *turn.CallMetrics) {
	w := a.warming
	if w == nil || w.resolved {
		return
	}
	select {
	case <-w.done:
	default:
		return
	}
	w.resolved = true
	if !w.ready {
		return
	}
	status := "stale"
	if c.Chained() {
		status = "consumed"
	}
	a.logWarm(status, w.readyAt.Sub(w.startedAt), time.Since(w.startedAt))
}

// logWarm writes one startup_prewarm line. duration is the time to ready or
// to the end of the warm-up; age is the time since it started.
func (a *Actor) logWarm(status string, duration, age time.Duration) {
	slog.Info("startup_prewarm", "session_id", a.cfg.ID, "status", status, "duration_ms", duration.Milliseconds(), "age_ms", age.Milliseconds())
}
