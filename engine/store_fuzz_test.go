package engine

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoadSessionReplay is the machine-checked counterpart to the
// hand-authored replay-fold tests in queue_durable_test.go. LoadSession's
// recPromptQueued/recPromptDequeued fold (store.go) has twice been the site
// of a real bug caught only by human review (see this feature's git
// history), so this throws arbitrary bytes at it and checks structural
// invariants that must hold for ANY journal LoadSession is willing to
// accept — not just the hand-picked shapes the other tests pin.
func FuzzLoadSessionReplay(f *testing.F) {
	// Seed corpus: shapes chosen to exercise the fold's documented corner
	// cases (see store.go's recPromptQueued case and scanLog's corruption
	// discipline) — each is a plausible or edge-of-plausible on-disk
	// journal, not a crafted attack input; the fuzzer mutates from here.

	// A valid full journal: header, model, a plain queued entry, two seq'd
	// queued entries, a same-seq pair that must fold to one, a dequeue by
	// id, a goal record, and a truncated final line (crash mid-write —
	// scanLog must tolerate it silently rather than erroring).
	f.Add([]byte(`{"type":"session","id":"ses_0000000000000001","created_at":"2026-07-21T00:00:00Z"}
{"type":"model","model":"test/m1"}
{"type":"prompt.queued","prompt":{"id":1,"text":"plain"}}
{"type":"prompt.queued","prompt":{"id":2,"text":"first","seq":3}}
{"type":"prompt.queued","prompt":{"id":3,"text":"second","seq":7}}
{"type":"prompt.queued","prompt":{"id":4,"text":"dup-a","seq":9}}
{"type":"prompt.queued","prompt":{"id":5,"text":"dup-b","seq":9}}
{"type":"prompt.dequeued","prompt":{"id":2,"text":"first","reason":"delivered"}}
{"type":"goal.set","goal":{"condition":"done"}}
{"type":"prompt.queued","prompt":{"id":6,"tex`))

	// Empty file.
	f.Add([]byte(``))

	// Only a partial header line — no trailing newline, truncated mid-write
	// before anything else was ever appended.
	f.Add([]byte(`{"type":"session","id":"ses_0000000000`))

	// Seq records revisited out of order: seq 10, then a lower seq 3, then
	// seq 10 again — must fold to one entry at the tail, watermark stays 10.
	f.Add([]byte(`{"type":"session","id":"ses_0000000000000001","created_at":"2026-07-21T00:00:00Z"}
{"type":"model","model":"test/m1"}
{"type":"prompt.queued","prompt":{"id":1,"text":"a","seq":10}}
{"type":"prompt.queued","prompt":{"id":2,"text":"b","seq":3}}
{"type":"prompt.queued","prompt":{"id":3,"text":"c","seq":10}}
`))

	// A large seq near MaxInt64, so the fuzzer explores overflow-adjacent
	// territory in the watermark and this test's own +1 probe.
	f.Add([]byte(`{"type":"session","id":"ses_0000000000000001","created_at":"2026-07-21T00:00:00Z"}
{"type":"model","model":"test/m1"}
{"type":"prompt.queued","prompt":{"id":1,"text":"big","seq":9223372036854775806}}
`))

	const sessionID = "ses_0000000000000001"

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			t.Skip()
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := LoadSession(Config{SessionDir: dir}, sessionID)
		if err != nil {
			return // rejection is fine
		}

		watermark, prompts := s.QueueState()

		// Invariants 1-3: no two queued entries share a Seq>0 or an ID, and
		// the watermark is at least every queued entry's Seq.
		seenSeq := make(map[int64]bool)
		seenID := make(map[int64]bool)
		for _, p := range prompts {
			if p.Seq > 0 {
				if seenSeq[p.Seq] {
					t.Fatalf("duplicate Seq %d among queued entries: %+v", p.Seq, prompts)
				}
				seenSeq[p.Seq] = true
				if p.Seq > watermark {
					t.Fatalf("queued entry Seq %d exceeds watermark %d: %+v", p.Seq, watermark, prompts)
				}
			}
			if seenID[p.ID] {
				t.Fatalf("duplicate ID %d among queued entries: %+v", p.ID, prompts)
			}
			seenID[p.ID] = true
		}

		// Invariant 4: every queued ID is below the session's next queue ID.
		s.mu.Lock()
		nextID := s.promptQueueNextID
		s.mu.Unlock()
		for _, p := range prompts {
			if p.ID >= nextID {
				t.Fatalf("queued ID %d >= promptQueueNextID %d: %+v", p.ID, nextID, prompts)
			}
		}

		// Invariant 5: the loaded session is USABLE, not just well-formed —
		// a follow-up durable enqueue AT the watermark is a clean duplicate,
		// and one past it succeeds with a fresh, unique ID. Skipped when
		// watermark is 0 (EnqueuePromptDurable rejects seq<1 outright, which
		// isn't the duplicate path this invariant is about) or when the
		// probe would overflow int64.
		if watermark > 0 {
			if _, dup, err := s.EnqueuePromptDurable("probe-dup", watermark); err != nil || !dup {
				t.Fatalf("EnqueuePromptDurable at watermark %d: dup=%v err=%v, want dup=true err=nil", watermark, dup, err)
			}
		}
		if watermark < math.MaxInt64-1 {
			id, dup, err := s.EnqueuePromptDurable("probe-fresh", watermark+1)
			if err != nil || dup {
				t.Fatalf("EnqueuePromptDurable at watermark+1 (%d): id=%d dup=%v err=%v, want dup=false err=nil", watermark+1, id, dup, err)
			}
			if seenID[id] {
				t.Fatalf("EnqueuePromptDurable minted a colliding ID %d (already used by a replayed queued entry)", id)
			}
		}
	})
}
