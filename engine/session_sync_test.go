package engine

import (
	"testing"
	"time"
)

// phaseRecorder captures OnStorePhase calls as (op, phase) pairs — the
// minimal shape TestSessionSyncVolumeSkipsSyncPhases and its siblings need
// to assert which phases fired and which did not.
type phaseRecorder struct {
	calls []struct{ op, phase string }
}

func (r *phaseRecorder) record(op, phase string, _ time.Duration) {
	r.calls = append(r.calls, struct{ op, phase string }{op, phase})
}

func (r *phaseRecorder) has(op, phase string) bool {
	for _, c := range r.calls {
		if c.op == op && c.phase == phase {
			return true
		}
	}
	return false
}

// TestSessionSyncVolumeSkipsSyncDirPhase pins the "volume" mode gate on
// ensureLog's fresh-file directory fsync (see store.go's ensureLog): in
// volume mode, a fresh session log's header_write phase still fires (the
// write(2) itself is unchanged in both modes — see EnqueuePromptDurable's
// doc comment), but sync_dir must not — neither the phase event nor, since
// syncDir is where a wedged FUSE/9p mount deadlocks permanently, the
// underlying syscall.
func TestSessionSyncVolumeSkipsSyncDirPhase(t *testing.T) {
	var rec phaseRecorder
	s := NewSession(Config{
		SessionDir:   t.TempDir(),
		SessionSync:  "volume",
		OnStorePhase: rec.record,
	})
	if err := s.Persist(); err != nil {
		t.Fatal(err)
	}
	if !rec.has("ensure_log", "header_write") {
		t.Errorf("header_write phase not reported: %+v", rec.calls)
	}
	if rec.has("ensure_log", "sync_dir") {
		t.Errorf("sync_dir phase reported in volume mode: %+v", rec.calls)
	}
}

// TestSessionSyncVolumeSkipsFsyncPhase pins the "volume" mode gate on
// EnqueuePromptDurable's file fsync: write_record still fires (the write(2)
// is unchanged in both modes), but fsync must not.
func TestSessionSyncVolumeSkipsFsyncPhase(t *testing.T) {
	var rec phaseRecorder
	s := NewSession(Config{
		SessionDir:   t.TempDir(),
		SessionSync:  "volume",
		OnStorePhase: rec.record,
	})
	if _, dup, err := s.EnqueuePromptDurable("hello", 1); err != nil || dup {
		t.Fatalf("EnqueuePromptDurable: dup %v err %v", dup, err)
	}
	if !rec.has("enqueue_durable", "write_record") {
		t.Errorf("write_record phase not reported: %+v", rec.calls)
	}
	if rec.has("enqueue_durable", "fsync") {
		t.Errorf("fsync phase reported in volume mode: %+v", rec.calls)
	}
}

// TestSessionSyncVolumeReloadRoundTrip: volume mode skips both fsync
// round-trips, but the write(2) calls and torn-write healing/replay logic
// are unchanged — a durable enqueue in volume mode must still survive a
// LoadSession reload with its watermark and entry intact.
func TestSessionSyncVolumeReloadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir, SessionSync: "volume"})
	id, dup, err := s.EnqueuePromptDurable("hello", 1)
	if err != nil || dup {
		t.Fatalf("EnqueuePromptDurable: id %d dup %v err %v", id, dup, err)
	}
	re, err := LoadSession(Config{SessionDir: dir, SessionSync: "volume"}, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if re.EnqueueSeq() != 1 {
		t.Errorf("EnqueueSeq = %d, want 1", re.EnqueueSeq())
	}
	q := re.QueuedPrompts()
	if len(q) != 1 || q[0].ID != id || q[0].Text != "hello" || q[0].Seq != 1 {
		t.Errorf("queue = %+v, want one entry id %d text hello seq 1", q, id)
	}
}

// TestSessionSyncDefaultAndExplicitFsyncStillEmitBothSyncPhases is the
// regression guard: an empty SessionSync (the default) and an explicit
// "fsync" must both behave identically to pre-session_sync behavior — every
// sync_dir/fsync phase still fires. TestOnStorePhaseReportsCreateAndEnqueuePhases
// already pins the empty-value case; this covers the explicit "fsync" value
// too, since the two must be indistinguishable in behavior.
func TestSessionSyncDefaultAndExplicitFsyncStillEmitBothSyncPhases(t *testing.T) {
	for _, mode := range []string{"", "fsync"} {
		t.Run("mode="+mode, func(t *testing.T) {
			var rec phaseRecorder
			s := NewSession(Config{
				SessionDir:   t.TempDir(),
				SessionSync:  mode,
				OnStorePhase: rec.record,
			})
			if err := s.Persist(); err != nil {
				t.Fatal(err)
			}
			if !rec.has("ensure_log", "sync_dir") {
				t.Errorf("mode %q: sync_dir phase not reported: %+v", mode, rec.calls)
			}
			if _, dup, err := s.EnqueuePromptDurable("hello", 1); err != nil || dup {
				t.Fatalf("EnqueuePromptDurable: dup %v err %v", dup, err)
			}
			if !rec.has("enqueue_durable", "fsync") {
				t.Errorf("mode %q: fsync phase not reported: %+v", mode, rec.calls)
			}
		})
	}
}
