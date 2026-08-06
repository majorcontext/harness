package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/provider"
)

// streamWatchdog bounds the gap between consecutive provider stream events
// within one model call (see Config.StreamIdleTimeout, and Codex's
// stream_idle_timeout_ms for the same knob in the wild). It owns a single
// timer goroutine: every kick resets the timer; if the timer ever fires,
// the watchdog cancels the request's child context — unblocking the
// adapter's HTTP body read exactly the way a caller abort would — and
// records that IT was the one that fired, so explain can convert the
// resulting cancellation error into a classified, named idle-timeout
// failure instead of an anonymous "context canceled".
//
// A permanently silent stream — no bytes, no EOF, no error — is otherwise
// unbounded: nothing in the engine or the adapters would ever cut it, and
// the turn (and any goal loop driving it) wedges forever. Field report
// 2026-08-06 (finding 2b) requested exactly this bound after observing
// streams stall with single-digit chunk counts while the transport stayed
// open.
//
// All methods are nil-receiver-safe so the disabled path costs nothing.
type streamWatchdog struct {
	cancel  context.CancelFunc
	timeout time.Duration
	kicks   chan struct{}
	stopped chan struct{}
	fired   atomic.Bool
}

// armIdleWatchdog wires the session's idle-stream watchdog around one
// provider stream call: it derives a cancellable child context, arms the
// watchdog against it, and returns the context to dial with, the watchdog
// (nil when disabled — all methods are nil-safe), and a release func the
// caller must defer. Used by streamTurn, evaluateGoal, and the compaction
// summarizer alike: the evaluator runs at every goal turn boundary and
// maybeAutoCompact at the top of every Prompt, so an unwatched silent
// stream at either site wedges the session exactly the way an unwatched
// worker stream would — while holding the server's run slot.
func (s *Session) armIdleWatchdog(ctx context.Context) (context.Context, *streamWatchdog, func()) {
	if s.cfg.StreamIdleTimeout <= 0 {
		return ctx, nil, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	w := startStreamWatchdog(cancel, s.cfg.StreamIdleTimeout)
	return ctx, w, func() {
		w.stop()
		cancel()
	}
}

// startStreamWatchdog arms a watchdog that calls cancel if timeout elapses
// with no kick. The caller must defer stop.
func startStreamWatchdog(cancel context.CancelFunc, timeout time.Duration) *streamWatchdog {
	w := &streamWatchdog{
		cancel:  cancel,
		timeout: timeout,
		kicks:   make(chan struct{}, 1),
		stopped: make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *streamWatchdog) run() {
	t := time.NewTimer(w.timeout)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			// Order matters: fired must be observable before the
			// cancellation can surface anywhere, so explain never sees a
			// cancel it cannot attribute.
			w.fired.Store(true)
			w.cancel()
			return
		case <-w.kicks:
			if !t.Stop() {
				// Timer already fired into the channel between the kick
				// arriving and Stop; drain it so Reset arms cleanly. The
				// non-blocking drain is safe: this goroutine is t.C's only
				// receiver.
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(w.timeout)
		case <-w.stopped:
			return
		}
	}
}

// kick notes stream activity, resetting the idle timer. Never blocks: the
// buffered channel coalesces bursts, and a kick racing the timer's own
// firing is moot either way.
func (w *streamWatchdog) kick() {
	if w == nil {
		return
	}
	select {
	case w.kicks <- struct{}{}:
	default:
	}
}

// stop releases the watchdog goroutine. Must be called exactly once (defer
// it at arm time); safe if the watchdog already fired.
func (w *streamWatchdog) stop() {
	if w == nil {
		return
	}
	close(w.stopped)
}

// explain converts err into the classified idle-timeout error when THIS
// watchdog cut the stream, and returns err untouched otherwise — a parent
// context's own cancellation (caller abort, shutdown) is never
// reclassified. The constructed error deliberately does NOT wrap the
// underlying cancellation: chaining to context.Canceled would make every
// retry loop's errors.Is(err, context.Canceled) abort check read the cut
// as deliberate.
//
// The conversion requires BOTH fired and a cancellation-shaped err, not
// fired alone: the watchdog's own cut always surfaces as the child
// context's cancellation, so an error that does NOT chain to
// context.Canceled arriving in the same instant the timer fires is a real
// provider failure that must keep its own identity — most pointedly a
// context-overflow classification, whose deliberate clear-don't-park
// semantics (see AGENTS.md) would otherwise be laundered into a retryable
// truncation.
func (w *streamWatchdog) explain(err error) error {
	if w == nil || err == nil || !w.fired.Load() || !errors.Is(err, context.Canceled) {
		return err
	}
	return provider.MarkRetryable(
		fmt.Errorf("engine: provider stream idle for %s with the response unfinished (stream idle watchdog cut the request)", w.timeout),
		provider.RetryableStreamTruncated,
	)
}
