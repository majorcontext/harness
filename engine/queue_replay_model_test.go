package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"pgregory.net/rapid"
)

// refQueueState is the reference fold's result: the folded queue (in FIFO
// append order), the durable-enqueue watermark, and the next queue ID.
type refQueueState struct {
	queue     []QueuedPrompt
	watermark int64
	nextID    int64
}

// referenceFold is an INDEPENDENT, test-local re-implementation of the
// DOCUMENTED replay semantics for prompt.queued/prompt.dequeued records (see
// store.go's LoadSession recPromptQueued/recPromptDequeued cases): fold
// queued records (remove any existing same-Seq entry, append the new one at
// the tail, advance the watermark, advance nextID past every ID seen) and
// dequeued records (remove by ID) directly from the spec in this comment,
// deliberately NOT by calling or copying LoadSession's fold. It is the
// differential oracle TestQueueReplayModel checks the production fold
// against — the two can only ever agree by both independently matching the
// documented behavior, never by sharing a bug.
//
// It mirrors scanLog's corruption discipline exactly (store.go): split on
// '\n', drop trailing empty lines, and treat a trailing line that fails to
// parse as JSON as a write-in-progress torn by a crash (silently dropped)
// rather than corruption — the same tolerance LoadSession itself extends.
// Every write in this package is a single ordered append ending in exactly
// one '\n' (writeRecord), so ANY suffix truncation of the file leaves a
// valid prefix of complete lines plus at most one partial trailing line:
// folding only lines that parse is therefore EXACT for a torn file, not an
// approximation — see the torn-crash-reload action below, which relies on
// this.
//
// tb is rapid.TB (satisfied by both *testing.T and *rapid.T) so the same
// oracle serves a plain sanity call and every rapid action/Check call.
func referenceFold(tb rapid.TB, data []byte) refQueueState {
	st := refQueueState{nextID: 1}
	lines := bytes.Split(data, []byte("\n"))
	last := len(lines) - 1
	for last >= 0 && len(bytes.TrimSpace(lines[last])) == 0 {
		last--
	}
	for i := 0; i <= last; i++ {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			if i == last {
				break // truncated final line: crash mid-write, ignore
			}
			tb.Fatalf("referenceFold: unexpected corrupt non-final line %d: %v\ndata: %s", i+1, err, data)
		}
		switch rec.Type {
		case recPromptQueued:
			if rec.Prompt == nil {
				continue
			}
			q := QueuedPrompt{ID: rec.Prompt.ID, Text: rec.Prompt.Text, Seq: rec.Prompt.Seq}
			if q.Seq > 0 {
				for j, p := range st.queue {
					if p.Seq == q.Seq {
						st.queue = append(st.queue[:j], st.queue[j+1:]...)
						break
					}
				}
				if q.Seq > st.watermark {
					st.watermark = q.Seq
				}
			}
			st.queue = append(st.queue, q)
			if rec.Prompt.ID >= st.nextID {
				st.nextID = rec.Prompt.ID + 1
			}
		case recPromptDequeued:
			if rec.Prompt == nil {
				continue
			}
			for j, p := range st.queue {
				if p.ID == rec.Prompt.ID {
					st.queue = append(st.queue[:j], st.queue[j+1:]...)
					break
				}
			}
		}
	}
	return st
}

// nextQueueID reaches promptQueueNextID directly (same package) under s.mu,
// mirroring the fuzz test's approach.
func nextQueueID(s *Session) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promptQueueNextID
}

// compareQueueState asserts the live/reloaded session's queue state matches
// the reference fold exactly — contents, order, watermark, and nextID.
// tb.Fatalf is what makes a mismatch a rapid property failure, which rapid
// then shrinks to a minimal action sequence (see the failfile note on
// TestQueueReplayModel below).
func compareQueueState(tb rapid.TB, phase string, want refQueueState, gotWatermark int64, gotQueue []QueuedPrompt, gotNextID int64) {
	if gotWatermark != want.watermark {
		tb.Fatalf("%s: watermark = %d, want %d", phase, gotWatermark, want.watermark)
	}
	if gotNextID != want.nextID {
		tb.Fatalf("%s: nextID = %d, want %d", phase, gotNextID, want.nextID)
	}
	if len(gotQueue) != len(want.queue) {
		tb.Fatalf("%s: queue len = %d, want %d\n got:  %+v\n want: %+v",
			phase, len(gotQueue), len(want.queue), gotQueue, want.queue)
	}
	for i := range gotQueue {
		g, w := gotQueue[i], want.queue[i]
		if g.ID != w.ID || g.Seq != w.Seq || g.Text != w.Text {
			tb.Fatalf("%s: queue[%d] = %+v, want %+v\n got:  %+v\n want: %+v",
				phase, i, g, w, gotQueue, want.queue)
		}
	}
}

// readJournalOrEmpty reads the session's journal file, returning nil data
// (not an error) when nothing has been durably written yet — persistence in
// this package is lazy (see store.go's package doc comment), so an
// as-yet-untouched session legitimately has no file on disk.
func readJournalOrEmpty(tb rapid.TB, dir, id string) []byte {
	data, err := os.ReadFile(sessionPath(dir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		tb.Fatalf("read journal: %v", err)
	}
	return data
}

var dequeueReasons = []string{"delivered", "injected", "cleared"}

// queueModel is the rapid.StateMachine driving TestQueueReplayModel. It
// holds the tempdir and the CURRENT live *Session handle — crash-reload and
// torn-crash-reload actions REPLACE m.s with a freshly loaded handle, so
// every subsequent action and Check runs against the post-restart
// successor, exactly like a real process restart continuing on the same
// on-disk journal.
type queueModel struct {
	dir string
	id  string
	s   *Session
}

func newQueueModel(dir string) *queueModel {
	s := NewSession(Config{SessionDir: dir})
	return &queueModel{dir: dir, id: s.ID, s: s}
}

// DurableEnqueue exercises EnqueuePromptDurable across the seq space that
// matters for the watermark/dedupe contract: the next fresh seq, a seq that
// leaves a gap, an exact duplicate of the watermark, a stale (below
// watermark) seq, and an arbitrary already-seen seq — rapid chooses which
// via seqKind and, for the last case, the in-range value itself.
func (m *queueModel) DurableEnqueue(t *rapid.T) {
	wm := m.s.EnqueueSeq()
	var seq int64
	switch rapid.IntRange(0, 4).Draw(t, "seqKind") {
	case 0:
		seq = wm + 1
	case 1:
		seq = wm + 2 // gap
	case 2:
		seq = wm // duplicate (or 0 if wm==0, clamped below)
	case 3:
		if wm > 0 {
			seq = wm - 1 // stale
		} else {
			seq = wm + 1
		}
	case 4:
		if wm > 0 {
			seq = rapid.Int64Range(1, wm).Draw(t, "seqInRange")
		} else {
			seq = wm + 1
		}
	}
	if seq < 1 {
		seq = 1
	}
	text := fmt.Sprintf("durable-%d", rapid.IntRange(0, 1<<20).Draw(t, "textSeed"))
	if _, _, err := m.s.EnqueuePromptDurable(text, seq); err != nil {
		t.Fatalf("EnqueuePromptDurable(seq=%d): %v", seq, err)
	}
}

// PlainEnqueue exercises the non-durable EnqueuePrompt path (no seq, no
// watermark movement), which shares the same folded queue and ID space.
func (m *queueModel) PlainEnqueue(t *rapid.T) {
	text := fmt.Sprintf("plain-%d", rapid.IntRange(0, 1<<20).Draw(t, "textSeed"))
	if _, err := m.s.EnqueuePrompt(text); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}
}

// DequeueOne pops the FIFO head, or is a clean no-op on an empty queue.
func (m *queueModel) DequeueOne(t *rapid.T) {
	reason := rapid.SampledFrom(dequeueReasons).Draw(t, "reason")
	m.s.DequeuePrompt(reason)
}

// DequeueAll drains the whole queue, or is a clean no-op when already empty.
func (m *queueModel) DequeueAll(t *rapid.T) {
	reason := rapid.SampledFrom(dequeueReasons).Draw(t, "reason")
	m.s.DequeueAllPrompts(reason)
}

// CrashReload simulates a clean process restart: reload from the journal as
// it stands (nothing truncated) and continue the action sequence on the
// reloaded handle. A no-op (via t.Skip, following the precondition-skip
// convention rapid's own state-machine example uses) when nothing has been
// durably written yet — LoadSession has no file to read.
func (m *queueModel) CrashReload(t *rapid.T) {
	if _, err := os.Stat(sessionPath(m.dir, m.id)); os.IsNotExist(err) {
		t.Skip("no journal yet")
	}
	reloaded, err := LoadSession(Config{SessionDir: m.dir}, m.id)
	if err != nil {
		t.Fatalf("crash-reload: LoadSession: %v", err)
	}
	m.s = reloaded
}

// TornCrashReload simulates a crash mid-write: truncate the journal to a
// rapid-chosen size anywhere in [0, current size], then reload and continue
// on the reloaded handle. Every engine write is a single ordered append
// (writeRecord), so ANY suffix truncation leaves a valid prefix of complete
// lines plus at most one partial trailing line — LoadSession's scanLog
// tolerates that unconditionally (see store.go). An error here is therefore
// always an invariant violation, never an expected outcome — suffix cuts
// must always be tolerated.
func (m *queueModel) TornCrashReload(t *rapid.T) {
	path := sessionPath(m.dir, m.id)
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		t.Skip("no journal yet")
	}
	if err != nil {
		t.Fatalf("torn-crash-reload: stat: %v", err)
	}
	size := fi.Size()
	truncTo := rapid.Int64Range(0, size).Draw(t, "truncTo")
	if err := os.Truncate(path, truncTo); err != nil {
		t.Fatalf("torn-crash-reload: truncate to %d/%d: %v", truncTo, size, err)
	}
	reloaded, err := LoadSession(Config{SessionDir: m.dir}, m.id)
	if err != nil {
		t.Fatalf("torn-crash-reload: LoadSession errored on a torn file (truncated to %d/%d bytes) — suffix truncation must always be tolerated: %v",
			truncTo, size, err)
	}
	m.s = reloaded
}

// Check is rapid's StateMachine invariant hook: it runs once before the
// first action and once after every subsequent action (see
// pgregory.net/rapid's Repeat/StateMachineActions). It reads the on-disk
// journal exactly as it stands right now — already truncated, mid
// TornCrashReload — folds it through the independent referenceFold oracle,
// and compares against the live session's QueueState()/nextID. This
// live-vs-replay equality is the invariant the historical FIFO replay bug
// violated: contents, ORDER, seqs, and watermark must match exactly, on
// every single action, not just at the end.
func (m *queueModel) Check(t *rapid.T) {
	data := readJournalOrEmpty(t, m.dir, m.id)
	want := referenceFold(t, data)
	gotWatermark, gotQueue := m.s.QueueState()
	compareQueueState(t, "live-vs-replay", want, gotWatermark, gotQueue, nextQueueID(m.s))
}

// TestQueueReplayModel is a rapid state-machine (property-based, shrinking)
// differential test: for each rapid.Check iteration it drives a fresh
// session through a random sequence of durable/plain enqueues, dequeues, and
// simulated crash-restarts (clean and torn), checking after every single
// action that the live session's folded queue state exactly matches
// referenceFold's independent replay of the on-disk journal.
//
// Default rapid tuning (100 checks x ~30 actions/check, see pgregory.net/
// rapid's flags.go) keeps this comfortably under a few seconds, since the
// only I/O per action is a handful of small appends/fsyncs to a tempdir
// file; if a future change makes it noticeably slower, re-tune with
// -rapid.checks=N (fewer checks) rather than weakening what each check does.
//
// On failure, rapid shrinks to a minimal reproducing action sequence and
// prints it; -rapid.failfile=<path> (or the auto-written failfile rapid
// reports on failure) replays that exact sequence for debugging — that is
// this test's reproduction handle, standing in for the seed+op-index
// handles a hand-rolled loop would need instead.
func TestQueueReplayModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dir, err := os.MkdirTemp("", "queue-replay-model-*")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })

		m := newQueueModel(dir)
		t.Repeat(rapid.StateMachineActions(m))
	})
}
