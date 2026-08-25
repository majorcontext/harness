package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	p := s.enqueueMemoryOnlyLocked("message A")
	s.queueRecordDeferredLocked(recPromptQueued, promptRecord{ID: p.ID, Text: p.Text})
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
