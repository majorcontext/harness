package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// promptRecordOrder reads a session log and returns one "type:id" string
// per prompt-queue record, in the order the records appear on disk.
func promptRecordOrder(t *testing.T, dir, sessionID string) []string {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading session log %s: %v", path, err)
	}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Type   string `json:"type"`
			Prompt *struct {
				ID int64 `json:"id"`
			} `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshaling session log line %q: %v", line, err)
		}
		if rec.Type != recPromptQueued && rec.Type != recPromptDequeued {
			continue
		}
		if rec.Prompt == nil {
			t.Fatalf("prompt-queue record with no prompt payload: %q", line)
		}
		order = append(order, rec.Type)
	}
	return order
}

// TestSendToDescendantEmitsQueueEventOutsideTreeLock is the regression
// test for a review note that is the same defect class as the disk-write
// finding this park/flush mechanism was built for: SendToDescendant's
// running-target branch emitted EventPromptQueued INLINE, under the
// tree-wide m.mu. A subscriber does real work on that call — the server's
// own Publish journals the event to events.jsonl, a synchronous disk write
// under its own server.mu — so the emit put a disk write back inside the
// lock every other session's Info/Reap/Spawn/finalize call also needs.
//
// The event is now parked with its record and emitted by
// flushQueueRecordsLocked, after m.mu is released. Proven directly, the
// same way TestUnlockAndFlushPersistRunsThunksAfterReleasingLock proves
// the write ordering: a subscriber that cannot take m.mu was still inside
// the critical section.
func TestSendToDescendantEmitsQueueEventOutsideTreeLock(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: release}
	mgr := NewSessionManager(context.Background(), 0, 0)
	cfg := managedConfig("root", scriptedTurns("root", nil), childProv)

	var (
		mu       sync.Mutex
		queued   int
		heldMu   int // emits observed while m.mu was still held
		mgrForCB *SessionManager
	)
	mgrForCB = mgr
	cfg.OnEvent = func(ev Event) {
		if ev.Type != EventPromptQueued {
			return
		}
		locked := mgrForCB.mu.TryLock()
		if locked {
			mgrForCB.mu.Unlock()
		}
		mu.Lock()
		queued++
		if !locked {
			heldMu++
		}
		mu.Unlock()
	}
	root := mgr.NewRoot(cfg)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started

	if ok, err := mgr.SendToDescendant(root.ID, childID, "message A"); err != nil || !ok {
		t.Fatalf("SendToDescendant: queued=%v err=%v", ok, err)
	}

	mu.Lock()
	gotQueued, gotHeld := queued, heldMu
	mu.Unlock()
	if gotQueued != 1 {
		t.Fatalf("EventPromptQueued emits = %d, want exactly 1 by the time SendToDescendant returns", gotQueued)
	}
	if gotHeld != 0 {
		t.Errorf("%d of %d EventPromptQueued emit(s) ran while m.mu was held: a subscriber's own work (the server journals this event to disk) is back inside the tree-wide lock", gotHeld, gotQueued)
	}
}

// TestEnqueuePromptDurableDrainsParkedRecordsFirst is the regression test
// for a review finding on the parked-record design: every prompt-queue
// writer must drain the park before its own write, and
// EnqueuePromptDurable was the one writer that did not — it calls
// writeRecord directly, for its write-ahead durability and fsync
// contract.
//
// With a prompt.queued record for item A still parked (SendToDescendant's
// deferred flush not run yet), a durable enqueue of item B wrote
// queued(B) first. Memory order stayed [A, B] but disk order became
// [queued(B), queued(A)], and LoadSession's fold appends in record order
// and never sorts by ID, so a reload restored [B, A]: a FIFO reorder
// across a restart. Nothing is lost or delivered twice here — the durable
// path writes no dequeued record — but a queue whose whole contract is
// FIFO must not reorder itself on resume.
func TestEnqueuePromptDurableDrainsParkedRecordsFirst(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})

	// SendToDescendant's running-target branch, memory half: item A is in
	// the queue, its durable record parked, its flush not run yet.
	s.mu.Lock()
	a := s.enqueueMemoryOnlyLocked("message A", "")
	s.queueRecordDeferredLocked(recPromptQueued, promptRecord{ID: a.ID, Text: a.Text},
		Event{Type: EventPromptQueued, QueueID: a.ID, QueueText: a.Text, QueueLen: 1})
	s.mu.Unlock()

	if _, dup, err := s.EnqueuePromptDurable("message B", 1); err != nil || dup {
		t.Fatalf("EnqueuePromptDurable = (dup %v, err %v), want a fresh accept", dup, err)
	}

	s.mu.Lock()
	s.flushQueueRecordsLocked()
	s.mu.Unlock()

	re, err := LoadSession(Config{SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	pending := re.QueuedPrompts()
	if len(pending) != 2 || pending[0].Text != "message A" || pending[1].Text != "message B" {
		t.Errorf("QueuedPrompts after reload = %+v, want [message A, message B]: the durable enqueue wrote its own record ahead of a parked one, so the queue reordered across a restart", pending)
	}
}

// TestDeferredQueuedRecordStillPrecedesItsDequeuedRecord is the
// regression test for a durability defect the enqueue persist-split
// introduced: SendToDescendant's running-target branch appends to the
// queue in memory under m.mu and DEFERS the prompt.queued disk write past
// the m.mu release, while the child's own turn goroutine — which needs
// only s.mu — can drain that same item at a tool-call boundary and write
// its prompt.dequeued record SYNCHRONOUSLY. The dequeued record could
// therefore land on disk first.
//
// LoadSession's fold removes a dequeued item by ID and no-ops when the
// queued record has not folded yet, so the later queued record re-appends
// the item: a reload resurrects an already-delivered prompt and the child
// runs it a second time, breaking the queue's no-double-delivery
// invariant.
//
// The fix keeps the write off m.mu and keeps the order: a deferred
// prompt-queue record is parked on the SESSION (queueRecordDeferredLocked,
// under the same s.mu hold as the memory mutation), and every later
// prompt-queue write drains that FIFO before writing its own record. The
// competing dequeuer therefore writes the parked queued record first.
//
// The test drives the two production halves in the exact order the race
// produces — memory enqueue plus parked record, then a full synchronous
// DequeuePrompt, then the deferred flush — and asserts both the on-disk
// order and the reload outcome.
func TestDeferredQueuedRecordStillPrecedesItsDequeuedRecord(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})

	// SendToDescendant's running-target branch, memory half: append and
	// park the durable record under ONE s.mu hold, no disk write.
	s.mu.Lock()
	p := s.enqueueMemoryOnlyLocked("message A", "")
	s.queueRecordDeferredLocked(recPromptQueued, promptRecord{ID: p.ID, Text: p.Text},
		Event{Type: EventPromptQueued, QueueID: p.ID, QueueText: p.Text, QueueLen: 1})
	s.mu.Unlock()

	// The child's own turn goroutine drains the item before the deferred
	// flush runs. DequeuePrompt persists synchronously.
	got, _, ok := s.DequeuePrompt("injected")
	if !ok || got.ID != p.ID {
		t.Fatalf("DequeuePrompt = (%+v, %v), want the just-enqueued item %+v", got, ok, p)
	}

	// SendToDescendant's deferred half, run after m.mu released.
	s.mu.Lock()
	s.flushQueueRecordsLocked()
	s.mu.Unlock()

	order := promptRecordOrder(t, dir, s.ID)
	want := []string{recPromptQueued, recPromptDequeued}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("prompt-queue record order on disk = %v, want %v: a dequeued record before its own queued record makes the replay fold resurrect the item", order, want)
	}

	re, err := LoadSession(Config{SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if pending := re.QueuedPrompts(); len(pending) != 0 {
		t.Errorf("QueuedPrompts after reload = %+v, want empty: the delivered prompt was resurrected and will be delivered a second time", pending)
	}
}
