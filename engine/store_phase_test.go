package engine

import (
	"testing"
	"time"
)

// TestOnStorePhaseReportsCreateAndEnqueuePhases verifies Config.OnStorePhase
// fires for every phase of a fresh-file Persist (ensure_log: mkdir, open,
// stat, header_write, sync_dir — no tail_repair on a brand-new file) and of
// EnqueuePromptDurable (enqueue_durable: write_record, fsync), with every
// reported elapsed >= 0. The nil-callback path is exercised by every other
// engine test in this package (none of them set OnStorePhase), so it isn't
// re-tested here.
func TestOnStorePhaseReportsCreateAndEnqueuePhases(t *testing.T) {
	dir := t.TempDir()
	type call struct {
		op, phase string
		elapsed   time.Duration
	}
	var calls []call
	s := NewSession(Config{
		SessionDir: dir,
		OnStorePhase: func(op, phase string, elapsed time.Duration) {
			calls = append(calls, call{op, phase, elapsed})
		},
	})

	if err := s.Persist(); err != nil {
		t.Fatal(err)
	}

	wantEnsureLog := map[string]bool{"mkdir": false, "open": false, "stat": false, "header_write": false, "sync_dir": false}
	for _, c := range calls {
		if c.op != "ensure_log" {
			t.Fatalf("unexpected op %q during Persist", c.op)
		}
		if _, ok := wantEnsureLog[c.phase]; !ok {
			t.Fatalf("unexpected ensure_log phase %q (or duplicate tail_repair on a fresh file)", c.phase)
		}
		wantEnsureLog[c.phase] = true
		if c.elapsed < 0 {
			t.Fatalf("phase %q elapsed = %v, want >= 0", c.phase, c.elapsed)
		}
	}
	for phase, seen := range wantEnsureLog {
		if !seen {
			t.Errorf("ensure_log phase %q not reported", phase)
		}
	}

	// A second Persist call is a no-op (logFile already open) and must
	// report nothing — the fast path.
	calls = nil
	if err := s.Persist(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("ensureLog fast path reported %d phases, want 0: %+v", len(calls), calls)
	}

	calls = nil
	if _, dup, err := s.EnqueuePromptDurable("hello", 1); err != nil || dup {
		t.Fatalf("EnqueuePromptDurable: dup %v err %v", dup, err)
	}
	wantEnqueue := map[string]bool{"write_record": false, "fsync": false}
	for _, c := range calls {
		if c.op != "enqueue_durable" {
			t.Fatalf("unexpected op %q during EnqueuePromptDurable", c.op)
		}
		if _, ok := wantEnqueue[c.phase]; !ok {
			t.Fatalf("unexpected enqueue_durable phase %q", c.phase)
		}
		wantEnqueue[c.phase] = true
		if c.elapsed < 0 {
			t.Fatalf("phase %q elapsed = %v, want >= 0", c.phase, c.elapsed)
		}
	}
	for phase, seen := range wantEnqueue {
		if !seen {
			t.Errorf("enqueue_durable phase %q not reported", phase)
		}
	}
}
