package engine

import (
	"testing"
	"time"
)

// TestOnStorePhaseStartFiresBeforeCompletion verifies Config.OnStorePhaseStart
// fires once per instrumented phase, strictly before that phase's matching
// OnStorePhase completion call, with identical op/phase pairing on both
// sides — the pairing an in-flight watchdog (cmd/harness/main.go) relies on
// to insert-on-start and remove-on-completion without ever leaking or
// double-clearing an entry. Exercised across both instrumented paths: a
// fresh-file Persist (ensure_log) and EnqueuePromptDurable (enqueue_durable).
func TestOnStorePhaseStartFiresBeforeCompletion(t *testing.T) {
	dir := t.TempDir()
	type startCall struct{ op, phase string }
	type doneCall struct {
		op, phase string
		elapsed   time.Duration
	}
	var starts []startCall
	var dones []doneCall
	s := NewSession(Config{
		SessionDir: dir,
		OnStorePhaseStart: func(op, phase string) {
			starts = append(starts, startCall{op, phase})
		},
		OnStorePhase: func(op, phase string, elapsed time.Duration) {
			dones = append(dones, doneCall{op, phase, elapsed})
		},
	})

	if err := s.Persist(); err != nil {
		t.Fatal(err)
	}

	if len(starts) == 0 {
		t.Fatal("OnStorePhaseStart never fired during Persist")
	}
	if len(starts) != len(dones) {
		t.Fatalf("got %d starts but %d completions: starts=%+v dones=%+v", len(starts), len(dones), starts, dones)
	}
	// Each start must be immediately followed, at the same index, by its
	// matching completion — proving Start fires BEFORE completion for every
	// phase, not just "somewhere earlier in the slice", and that op/phase
	// pairing is exact.
	for i := range starts {
		if starts[i].op != dones[i].op || starts[i].phase != dones[i].phase {
			t.Errorf("call %d: start %+v does not pair with completion %+v", i, starts[i], dones[i])
		}
	}
	wantPhases := map[string]bool{"mkdir": false, "open": false, "stat": false, "header_write": false, "sync_dir": false}
	for _, c := range starts {
		if c.op != "ensure_log" {
			t.Fatalf("unexpected op %q during Persist", c.op)
		}
		if _, ok := wantPhases[c.phase]; !ok {
			t.Fatalf("unexpected ensure_log phase %q (or duplicate tail_repair on a fresh file)", c.phase)
		}
		wantPhases[c.phase] = true
	}
	for phase, seen := range wantPhases {
		if !seen {
			t.Errorf("ensure_log phase %q start not reported", phase)
		}
	}

	// A second Persist call is a no-op (logFile already open, fast path) and
	// must report nothing — same as OnStorePhase's own fast-path contract.
	starts, dones = nil, nil
	if err := s.Persist(); err != nil {
		t.Fatal(err)
	}
	if len(starts) != 0 || len(dones) != 0 {
		t.Fatalf("ensureLog fast path reported starts=%+v dones=%+v, want none", starts, dones)
	}

	starts, dones = nil, nil
	if _, dup, err := s.EnqueuePromptDurable("hello", 1); err != nil || dup {
		t.Fatalf("EnqueuePromptDurable: dup %v err %v", dup, err)
	}
	if len(starts) != len(dones) {
		t.Fatalf("got %d starts but %d completions: starts=%+v dones=%+v", len(starts), len(dones), starts, dones)
	}
	wantEnqueue := map[string]bool{"write_record": false, "fsync": false}
	for i, c := range starts {
		if c.op != "enqueue_durable" {
			t.Fatalf("unexpected op %q during EnqueuePromptDurable", c.op)
		}
		if _, ok := wantEnqueue[c.phase]; !ok {
			t.Fatalf("unexpected enqueue_durable phase %q", c.phase)
		}
		wantEnqueue[c.phase] = true
		if starts[i].op != dones[i].op || starts[i].phase != dones[i].phase {
			t.Errorf("call %d: start %+v does not pair with completion %+v", i, starts[i], dones[i])
		}
	}
	for phase, seen := range wantEnqueue {
		if !seen {
			t.Errorf("enqueue_durable phase %q start not reported", phase)
		}
	}
}

// TestOnStorePhaseStartNilSafe verifies a nil OnStorePhaseStart (the
// zero-value default, exercised by every other engine test in this package)
// never panics, mirroring OnStorePhase's own nil-safety.
func TestOnStorePhaseStartNilSafe(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	if err := s.Persist(); err != nil {
		t.Fatal(err)
	}
	if _, dup, err := s.EnqueuePromptDurable("hello", 1); err != nil || dup {
		t.Fatalf("EnqueuePromptDurable: dup %v err %v", dup, err)
	}
}
