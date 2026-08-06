package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// stallProvider returns streams built by mk, capturing the ctx passed to
// Stream — the watchdog cuts a stalled stream by cancelling exactly that
// ctx, the same way a real adapter's HTTP body read unblocks.
type stallProvider struct {
	mk func(ctx context.Context) provider.Stream
}

func (p *stallProvider) Name() string { return "stall" }
func (p *stallProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	return p.mk(ctx), nil
}

// hangingStream blocks every Next on ctx — a provider stream that goes
// permanently silent without dying: no bytes, no EOF, no error. This is
// the field report's missing-watchdog fingerprint (finding 2b): with no
// idle bound, such a stream wedges the turn forever.
type hangingStream struct{ ctx context.Context }

func (s *hangingStream) Next() (provider.Event, error) {
	<-s.ctx.Done()
	return provider.Event{}, s.ctx.Err()
}
func (s *hangingStream) Close() error { return nil }

// dripStream delivers one scripted event per Next after a fixed delay —
// slow but alive.
type dripStream struct {
	ctx    context.Context
	delay  time.Duration
	events []provider.Event
	i      int
}

func (s *dripStream) Next() (provider.Event, error) {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	}
	if s.i >= len(s.events) {
		return provider.Event{}, errors.New("dripStream: script exhausted")
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}
func (s *dripStream) Close() error { return nil }

func doneEvent(text string) provider.Event {
	return provider.Event{
		Type:       provider.EventDone,
		Message:    &message.Message{ID: "msg_done", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}},
		StopReason: provider.StopEndTurn,
	}
}

// TestStreamIdleWatchdogCutsStalledStream: with StreamIdleTimeout set, a
// stream that delivers nothing for that long must be cut and the failure
// classified RetryableStreamTruncated — carrying a message that names the
// watchdog and the timeout, and NOT reading as a caller abort
// (errors.Is(err, context.Canceled) must be false, or every retry loop
// would treat the cut as a deliberate stop and give up).
func TestStreamIdleWatchdogCutsStalledStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &stallProvider{mk: func(ctx context.Context) provider.Stream {
			return &hangingStream{ctx: ctx}
		}}
		s := NewSession(Config{
			Providers:         provider.Registry{"stall": prov},
			Model:             message.ModelRef{Provider: "stall", Model: "m"},
			StreamIdleTimeout: 90 * time.Second,
		})
		start := time.Now()
		_, err := s.Prompt(context.Background(), "go")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("Prompt against a permanently silent stream succeeded, want watchdog cut")
		}
		if elapsed != 90*time.Second {
			t.Errorf("elapsed = %v, want exactly 90s (the idle timeout)", elapsed)
		}
		class, ok := provider.AsRetryable(err)
		if !ok || class != provider.RetryableStreamTruncated {
			t.Fatalf("AsRetryable(%v) = %q, %v; want %q, true", err, class, ok, provider.RetryableStreamTruncated)
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("err = %v matches context.Canceled — a watchdog cut must never read as a caller abort", err)
		}
	})
}

// TestStreamIdleWatchdogResetsOnActivity: the timeout bounds the gap
// BETWEEN events, not the turn's total duration — a slow-but-alive stream
// (events every 60s against a 90s idle bound, 3 minutes total) must
// complete normally. This is what distinguishes an idle watchdog from the
// rejected total-duration deadline: legitimate long turns stay legal.
func TestStreamIdleWatchdogResetsOnActivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &stallProvider{mk: func(ctx context.Context) provider.Stream {
			return &dripStream{ctx: ctx, delay: 60 * time.Second, events: []provider.Event{
				{Type: provider.EventTextDelta, Text: "a"},
				{Type: provider.EventTextDelta, Text: "b"},
				doneEvent("ab"),
			}}
		}}
		s := NewSession(Config{
			Providers:         provider.Registry{"stall": prov},
			Model:             message.ModelRef{Provider: "stall", Model: "m"},
			StreamIdleTimeout: 90 * time.Second,
		})
		start := time.Now()
		msg, err := s.Prompt(context.Background(), "go")
		if err != nil {
			t.Fatalf("Prompt = %v, want success (every inter-event gap is below the idle timeout)", err)
		}
		if got := time.Since(start); got != 3*time.Minute {
			t.Errorf("elapsed = %v, want 3m (3 events x 60s)", got)
		}
		if msg.Parts.Text() != "ab" {
			t.Errorf("text = %q, want %q", msg.Parts.Text(), "ab")
		}
	})
}

// TestStreamIdleWatchdogParentCancelStaysCanceled: a caller abort (POST
// /abort cancelling the turn ctx) during a stalled stream must surface as
// context.Canceled — never converted into a retryable truncation, or an
// abort would trigger retries of the very turn the operator just killed.
func TestStreamIdleWatchdogParentCancelStaysCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &stallProvider{mk: func(ctx context.Context) provider.Stream {
			return &hangingStream{ctx: ctx}
		}}
		s := NewSession(Config{
			Providers:         provider.Registry{"stall": prov},
			Model:             message.ModelRef{Provider: "stall", Model: "m"},
			StreamIdleTimeout: 90 * time.Second,
		})
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Second) // well inside the idle window
			cancel()
		}()
		_, err := s.Prompt(ctx, "go")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled (caller abort)", err)
		}
		if _, ok := provider.AsRetryable(err); ok {
			t.Errorf("caller abort classified retryable: %v", err)
		}
	})
}

// TestStreamIdleWatchdogDefaultFiveMinutes: an UNSET StreamIdleTimeout
// defaults to 5 minutes (matching Codex's stream_idle_timeout_ms default of
// 300_000 — the same knob for the same failure mode), so a permanently
// silent stream can never wedge a turn forever out of the box. A healthy
// stream is never idle-anywhere-near that long: Anthropic sends ping
// keep-alives between content events.
func TestStreamIdleWatchdogDefaultFiveMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &stallProvider{mk: func(ctx context.Context) provider.Stream {
			return &hangingStream{ctx: ctx}
		}}
		s := NewSession(Config{
			Providers: provider.Registry{"stall": prov},
			Model:     message.ModelRef{Provider: "stall", Model: "m"},
		})
		start := time.Now()
		_, err := s.Prompt(context.Background(), "go")
		if err == nil {
			t.Fatal("Prompt against a permanently silent stream succeeded, want the default watchdog to cut it")
		}
		if got := time.Since(start); got != 5*time.Minute {
			t.Errorf("elapsed = %v, want exactly 5m (the default idle timeout)", got)
		}
		if class, ok := provider.AsRetryable(err); !ok || class != provider.RetryableStreamTruncated {
			t.Fatalf("AsRetryable(%v) = %q, %v; want %q, true", err, class, ok, provider.RetryableStreamTruncated)
		}
	})
}

// TestStreamIdleWatchdogNegativeDisables: a negative StreamIdleTimeout is
// the explicit opt-out — a slow stream is left alone indefinitely.
func TestStreamIdleWatchdogNegativeDisables(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &stallProvider{mk: func(ctx context.Context) provider.Stream {
			return &dripStream{ctx: ctx, delay: 10 * time.Minute, events: []provider.Event{doneEvent("late")}}
		}}
		s := NewSession(Config{
			Providers:         provider.Registry{"stall": prov},
			Model:             message.ModelRef{Provider: "stall", Model: "m"},
			StreamIdleTimeout: -1,
		})
		msg, err := s.Prompt(context.Background(), "go")
		if err != nil {
			t.Fatalf("Prompt = %v, want success with the watchdog disabled", err)
		}
		if msg.Parts.Text() != "late" {
			t.Errorf("text = %q, want %q", msg.Parts.Text(), "late")
		}
	})
}

// TestStreamWatchdogExplainKeepsRealErrorIdentity: once the watchdog has
// fired, explain must still pass a NON-cancellation error through untouched
// — a real provider failure racing the timer (most pointedly a
// context-overflow classification, whose clear-don't-park semantics are
// deliberate) must never be laundered into a retryable truncation. Only the
// watchdog's own cut — which always surfaces as the child context's
// cancellation — is converted.
func TestStreamWatchdogExplainKeepsRealErrorIdentity(t *testing.T) {
	w := startStreamWatchdog(func() {}, time.Nanosecond)
	defer w.stop()
	for !w.fired.Load() {
		// The nanosecond timer fires effectively immediately; spin until
		// the flag is observable (bounded by the test binary timeout).
		runtime.Gosched()
	}
	realErr := errors.New("anthropic: prompt is too long (invalid_request_error, HTTP 400)")
	if got := w.explain(realErr); got != realErr { //nolint:errorlint // identity is the point
		t.Fatalf("explain(real error) = %v, want the error returned unchanged", got)
	}
	cancelErr := fmt.Errorf("read: %w", context.Canceled)
	got := w.explain(cancelErr)
	if class, ok := provider.AsRetryable(got); !ok || class != provider.RetryableStreamTruncated {
		t.Fatalf("explain(cancellation) = %v, want classified %q", got, provider.RetryableStreamTruncated)
	}
}
