package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestTaskEventLoggerCountsAndLogs is the regression test for a follow-up
// finding ("metrics"): taskEventLogger.OnTaskEvent accumulates per-event
// counts and logs each occurrence, mirroring createPhaseLogger's own
// counters-plus-slog shape (see TestCreatePhaseLoggerEmptiesMapOnTotal).
func TestTaskEventLoggerCountsAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	e := newTaskEventLogger(logger)

	e.OnTaskEvent("spawned", "ses_parent1", "ses_child1")
	e.OnTaskEvent("spawned", "ses_parent1", "ses_child2")
	e.OnTaskEvent("depth_refused", "ses_parent2", "")

	counts := e.Counts()
	if counts["spawned"] != 2 {
		t.Errorf("Counts()[\"spawned\"] = %d, want 2", counts["spawned"])
	}
	if counts["depth_refused"] != 1 {
		t.Errorf("Counts()[\"depth_refused\"] = %d, want 1", counts["depth_refused"])
	}

	out := buf.String()
	if !strings.Contains(out, "task spawned") {
		t.Errorf("log output missing \"task spawned\" line: %s", out)
	}
	if !strings.Contains(out, "task spawn refused") {
		t.Errorf("log output missing \"task spawn refused\" line: %s", out)
	}
	if !strings.Contains(out, "child=ses_child1") {
		t.Errorf("log output missing child id: %s", out)
	}
}
