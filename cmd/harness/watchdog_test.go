package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestWatchdogWarnsWhileStorePhaseStuck drives inFlightWatchdog directly
// (start/done/check), never a real ticker or sleep: a phase is started via
// startStorePhase and never completed, check is called with a synthetic
// "now" past inFlightWatchdogThreshold, and a warn record must name the
// stuck op/phase. Completing it (doneStorePhase) must silence subsequent
// checks — this is the regression the watchdog exists to prevent: a
// permanently hung phase produces zero completion log lines, so the
// in-flight table (not the completion callback) is the only thing that can
// ever warn about it.
func TestWatchdogWarnsWhileStorePhaseStuck(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	w := newInFlightWatchdog(logger)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }
	w.startStorePhase("ensure_log", "open")

	// Not yet past the threshold: no warn.
	w.check(t0.Add(3 * time.Second))
	if buf.Len() != 0 {
		t.Fatalf("warned before threshold: %s", buf.String())
	}

	// Past the threshold: warn, naming op/phase/in_flight_ms.
	w.check(t0.Add(6 * time.Second))
	line := buf.String()
	if !strings.Contains(line, "store phase in flight") {
		t.Fatalf("log = %q, want a \"store phase in flight\" warn", line)
	}
	for _, field := range []string{`"op":"ensure_log"`, `"phase":"open"`, `"in_flight_ms":6000`} {
		if !strings.Contains(line, field) {
			t.Errorf("log = %q, want field %s", line, field)
		}
	}
	if strings.Contains(line, `"session"`) {
		t.Errorf("log = %q, store-phase entries must never carry a session field", line)
	}

	// Repeats on every subsequent tick while still stuck (bounded to one
	// line per stuck phase per check call).
	buf.Reset()
	w.check(t0.Add(11 * time.Second))
	if !strings.Contains(buf.String(), "store phase in flight") {
		t.Fatalf("watchdog stopped warning on a later tick while still stuck: %q", buf.String())
	}

	// Completion removes the entry: no further warns.
	buf.Reset()
	w.doneStorePhase("ensure_log", "open")
	w.check(t0.Add(20 * time.Second))
	if buf.Len() != 0 {
		t.Fatalf("warned after completion cleared the entry: %s", buf.String())
	}
}

// TestWatchdogClearsEntryOnErrorTimedCompletion models serveCmd's actual
// wiring shape (see main.go's storePhase/onCreatePhase closures): the
// watchdog's done call is unconditional, made BEFORE delegating to the
// existing completion logger, regardless of whether the underlying
// operation succeeded or errored. This is the piece that only works end to
// end because of the PR #89 review fix one layer down (engine's
// timedStorePhase / server's timedCreatePhase now guarantee OnStorePhase/
// OnCreatePhase fire on error too, not just success) — a start with no
// matching completion call at all is exactly what leaves a permanent, false
// "still stuck" warning. Here that is modeled directly: start, then a
// done call carrying error-path characteristics (a short elapsed, as a
// fast-failing EIO/ENOSPC would produce) — the entry must be gone
// afterward, with no warn on a later check.
func TestWatchdogClearsEntryOnErrorTimedCompletion(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	w := newInFlightWatchdog(logger)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }

	// storePhase mirrors serveCmd's own closure: clear the watchdog entry
	// unconditionally, then delegate to whatever the completion logger does
	// with the (op, phase, elapsed) triple — irrespective of whether the
	// underlying operation errored, which this signature can't even see.
	var completionLogged []string
	storePhase := func(op, phase string, elapsed time.Duration) {
		w.doneStorePhase(op, phase)
		completionLogged = append(completionLogged, op+"/"+phase)
	}

	w.startStorePhase("ensure_log", "open")
	// A fast-failing EIO/ENOSPC still reports a real (short) elapsed —
	// nothing about the watchdog's clearing depends on the outcome or the
	// duration.
	storePhase("ensure_log", "open", 2*time.Millisecond)

	if len(completionLogged) != 1 || completionLogged[0] != "ensure_log/open" {
		t.Fatalf("completion logger calls = %v, want exactly one for ensure_log/open", completionLogged)
	}
	w.check(t0.Add(20 * time.Second))
	if buf.Len() != 0 {
		t.Fatalf("warned after an error-path completion cleared the entry: %s", buf.String())
	}
}

// TestWatchdogWarnsWhileCreatePhaseStuckIncludesSession is the create-phase
// counterpart: entries started via startCreatePhase must carry the session
// ID in the warn record, and doneCreatePhase must clear them the same way
// doneStorePhase does.
func TestWatchdogWarnsWhileCreatePhaseStuckIncludesSession(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	w := newInFlightWatchdog(logger)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }
	w.startCreatePhase("ses_abc123", "persist")

	w.check(t0.Add(6 * time.Second))
	line := buf.String()
	if !strings.Contains(line, "store phase in flight") {
		t.Fatalf("log = %q, want a \"store phase in flight\" warn", line)
	}
	for _, field := range []string{`"phase":"persist"`, `"session":"ses_abc123"`, `"in_flight_ms":6000`} {
		if !strings.Contains(line, field) {
			t.Errorf("log = %q, want field %s", line, field)
		}
	}

	buf.Reset()
	w.doneCreatePhase("ses_abc123", "persist")
	w.check(t0.Add(20 * time.Second))
	if buf.Len() != 0 {
		t.Fatalf("warned after completion cleared the entry: %s", buf.String())
	}
}

// TestWatchdogTracksMultipleEntriesIndependently proves two distinct
// in-flight entries (one store, one create) don't clobber each other's
// table slot, and completing one leaves the other's warn intact.
func TestWatchdogTracksMultipleEntriesIndependently(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	w := newInFlightWatchdog(logger)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }
	w.startStorePhase("enqueue_durable", "fsync")
	w.startCreatePhase("ses_xyz", "register")

	w.check(t0.Add(6 * time.Second))
	lines := strings.TrimSpace(buf.String())
	if n := strings.Count(lines, "store phase in flight"); n != 2 {
		t.Fatalf("got %d warn lines, want 2 (one per stuck entry): %q", n, lines)
	}

	buf.Reset()
	w.doneStorePhase("enqueue_durable", "fsync")
	w.check(t0.Add(11 * time.Second))
	line := buf.String()
	if strings.Contains(line, `"op":"enqueue_durable"`) {
		t.Errorf("completed store entry still warned: %q", line)
	}
	if !strings.Contains(line, `"session":"ses_xyz"`) {
		t.Errorf("still-stuck create entry stopped warning: %q", line)
	}
}
