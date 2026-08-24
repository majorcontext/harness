// These tests exercise Manager with real `sh` subprocesses — deliberately:
// the subprocess machinery (process groups, ready-line detection, death
// detection) is exactly what's under test, the sanctioned exception to
// "never spawn real subprocess fixtures" in AGENTS.md (see
// engine/bash_pipe_test.go for the precedent). Long-lived fixture
// processes print a ready line, if any, and then `sleep` (Manager does not
// wire a managed process's stdin, so a script waiting on stdin would spin
// on an immediate EOF from /dev/null instead of actually blocking) so a
// test can Stop them deterministically rather than waiting them out.
// Because real OS processes run in real wall-clock time, none of this uses
// testing/synctest; every wait is either a channel-blocking call already
// under test (Start/Stop) or a short, deadline-bound poll loop over
// observable state — never a raw time.Sleep for synchronization.
package process

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitForState(t *testing.T, m *Manager, name string, want State, deadline time.Duration) Status {
	t.Helper()
	end := time.Now().Add(deadline)
	var last Status
	for time.Now().Before(end) {
		st, err := m.Status(name)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		last = st
		if st.State == want {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("process %q did not reach state %q within %s (last status: %+v)", name, want, deadline, last)
	return last
}

func TestStartNoReadyRegex_ReadyImmediately(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {Command: []string{"sh", "-c", "echo started; sleep 100"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := m.Start(ctx, "dev")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != StateReady || !st.Ready {
		t.Fatalf("Start status = %+v, want ready", st)
	}
	if st.PID == 0 {
		t.Errorf("PID = 0, want nonzero")
	}
	wantLog := filepath.Join(dir, ".harness", "proc", "dev.log")
	if st.Log != wantLog {
		t.Errorf("Log = %q, want %q", st.Log, wantLog)
	}
	if _, err := os.Stat(wantLog); err != nil {
		t.Errorf("log file missing: %v", err)
	}

	if _, err := m.Stop(ctx, "dev"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartIdempotent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {Command: []string{"sh", "-c", "echo started; sleep 100"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, err := m.Start(ctx, "dev")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	second, err := m.Start(ctx, "dev")
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if second.PID != first.PID {
		t.Errorf("second Start spawned a new process: PID %d != %d", second.PID, first.PID)
	}
	m.Stop(ctx, "dev")
}

func TestStartWithReadyRegex_BlocksUntilMatch(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {
			Command:      []string{"sh", "-c", `echo "Ready in 12ms"; sleep 100`},
			ReadyRegex:   `Ready in .*ms`,
			ReadyTimeout: 5 * time.Second,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := m.Start(ctx, "dev")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != StateReady || !st.Ready {
		t.Fatalf("Start status = %+v, want ready", st)
	}
	m.Stop(ctx, "dev")
}

func TestStartWithReadyRegex_TimesOut(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {
			Command:      []string{"sh", "-c", "sleep 100"}, // never prints a ready line
			ReadyRegex:   `Ready in .*ms`,
			ReadyTimeout: 50 * time.Millisecond,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := m.Start(ctx, "dev")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != StateRunning {
		t.Fatalf("Start status = %+v, want running (timed out but left running)", st)
	}
	if !strings.Contains(st.Note, "timed out") {
		t.Errorf("Note = %q, want a timeout note", st.Note)
	}
	if st.Ready {
		t.Errorf("Ready = true, want false (never matched)")
	}

	// The process must genuinely still be running (never killed by a
	// ready-gate timeout).
	st2, err := m.Status("dev")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st2.State != StateRunning {
		t.Fatalf("Status after timeout = %+v, want still running", st2)
	}

	m.Stop(ctx, "dev")
}

func TestProcessDeathDetected(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {Command: []string{"sh", "-c", "exit 7"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := m.Start(ctx, "dev"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	st := waitForState(t, m, "dev", StateExited, 3*time.Second)
	if !st.HasExitCode || st.ExitCode != 7 {
		t.Errorf("exit status = %+v, want code 7", st)
	}
	if st.FinishedAt.IsZero() {
		t.Errorf("FinishedAt not set")
	}
}

func TestStopRecordsExit(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {Command: []string{"sh", "-c", "echo started; sleep 100"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := m.Start(ctx, "dev"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err := m.Stop(ctx, "dev")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st.State != StateStopped {
		t.Fatalf("Stop status = %+v, want stopped", st)
	}
	if !st.HasExitCode {
		t.Errorf("HasExitCode = false, want true after Stop")
	}
	if st.FinishedAt.IsZero() {
		t.Errorf("FinishedAt not set after Stop")
	}

	// Stopping an already-stopped process is a safe no-op.
	st2, err := m.Stop(ctx, "dev")
	if err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if st2.State != StateStopped {
		t.Errorf("second Stop status = %+v, want still stopped", st2)
	}
}

func TestRestartSpawnsFreshProcess(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {Command: []string{"sh", "-c", "echo started; sleep 100"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, err := m.Start(ctx, "dev")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	second, err := m.Restart(ctx, "dev")
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if second.PID == first.PID {
		t.Errorf("Restart kept the same PID %d, want a fresh process", second.PID)
	}
	if second.State != StateReady {
		t.Errorf("Restart status = %+v, want ready", second)
	}
	m.Stop(ctx, "dev")
}

func TestLogsTail(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {Command: []string{"sh", "-c", "for i in $(seq 1 10); do echo line$i; done; sleep 100"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := m.Start(ctx, "dev"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Poll until the log file has all 10 lines (deadline-bound, real
	// out-of-process I/O — no in-process channel signals "the child has
	// flushed its writes").
	deadline := time.Now().Add(3 * time.Second)
	var content string
	for time.Now().Before(deadline) {
		c, _, err := m.Logs("dev", 100)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		content = c
		if strings.Count(content, "\n")+1 >= 10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	tail, st, err := m.Logs("dev", 3)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("tail lines = %v, want 3", lines)
	}
	if lines[2] != "line10" {
		t.Errorf("last tail line = %q, want line10 (full content: %q)", lines[2], content)
	}
	if st.Log == "" {
		t.Errorf("Logs status missing log path")
	}
	m.Stop(ctx, "dev")
}

func TestLogsBeforeStart(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})
	content, st, err := m.Logs("dev", 50)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if content != "" {
		t.Errorf("content = %q, want empty before any start", content)
	}
	if st.State != "" {
		t.Errorf("State = %q, want empty (never started)", st.State)
	}
	if st.Log == "" {
		t.Errorf("Log path should be populated even before start")
	}
}

func TestUnknownProcessName(t *testing.T) {
	m := NewManager(t.TempDir(), map[string]Def{})
	ctx := context.Background()
	if _, err := m.Start(ctx, "nope"); err == nil {
		t.Error("Start(unknown) = nil error, want error")
	}
	if _, err := m.Stop(ctx, "nope"); err == nil {
		t.Error("Stop(unknown) = nil error, want error")
	}
	if _, err := m.Status("nope"); err == nil {
		t.Error("Status(unknown) = nil error, want error")
	}
}

func TestEverStarted(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {Command: []string{"sh", "-c", "echo started; sleep 100"}},
	})
	if m.EverStarted() {
		t.Fatal("EverStarted = true before any Start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Start(ctx, "dev"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !m.EverStarted() {
		t.Error("EverStarted = false after Start")
	}
	m.Stop(ctx, "dev")
	if !m.EverStarted() {
		t.Error("EverStarted flipped back to false after Stop, want sticky true")
	}
}

func TestDeclareAndUndeclare(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, map[string]Def{
		"dev": {Command: []string{"sh", "-c", "true"}}, // config-origin
	})

	t.Run("declare validates like config parsing", func(t *testing.T) {
		if err := m.Declare("bad", Def{Command: nil}); err == nil {
			t.Fatal("Declare with empty command: want error")
		} else if !strings.Contains(err.Error(), "command is required") {
			t.Errorf("error = %q, want it to name the empty-argv problem", err)
		}
		if err := m.Declare("bad2", Def{Command: []string{"x"}, ReadyRegex: "("}); err == nil {
			t.Fatal("Declare with invalid ready_regex: want error")
		} else if !strings.Contains(err.Error(), "invalid ready_regex") {
			t.Errorf("error = %q, want it to name the invalid regex", err)
		}
	})

	t.Run("cannot redeclare config-origin name", func(t *testing.T) {
		if err := m.Declare("dev", Def{Command: []string{"x"}}); err == nil {
			t.Fatal("Declare over config-origin name: want error")
		}
	})

	t.Run("declare then list then undeclare", func(t *testing.T) {
		if err := m.Declare("adhoc", Def{Command: []string{"sh", "-c", "true"}}); err != nil {
			t.Fatalf("Declare: %v", err)
		}
		found := false
		for _, info := range m.List() {
			if info.Name == "adhoc" {
				found = true
				if info.Origin != OriginRuntime {
					t.Errorf("Origin = %q, want runtime", info.Origin)
				}
			}
			if info.Name == "dev" && info.Origin != OriginConfig {
				t.Errorf("dev Origin = %q, want config", info.Origin)
			}
		}
		if !found {
			t.Fatal("declared process 'adhoc' not in List()")
		}
		if err := m.Undeclare("adhoc"); err != nil {
			t.Fatalf("Undeclare: %v", err)
		}
		for _, info := range m.List() {
			if info.Name == "adhoc" {
				t.Fatal("undeclared process still in List()")
			}
		}
	})

	t.Run("cannot undeclare config-origin name", func(t *testing.T) {
		if err := m.Undeclare("dev"); err == nil {
			t.Fatal("Undeclare(config-origin): want error")
		}
	})

	t.Run("cannot undeclare or redeclare while running", func(t *testing.T) {
		if err := m.Declare("svc", Def{Command: []string{"sh", "-c", "echo up; sleep 100"}}); err != nil {
			t.Fatalf("Declare: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := m.Start(ctx, "svc"); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := m.Undeclare("svc"); err == nil {
			t.Fatal("Undeclare(running): want error")
		}
		if err := m.Declare("svc", Def{Command: []string{"x"}}); err == nil {
			t.Fatal("Declare(running, replace): want error")
		}
		m.Stop(ctx, "svc")
		if err := m.Undeclare("svc"); err != nil {
			t.Fatalf("Undeclare after stop: %v", err)
		}
	})

	t.Run("env names never expose values", func(t *testing.T) {
		if err := m.Declare("withenv", Def{Command: []string{"sh", "-c", "true"}, Env: []string{"SECRET=topsecret"}}); err != nil {
			t.Fatalf("Declare: %v", err)
		}
		for _, info := range m.List() {
			if info.Name == "withenv" {
				if len(info.EnvNames) != 1 || info.EnvNames[0] != "SECRET" {
					t.Fatalf("EnvNames = %v, want [SECRET]", info.EnvNames)
				}
			}
		}
	})
}
