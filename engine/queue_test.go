package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func TestEnqueuePromptPersistsAndEmits(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	id, err := s.EnqueuePrompt("do the thing")
	if err != nil {
		t.Fatalf("EnqueuePrompt = %v", err)
	}
	if id != 1 {
		t.Errorf("id = %d, want 1 (first monotonic ID)", id)
	}

	pending := s.QueuedPrompts()
	if len(pending) != 1 || pending[0].ID != id || pending[0].Text != "do the thing" {
		t.Fatalf("QueuedPrompts = %+v", pending)
	}

	var sawEvent bool
	for _, ev := range evs {
		if ev.Type == EventPromptQueued {
			sawEvent = true
			if ev.QueueID != id {
				t.Errorf("EventPromptQueued.QueueID = %d, want %d", ev.QueueID, id)
			}
			if ev.QueueText != "do the thing" {
				t.Errorf("EventPromptQueued.QueueText = %q", ev.QueueText)
			}
			if ev.QueueLen != 1 {
				t.Errorf("EventPromptQueued.QueueLen = %d, want 1", ev.QueueLen)
			}
		}
	}
	if !sawEvent {
		t.Error("EventPromptQueued was not emitted")
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"prompt.queued"`) || !strings.Contains(log, `"text":"do the thing"`) {
		t.Fatalf("log missing prompt.queued record with text: %s", log)
	}
}

func TestEnqueueRejectsEmpty(t *testing.T) {
	s := NewSession(Config{})
	if _, err := s.EnqueuePrompt("   \n\t "); err == nil {
		t.Fatal("EnqueuePrompt with whitespace-only text should error")
	}
	if len(s.QueuedPrompts()) != 0 {
		t.Errorf("QueuedPrompts = %+v, want empty after a rejected enqueue", s.QueuedPrompts())
	}
}

func TestDequeueFIFOAndJournalsReason(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	id1, err := s.EnqueuePrompt("first")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.EnqueuePrompt("second")
	if err != nil {
		t.Fatal(err)
	}

	p1, ok := s.DequeuePrompt("delivered")
	if !ok || p1.ID != id1 || p1.Text != "first" {
		t.Fatalf("first DequeuePrompt = %+v, %v", p1, ok)
	}
	if pending := s.QueuedPrompts(); len(pending) != 1 || pending[0].ID != id2 {
		t.Fatalf("QueuedPrompts after one dequeue = %+v, want only id %d left", pending, id2)
	}

	p2, ok := s.DequeuePrompt("injected")
	if !ok || p2.ID != id2 || p2.Text != "second" {
		t.Fatalf("second DequeuePrompt = %+v, %v", p2, ok)
	}
	if pending := s.QueuedPrompts(); len(pending) != 0 {
		t.Fatalf("QueuedPrompts after draining = %+v, want empty", pending)
	}

	// Dequeuing an empty queue is a clean no-op: no record, no event.
	if _, ok := s.DequeuePrompt("delivered"); ok {
		t.Fatal("DequeuePrompt on an empty queue should report ok=false")
	}

	var reasons []string
	for _, ev := range evs {
		if ev.Type == EventPromptDequeued {
			reasons = append(reasons, ev.QueueReason)
		}
	}
	if want := []string{"delivered", "injected"}; len(reasons) != len(want) || reasons[0] != want[0] || reasons[1] != want[1] {
		t.Fatalf("EventPromptDequeued reasons = %v, want %v", reasons, want)
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"prompt.dequeued","prompt":{"id":1,"text":"first","reason":"delivered"}`) {
		t.Fatalf("log missing first prompt.dequeued record: %s", log)
	}
	if !strings.Contains(log, `"type":"prompt.dequeued","prompt":{"id":2,"text":"second","reason":"injected"}`) {
		t.Fatalf("log missing second prompt.dequeued record: %s", log)
	}
	// The no-op dequeue on an empty queue must not have journaled a third
	// prompt.dequeued record.
	if n := strings.Count(log, `"type":"prompt.dequeued"`); n != 2 {
		t.Fatalf("prompt.dequeued record count = %d, want 2 (no record for the empty-queue no-op)", n)
	}
}

// TestQueuedPromptsAbsentFromHistory is invariant 2: a queued prompt must
// never enter s.history, and must never appear in the provider request of a
// subsequent, unrelated Prompt call. Task 1 wires no injection path at all
// (that is Task 2/3's job), so this also guards against any accidental
// coupling introduced by EnqueuePrompt/DequeuePrompt themselves.
func TestQueuedPromptsAbsentFromHistory(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		System:    []string{"base"},
	})

	if _, err := s.EnqueuePrompt("queued text should never appear"); err != nil {
		t.Fatal(err)
	}
	if h := s.History(); len(h) != 0 {
		t.Fatalf("History() after enqueue = %+v, want empty", h)
	}

	if _, err := s.Prompt(context.Background(), "explicit prompt"); err != nil {
		t.Fatal(err)
	}

	h := s.History()
	if len(h) != 2 {
		t.Fatalf("History() after Prompt = %d messages, want 2 (user + assistant)", len(h))
	}
	if h[0].Parts.Text() != "explicit prompt" {
		t.Errorf("history[0] text = %q, want the explicit prompt only", h[0].Parts.Text())
	}
	for _, m := range h {
		if strings.Contains(m.Parts.Text(), "queued text") {
			t.Errorf("queued text leaked into history: %+v", m)
		}
	}

	if len(prov.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(prov.requests))
	}
	req := prov.requests[0]
	if len(req.Messages) != 1 || req.Messages[0].Parts.Text() != "explicit prompt" {
		t.Fatalf("provider request messages = %+v, want exactly the explicit prompt", req.Messages)
	}
	for _, m := range req.Messages {
		if strings.Contains(m.Parts.Text(), "queued text") {
			t.Errorf("queued text leaked into the provider request: %+v", m)
		}
	}

	// The enqueued prompt is untouched — Task 1 wires no drain/injection.
	if pending := s.QueuedPrompts(); len(pending) != 1 {
		t.Fatalf("QueuedPrompts after an unrelated Prompt call = %+v, want still 1 pending", pending)
	}
}

// TestLoadSessionRefoldsQueue is invariant 3: a mix of prompt.queued and
// prompt.dequeued records must refold to exactly the undelivered set, in ID
// order, and the next-ID counter must continue monotonically rather than
// colliding with an already-used ID.
func TestLoadSessionRefoldsQueue(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})

	if _, err := s.EnqueuePrompt("a"); err != nil {
		t.Fatal(err)
	}
	id2, err := s.EnqueuePrompt("b")
	if err != nil {
		t.Fatal(err)
	}
	id3, err := s.EnqueuePrompt("c")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.DequeuePrompt("delivered"); !ok {
		t.Fatal("expected a prompt to dequeue")
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	loaded, err := LoadSession(Config{SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatal(err)
	}

	pending := loaded.QueuedPrompts()
	if len(pending) != 2 {
		t.Fatalf("loaded QueuedPrompts = %+v, want exactly 2 (b, c)", pending)
	}
	if pending[0].ID != id2 || pending[0].Text != "b" {
		t.Errorf("pending[0] = %+v, want {%d, b}", pending[0], id2)
	}
	if pending[1].ID != id3 || pending[1].Text != "c" {
		t.Errorf("pending[1] = %+v, want {%d, c}", pending[1], id3)
	}

	// The next-ID counter must continue past the highest folded ID, never
	// reissuing an ID already used on disk.
	id4, err := loaded.EnqueuePrompt("d")
	if err != nil {
		t.Fatal(err)
	}
	if id4 != id3+1 {
		t.Errorf("next ID after reload = %d, want %d (continuing monotonically)", id4, id3+1)
	}
}
