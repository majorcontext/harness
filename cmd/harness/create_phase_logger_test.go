package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestCreatePhaseLoggerEmptiesMapOnTotal is a regression test for the
// phase-accumulator leak (PR #87 review): a create that reports every
// intermediate phase before "total" must leave byID empty afterward, not
// just render a sensible summary line.
func TestCreatePhaseLoggerEmptiesMapOnTotal(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	c := newCreatePhaseLogger(logger)

	const id = "ses_0000000000000001"
	c.OnCreatePhase(id, "new_session", 1*time.Millisecond)
	c.OnCreatePhase(id, "persist", 2*time.Millisecond)
	c.OnCreatePhase(id, "register", 3*time.Millisecond)
	c.OnCreatePhase(id, "emit_created", 4*time.Millisecond)
	c.OnCreatePhase(id, "total", 10*time.Millisecond)

	c.mu.Lock()
	n := len(c.byID)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("byID has %d entries after total, want 0: %+v", n, c.byID)
	}

	line := buf.String()
	if !strings.Contains(line, "session create phases") {
		t.Fatalf("log output = %q, want a session create phases summary line", line)
	}
	for _, field := range []string{"new_session_ms=1", "persist_ms=2", "register_ms=3", "emit_created_ms=4", "total_ms=10"} {
		if !strings.Contains(line, field) {
			t.Errorf("log output = %q, want field %q", line, field)
		}
	}
}

// TestCreatePhaseLoggerHandlesTotalWithoutIntermediatePhases is the
// regression case the leak actually manifested as: NEP-4897's saturated
// volume makes Persist fail on every create, so handleCreate's defer (see
// server/handlers.go) reports "new_session" then jumps straight to "total"
// with no persist/register/emit_created in between. The summary line must
// still render — with the missing phases simply absent — and the map entry
// must still be reclaimed, not orphaned.
func TestCreatePhaseLoggerHandlesTotalWithoutIntermediatePhases(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	c := newCreatePhaseLogger(logger)

	const id = "ses_0000000000000002"
	c.OnCreatePhase(id, "new_session", 1*time.Millisecond)
	// persist failed; handleCreate's defer fires "total" directly.
	c.OnCreatePhase(id, "total", 5*time.Millisecond)

	c.mu.Lock()
	n := len(c.byID)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("byID has %d entries after total, want 0 (leaked on a failed create): %+v", n, c.byID)
	}

	line := buf.String()
	if !strings.Contains(line, "session create phases") {
		t.Fatalf("log output = %q, want a session create phases summary line", line)
	}
	if !strings.Contains(line, "new_session_ms=1") || !strings.Contains(line, "total_ms=5") {
		t.Fatalf("log output = %q, want new_session_ms=1 and total_ms=5", line)
	}
	for _, absent := range []string{"persist_ms", "register_ms", "emit_created_ms"} {
		if strings.Contains(line, absent) {
			t.Errorf("log output = %q, want no %s field (that phase never ran)", line, absent)
		}
	}
}

// TestCreatePhaseLoggerKeysConcurrentCreatesIndependently proves the
// mutex-guarded, session-ID-keyed map does not cross-contaminate phases
// between two creates in flight at once — the exact scenario the doc
// comment on createPhaseLogger calls out.
func TestCreatePhaseLoggerKeysConcurrentCreatesIndependently(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	c := newCreatePhaseLogger(logger)

	const idA, idB = "ses_00000000000000aa", "ses_00000000000000bb"
	c.OnCreatePhase(idA, "new_session", 1*time.Millisecond)
	c.OnCreatePhase(idB, "new_session", 1*time.Millisecond)
	c.OnCreatePhase(idA, "persist", 2*time.Millisecond)
	c.OnCreatePhase(idB, "total", 9*time.Millisecond) // B fails right after new_session
	c.OnCreatePhase(idA, "register", 3*time.Millisecond)
	c.OnCreatePhase(idA, "emit_created", 4*time.Millisecond)
	c.OnCreatePhase(idA, "total", 10*time.Millisecond)

	c.mu.Lock()
	n := len(c.byID)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("byID has %d entries after both totals, want 0: %+v", n, c.byID)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2: %q", len(lines), buf.String())
	}
	// B's total lands first (its create failed right after new_session, so
	// its summary carries only new_session_ms/total_ms); A's total lands
	// second, with the full phase set. Either line carrying the other
	// session's phases would mean the map is cross-contaminating.
	if strings.Contains(lines[0], "persist_ms") {
		t.Errorf("B's summary unexpectedly carries persist_ms (leaked from A): %q", lines[0])
	}
	if !strings.Contains(lines[1], "persist_ms") {
		t.Errorf("A's summary is missing persist_ms: %q", lines[1])
	}
}
