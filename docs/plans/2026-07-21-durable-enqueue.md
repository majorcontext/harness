# Durable Idempotent Enqueue Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** A `POST /session/{id}/enqueue` primitive whose 2xx is an honest durability attestation: the prompt is fsynced into the session journal, deduplicated by a caller-issued monotonic `seq`, before any success response is written — so an external delivery loop (coordinator inbox poller, sidecar, anything) can ack its upstream on 2xx without ever ghost-delivering or double-delivering.

**Architecture:** Builds directly on the #80 prompt queue (`engine/queue.go`, `docs/plans/2026-07-19-prompt-queue.md`). A new engine method `EnqueuePromptDurable(text, seq)` extends `EnqueuePrompt` with three properties the plain path deliberately lacks: (1) **write-ahead durability** — the `prompt.queued` record is written *and fsynced* before any in-memory mutation, and persistence failure is a returned error, never a swallowed `lastPersistErr`; (2) **idempotency** — a per-session high-water-mark `enqueueSeq` (journaled on the record, rebuilt on replay) makes `seq <= watermark` a clean duplicate no-op, so upstream retries are always safe; (3) **torn-write healing** — queue IDs are burned on failed writes (never reused, so they can't collide with a later plain enqueue), and `LoadSession` folds same-`seq` records last-writer-wins, so a record that tore during a failed fsync converges with its successful retry to exactly one queue entry. The HTTP handler mirrors `handlePrompt`'s claim/enqueue/dispatch shapes exactly (idle: claim → durable enqueue → dispatch queue head; busy: durable enqueue → one claim retry), so delivery into a turn inherits #80's FIFO, tool-boundary injection, and goal-boundary machinery unchanged.

**Why the watermark lives in the session journal (not caller-side):** if the acked-seq state lived upstream, a crash after upstream-ack but before the journal flush loses the message permanently. Co-located in the same journal, tail loss destroys the message *and* the watermark atomically — redelivery occurs and dedupe correctly doesn't suppress it. Fate-sharing is the design.

**Tech Stack:** Go stdlib only (`net/http` mux patterns, `encoding/json`, `os.File.Sync`). No new dependencies.

**Out of scope (explicit):** no inbox poll loop, no coordinator contract, no config additions, no changes to `prompt_async` semantics. This is the passive engine primitive only.

---

## Conventions for every task

- Run tests from repo root: `go test ./engine/ ./server/` (fast); full sweep `go test ./...` before each commit.
- House style: doc comments explain *why* and cross-reference designs (see `engine/queue.go` for tone). Persist-and-emit under `s.mu`, mirroring `EnqueuePrompt`.
- Commit after every task, message style: `feat(engine): ...` / `feat(server): ...` / `docs: ...`.

---

### Task 1: Engine record + replay — `seq` on queue records, watermark restore, last-writer-wins fold

**Files:**
- Modify: `engine/store.go` (promptRecord, LoadSession's `recPromptQueued` case)
- Modify: `engine/queue.go` (QueuedPrompt struct)
- Modify: `engine/engine.go` (Session struct: add `enqueueSeq int64` next to `promptQueue`/`promptQueueNextID`; find with `grep -n promptQueueNextID engine/engine.go`)
- Test: `engine/queue_durable_test.go` (new)

**Step 1: Write the failing tests**

```go
package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSessionLog seeds a session log file from raw JSONL lines, the same
// hand-authored-log technique store tests use for replay-shape cases.
func writeSessionLog(t *testing.T, dir, id string, lines ...string) {
	t.Helper()
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadSessionRestoresEnqueueWatermark: prompt.queued records carrying seq
// rebuild the high-water mark; plain (seq-less) records leave it untouched.
func TestLoadSessionRestoresEnqueueWatermark(t *testing.T) {
	dir := t.TempDir()
	writeSessionLog(t, dir, "sess_wm",
		`{"type":"session","id":"sess_wm","created_at":"2026-07-21T00:00:00Z"}`,
		`{"type":"model","model":{"provider":"test","model":"m1"}}`,
		`{"type":"prompt.queued","prompt":{"id":1,"text":"plain","reason":""}}`,
		`{"type":"prompt.queued","prompt":{"id":2,"text":"first durable","seq":3}}`,
		`{"type":"prompt.queued","prompt":{"id":3,"text":"second durable","seq":7}}`,
	)
	s, err := LoadSession(Config{SessionDir: dir}, "sess_wm")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.EnqueueSeq(); got != 7 {
		t.Fatalf("EnqueueSeq = %d, want 7", got)
	}
	q := s.QueuedPrompts()
	if len(q) != 3 {
		t.Fatalf("queue len = %d, want 3", len(q))
	}
	if q[1].Seq != 3 || q[2].Seq != 7 || q[0].Seq != 0 {
		t.Fatalf("folded seqs = %d,%d,%d, want 0,3,7", q[0].Seq, q[1].Seq, q[2].Seq)
	}
}

// TestLoadSessionFoldsDuplicateSeqLastWriterWins: a torn write during a
// failed fsync can leave a prompt.queued record on disk whose
// EnqueuePromptDurable call reported failure; the successful retry then
// writes a second record with the SAME seq but a fresh (burned-forward) ID.
// Replay must converge to exactly ONE queue entry carrying the LATER id —
// matching what live memory held — never two entries or the torn record's id
// (a later prompt.dequeued referencing the retry's id must find its entry).
func TestLoadSessionFoldsDuplicateSeqLastWriterWins(t *testing.T) {
	dir := t.TempDir()
	writeSessionLog(t, dir, "sess_dup",
		`{"type":"session","id":"sess_dup","created_at":"2026-07-21T00:00:00Z"}`,
		`{"type":"model","model":{"provider":"test","model":"m1"}}`,
		`{"type":"prompt.queued","prompt":{"id":1,"text":"msg","seq":5}}`,
		`{"type":"prompt.queued","prompt":{"id":2,"text":"msg","seq":5}}`,
	)
	s, err := LoadSession(Config{SessionDir: dir}, "sess_dup")
	if err != nil {
		t.Fatal(err)
	}
	q := s.QueuedPrompts()
	if len(q) != 1 {
		t.Fatalf("queue len = %d, want 1 (last-writer-wins fold)", len(q))
	}
	if q[0].ID != 2 || q[0].Seq != 5 {
		t.Fatalf("folded entry = id %d seq %d, want id 2 seq 5", q[0].ID, q[0].Seq)
	}
	if got := s.EnqueueSeq(); got != 5 {
		t.Fatalf("EnqueueSeq = %d, want 5", got)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./engine/ -run 'TestLoadSessionRestoresEnqueueWatermark|TestLoadSessionFoldsDuplicateSeqLastWriterWins' -v`
Expected: compile FAIL — `QueuedPrompt` has no field `Seq`, `s.EnqueueSeq` undefined.

**Step 3: Implement**

`engine/queue.go` — extend `QueuedPrompt` (keep existing doc comment, append):

```go
type QueuedPrompt struct {
	ID   int64
	Text string
	// Seq is the caller-issued idempotency sequence for a prompt enqueued
	// via EnqueuePromptDurable (see store.go's promptRecord.Seq); 0 for a
	// plain EnqueuePrompt, which has no idempotency contract.
	Seq int64
}
```

Add accessor (near `QueuedPrompts`):

```go
// EnqueueSeq returns the durable-enqueue high-water mark: the largest seq
// accepted by EnqueuePromptDurable, live or restored by LoadSession's
// replay. A caller recovering after ITS OWN crash reads this to learn which
// messages are already inside the durability domain and must not be re-sent
// as fresh (they would be deduplicated anyway — this is the read that lets
// it skip the round-trip).
func (s *Session) EnqueueSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enqueueSeq
}
```

`engine/engine.go` — Session struct, next to `promptQueueNextID`:

```go
	// enqueueSeq is the durable-enqueue idempotency high-water mark (see
	// EnqueuePromptDurable in queue.go and promptRecord.Seq in store.go):
	// the largest caller-issued seq durably accepted. Monotonic; a seq at or
	// below it is a duplicate no-op. Rebuilt on replay by LoadSession.
	enqueueSeq int64
```

`engine/store.go` — `promptRecord` gains:

```go
	// Seq is the caller-issued idempotency sequence carried on a
	// prompt.queued record written by EnqueuePromptDurable (see queue.go);
	// 0/omitted on plain EnqueuePrompt records and on every
	// prompt.dequeued. LoadSession folds it into the session's enqueueSeq
	// high-water mark and dedupes same-seq records last-writer-wins — see
	// the recPromptQueued replay case for why that heals torn fsync
	// failures.
	Seq int64 `json:"seq,omitempty"`
```

`engine/store.go` — replace the `case recPromptQueued:` body in `LoadSession`:

```go
		case recPromptQueued:
			// Append to the folded queue and advance the next-ID counter past
			// whatever this record used (IDs are burned on failed durable
			// writes — see EnqueuePromptDurable — so advancing past every ID
			// seen, folded or not, is what keeps a resumed session's counter
			// collision-free).
			//
			// A record carrying Seq (durable enqueue) folds last-writer-wins
			// against any already-folded entry with the SAME Seq: a failed
			// fsync can leave a torn record on disk whose write reported
			// failure, followed by its successful retry under a fresh ID —
			// live memory only ever held the retry's entry, so replay must
			// converge to that one too (a later prompt.dequeued references
			// the retry's ID). Seq also advances the enqueueSeq high-water
			// mark, which is what makes duplicate detection survive a
			// process restart.
			if rec.Prompt != nil {
				q := QueuedPrompt{ID: rec.Prompt.ID, Text: rec.Prompt.Text, Seq: rec.Prompt.Seq}
				replaced := false
				if q.Seq > 0 {
					for i, p := range s.promptQueue {
						if p.Seq == q.Seq {
							s.promptQueue[i] = q
							replaced = true
							break
						}
					}
					if q.Seq > s.enqueueSeq {
						s.enqueueSeq = q.Seq
					}
				}
				if !replaced {
					s.promptQueue = append(s.promptQueue, q)
				}
				if rec.Prompt.ID >= s.promptQueueNextID {
					s.promptQueueNextID = rec.Prompt.ID + 1
				}
			}
```

**Step 4: Run tests**

Run: `go test ./engine/ -run 'TestLoadSessionRestoresEnqueueWatermark|TestLoadSessionFoldsDuplicateSeqLastWriterWins' -v` → PASS, then `go test ./engine/` → all PASS (no existing behavior changed: seq-less records take the exact old path).

**Step 5: Commit**

```bash
git add engine/ && git commit -m "feat(engine): seq on prompt.queued records — watermark replay, last-writer-wins fold"
```

---

### Task 2: Engine — `EnqueuePromptDurable`: write-ahead, fsync, dedupe, burned IDs

**Files:**
- Modify: `engine/queue.go` (new method, after `EnqueuePrompt`)
- Modify: `engine/engine.go` (Event struct: `QueueSeq` field next to `QueueLen`)
- Test: `engine/queue_durable_test.go` (extend)

**Step 1: Write the failing tests**

```go
// durableTestSession builds a persisted session rooted in a temp dir,
// capturing emitted events.
func durableTestSession(t *testing.T) (*Session, *[]Event) {
	t.Helper()
	var events []Event
	s := NewSession(Config{
		SessionDir: t.TempDir(),
		OnEvent:    func(ev Event) { events = append(events, ev) },
	})
	return s, &events
}

func TestEnqueuePromptDurableAcceptsAndAdvancesWatermark(t *testing.T) {
	s, events := durableTestSession(t)
	id, dup, err := s.EnqueuePromptDurable("hello", 1)
	if err != nil || dup {
		t.Fatalf("EnqueuePromptDurable = id %d dup %v err %v", id, dup, err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}
	if got := s.EnqueueSeq(); got != 1 {
		t.Fatalf("EnqueueSeq = %d, want 1", got)
	}
	q := s.QueuedPrompts()
	if len(q) != 1 || q[0].Seq != 1 || q[0].Text != "hello" {
		t.Fatalf("queue = %+v", q)
	}
	// Event carries the seq so a journal tailer can correlate.
	var found bool
	for _, ev := range *events {
		if ev.Type == EventPromptQueued && ev.QueueSeq == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no EventPromptQueued with QueueSeq=1 in %+v", *events)
	}
	// Durable now: a fresh load sees both entry and watermark.
	re, err := LoadSession(Config{SessionDir: s.cfg.SessionDir}, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if re.EnqueueSeq() != 1 || len(re.QueuedPrompts()) != 1 {
		t.Fatalf("reload: seq %d queue %d", re.EnqueueSeq(), len(re.QueuedPrompts()))
	}
}

// Duplicate and out-of-order seqs are the same case: at-or-below watermark
// is a clean no-op (the caller's retry or replay of an already-accepted
// message), nothing persisted or emitted.
func TestEnqueuePromptDurableDuplicateAndStaleSeqAreNoOps(t *testing.T) {
	s, events := durableTestSession(t)
	if _, _, err := s.EnqueuePromptDurable("m5", 5); err != nil {
		t.Fatal(err)
	}
	evBefore := len(*events)
	for _, seq := range []int64{5, 3} {
		id, dup, err := s.EnqueuePromptDurable("dup", seq)
		if err != nil || !dup || id != 0 {
			t.Fatalf("seq %d: id %d dup %v err %v, want 0 true nil", seq, id, dup, err)
		}
	}
	if len(s.QueuedPrompts()) != 1 || len(*events) != evBefore {
		t.Fatalf("duplicate mutated state: queue %d events %d->%d",
			len(s.QueuedPrompts()), evBefore, len(*events))
	}
}

func TestEnqueuePromptDurableRejectsInvalid(t *testing.T) {
	s, _ := durableTestSession(t)
	if _, _, err := s.EnqueuePromptDurable("  ", 1); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, _, err := s.EnqueuePromptDurable("x", 0); err == nil {
		t.Fatal("seq 0 accepted")
	}
	if _, _, err := NewSession(Config{}).EnqueuePromptDurable("x", 1); err == nil {
		t.Fatal("no SessionDir accepted — a durable enqueue with nowhere durable to write must error")
	}
}

// A failed write returns an error (unlike EnqueuePrompt's swallowed
// lastPersistErr — the entire point is that the caller must not 2xx), leaves
// the queue and watermark untouched, and BURNS the assigned ID so a torn
// on-disk record can never collide with a later plain enqueue's ID.
func TestEnqueuePromptDurableWriteFailureReturnsErrorAndBurnsID(t *testing.T) {
	var events []Event
	s := NewSession(Config{
		SessionDir: unwritableSessionDir(t),
		OnEvent:    func(ev Event) { events = append(events, ev) },
	})
	if _, _, err := s.EnqueuePromptDurable("doomed", 1); err == nil {
		t.Fatal("write failure did not surface as error")
	}
	if len(s.QueuedPrompts()) != 0 || s.EnqueueSeq() != 0 || len(events) != 0 {
		t.Fatalf("failed enqueue mutated state: queue %d seq %d events %d",
			len(s.QueuedPrompts()), s.EnqueueSeq(), len(events))
	}
	if s.PersistErr() == nil {
		t.Fatal("PersistErr not set")
	}
	s.mu.Lock()
	next := s.promptQueueNextID
	s.mu.Unlock()
	if next != 2 {
		t.Fatalf("promptQueueNextID = %d, want 2 (ID 1 burned by the failed write)", next)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./engine/ -run TestEnqueuePromptDurable -v`
Expected: compile FAIL — `EnqueuePromptDurable`, `QueueSeq` undefined. (`unwritableSessionDir` already exists in `store_failure_test.go`, same package.)

**Step 3: Implement**

`engine/engine.go` — Event struct, after `QueueLen` (extend the prompt-queue fields doc comment accordingly):

```go
	// QueueSeq is the caller-issued idempotency sequence on an
	// EventPromptQueued emitted by EnqueuePromptDurable (see queue.go);
	// 0/omitted on plain enqueues and on every EventPromptDequeued.
	QueueSeq int64 `json:"queue_seq,omitempty"`
```

`engine/queue.go`:

```go
// EnqueuePromptDurable is EnqueuePrompt with an honest durability and
// idempotency contract, for callers (an inbox poller, a coordinator relay)
// whose OWN upstream ack rides on this call's success — see
// docs/plans/2026-07-21-durable-enqueue.md:
//
//   - seq is a caller-issued, session-monotonic idempotency sequence. At or
//     below the current high-water mark (EnqueueSeq) the call is a clean
//     duplicate no-op — nothing persisted, emitted, or enqueued — so
//     upstream retries are always safe. The caller must issue seqs for one
//     session in nondecreasing order; a gap is fine (the mark jumps), an
//     out-of-order fresh seq is indistinguishable from a duplicate and is
//     dropped.
//   - The prompt.queued record (carrying seq) is written AND fsynced
//     before any in-memory mutation and before success returns —
//     write-ahead, unlike every other persist path in this package, which
//     buffers to the page cache and swallows errors into lastPersistErr.
//     An error return means "not durably accepted; retry with the same
//     seq"; only a nil error authorizes the caller to ack upstream.
//   - The assigned queue ID is burned on failure (the counter advances
//     regardless): a failed fsync may still have torn the record onto
//     disk, and reusing its ID for a later plain enqueue would fold two
//     different prompts under one ID on replay. LoadSession converges a
//     torn record and its successful same-seq retry last-writer-wins —
//     see the recPromptQueued replay case in store.go.
//
// Delivery of the enqueued prompt is unchanged #80 queue machinery: FIFO
// with plain-enqueued prompts, drained at idle dispatch or injected at
// tool-call/goal-turn boundaries.
func (s *Session) EnqueuePromptDurable(text string, seq int64) (id int64, duplicate bool, err error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, false, errors.New("engine: EnqueuePromptDurable requires non-empty text")
	}
	if seq < 1 {
		return 0, false, errors.New("engine: EnqueuePromptDurable requires seq >= 1")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq <= s.enqueueSeq {
		return 0, true, nil
	}
	if s.cfg.SessionDir == "" {
		return 0, false, errors.New("engine: EnqueuePromptDurable requires Config.SessionDir")
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return 0, false, err
	}
	// Burn the ID now, success or failure — see the doc comment above.
	id = s.promptQueueNextID
	s.promptQueueNextID++
	rec := record{Type: recPromptQueued, Prompt: &promptRecord{ID: id, Text: trimmed, Seq: seq}}
	if err := s.writeRecord(rec); err != nil {
		s.lastPersistErr = err
		return 0, false, err
	}
	if err := s.logFile.Sync(); err != nil {
		// The record may or may not have reached stable storage — torn
		// state. Nothing in memory moved, so a retry with the same seq is
		// clean here; replay's last-writer-wins fold heals the disk side.
		s.lastPersistErr = err
		return 0, false, err
	}
	s.promptQueue = append(s.promptQueue, QueuedPrompt{ID: id, Text: trimmed, Seq: seq})
	s.enqueueSeq = seq
	// Emit while still holding s.mu, exactly like EnqueuePrompt above.
	s.emit(Event{Type: EventPromptQueued, QueueID: id, QueueText: trimmed, QueueSeq: seq, QueueLen: len(s.promptQueue)})
	return id, false, nil
}
```

Note: `writeRecord` requires `s.logFile` non-nil, guaranteed by `ensureLog`; the header written by a first-ever `ensureLog` is covered by the same `Sync` (fsync flushes the whole file).

**Step 4: Run tests**

Run: `go test ./engine/ -run TestEnqueuePromptDurable -v` → PASS; `go test ./engine/` → PASS.

**Step 5: Commit**

```bash
git add engine/ && git commit -m "feat(engine): EnqueuePromptDurable — write-ahead fsync, seq dedupe, burned IDs"
```

---

### Task 3: Server — extract `releasePromptClaim` (mechanical DRY, no behavior change)

The run-slot release block (`st.running = false … s.wg.Done()`) is duplicated verbatim in `handlePrompt`'s enqueue-error path (`server/handlers.go:835-842`) and `dispatchQueueHead`'s empty branch (`server/handlers.go:1031-1038`); Task 4 would add two more copies.

**Files:**
- Modify: `server/handlers.go`

**Step 1: Add the helper** (above `dispatchQueueHead`):

```go
// releasePromptClaim releases a run-slot claim taken by claimForPrompt
// without running a turn: the exact reset runPrompt's own tail performs,
// shared by every path that claims the slot and then discovers there is
// nothing to run (an enqueue error, a queue emptied by a concurrent DELETE
// /session/{id}/queue, a duplicate durable enqueue).
func (s *Server) releasePromptClaim(st *sessionState) {
	s.mu.Lock()
	st.running = false
	st.cancel = nil
	st.goalLoop = false
	st.lastUsed = time.Now()
	s.evictResidentLocked()
	s.mu.Unlock()
	s.wg.Done()
}
```

Replace both existing inline blocks with `s.releasePromptClaim(st)`.

**Step 2: Run the full server suite (race) — pure refactor, everything stays green**

Run: `go test ./server/ -race`
Expected: PASS

**Step 3: Commit**

```bash
git add server/handlers.go && git commit -m "refactor(server): extract releasePromptClaim from duplicated claim-release blocks"
```

---

### Task 4: Server — `POST /session/{id}/enqueue`

**Files:**
- Modify: `server/server.go` (route, after the `prompt_async` line)
- Modify: `server/handlers.go` (handler + response type, after `enqueueOrDispatch`)
- Modify: `server/journal.go` (Event: `QueueSeq` field next to `QueueLen`; `publishQueue`: copy `ev.QueueSeq`)
- Test: `server/enqueue_test.go` (new)

**Step 1: Write the failing tests** (use `harness` from `server_test.go` and the `queueProv` blocking-provider pattern from `queue_test.go`; `h.do(method, path, body)` issues authed requests)

```go
package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/provider"
)

func enqueueBody(text string, seq int64) map[string]any {
	return map[string]any{
		"parts": []map[string]any{{"type": "text", "text": text}},
		"seq":   seq,
	}
}

// Idle session: the enqueued prompt is durably journaled, then dispatched
// immediately into the free run slot — "started", exactly like prompt_async
// on an idle session, but with the durable-first ordering.
func TestEnqueueIdleDispatchesImmediately(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test", turns: [][]provider.Event{scriptedTurn("ok")}})
	id := h.createSession(t)

	resp, body := h.do("POST", "/session/"+id+"/enqueue", enqueueBody("run this", 1))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var er struct {
		Status    string `json:"status"`
		Watermark int64  `json:"watermark"`
	}
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatal(err)
	}
	if er.Status != "started" || er.Watermark != 1 {
		t.Fatalf("resp = %+v, want started/1", er)
	}
	h.waitIdle(t, id)
}

// Busy session: durably queued behind the running turn (the #80 FIFO), and
// the duplicate retry of an accepted seq is a 200 no-op even while busy.
func TestEnqueueBusyQueuesAndDeduplicates(t *testing.T) {
	p := &queueProv{name: "test", started: make(chan struct{}), release: make(chan struct{}),
		turns: [][]provider.Event{scriptedTurn("drained")}}
	h := newHarness(t, p)
	id := h.createSession(t)
	h.promptAsync(t, id, "occupy") // blocks in provider
	<-p.started

	resp, body := h.do("POST", "/session/"+id+"/enqueue", enqueueBody("queued msg", 1))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var er struct {
		Status    string `json:"status"`
		Watermark int64  `json:"watermark"`
		Queued    int    `json:"queued"`
	}
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatal(err)
	}
	if er.Status != "queued" || er.Watermark != 1 || er.Queued != 1 {
		t.Fatalf("resp = %+v, want queued/1/1", er)
	}

	// Same seq again — upstream retry — clean duplicate, 200, still one copy.
	resp, body = h.do("POST", "/session/"+id+"/enqueue", enqueueBody("queued msg", 1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate status %d: %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatal(err)
	}
	if er.Status != "duplicate" || er.Watermark != 1 {
		t.Fatalf("duplicate resp = %+v", er)
	}

	close(p.release) // occupant finishes; queue drains via maybeDispatchQueued
	h.waitIdle(t, id)
}

func TestEnqueueValidation(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession(t)
	for name, body := range map[string]map[string]any{
		"missing seq":   {"parts": []map[string]any{{"type": "text", "text": "x"}}},
		"zero seq":      enqueueBody("x", 0),
		"empty parts":   {"parts": []map[string]any{}, "seq": 1},
		"non-text part": {"parts": []map[string]any{{"type": "image", "text": "x"}}, "seq": 1},
	} {
		resp, _ := h.do("POST", "/session/"+id+"/enqueue", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
}
```

Adapt helper names to what `server_test.go` actually provides (`createSession`/`promptAsync`/`waitIdle`/`scriptedTurn` or their real equivalents — read the file first; add tiny local helpers only if missing).

**Step 2: Run to verify failure**

Run: `go test ./server/ -run TestEnqueue -v`
Expected: FAIL — 404 (route absent).

**Step 3: Implement**

`server/journal.go` — Event struct after `QueueLen`:

```go
	QueueSeq int64 `json:"queue_seq,omitempty"`
```

(extend the prompt-queue doc comment: carried on prompt.queued records from durable enqueues) and in `publishQueue`, copy `QueueSeq: ev.QueueSeq`.

`server/server.go` route:

```go
	mux.HandleFunc("POST /session/{id}/enqueue", s.auth(s.handleEnqueue))
```

`server/handlers.go`:

```go
// enqueueResponse is POST /session/{id}/enqueue's success body. Unlike
// promptAsyncResponse it never carries the journal's SSE seq — the field
// name "seq" is already taken by the request's own idempotency sequence,
// and an enqueue caller acks by watermark, not by event cursor.
// Watermark is the session's durable-enqueue high-water mark AFTER this
// request (== the request's own seq on accept; the pre-existing mark on
// duplicate). Queued mirrors promptAsyncResponse's rule: depth including
// this prompt, only when status is "queued".
type enqueueResponse struct {
	Status    string `json:"status"` // "started" | "queued" | "duplicate"
	Watermark int64  `json:"watermark"`
	Queued    int    `json:"queued,omitempty"`
}

// handleEnqueue is POST /session/{id}/enqueue (see docs/plans/2026-07-21-
// durable-enqueue.md): prompt_async's shape with an honest durability and
// idempotency contract. The prompt is fsynced into the session journal
// (engine.Session.EnqueuePromptDurable) BEFORE any success response — a 2xx
// authorizes the caller to ack ITS upstream — and a seq at or below the
// session's watermark is a 200 duplicate no-op, so upstream retries are
// always safe. Delivery is unchanged #80 queue machinery: idle sessions
// dispatch the queue head immediately, busy sessions drain at turn/tool
// boundaries. No model override (queued prompts carry text only — see
// enqueueOrDispatch's doc comment); the workdir-busy 409, draining 503, and
// unknown-session 404 mirror handlePrompt.
func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	var body struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
		Seq int64 `json:"seq"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Parts) == 0 {
		writeErr(w, http.StatusBadRequest, "parts must be non-empty")
		return
	}
	if body.Seq < 1 {
		writeErr(w, http.StatusBadRequest, "seq must be >= 1")
		return
	}
	var texts []string
	for _, p := range body.Parts {
		if p.Type != "text" {
			writeErr(w, http.StatusBadRequest, "v1 accepts text parts only")
			return
		}
		texts = append(texts, p.Text)
	}
	text := strings.Join(texts, "\n")

	st, ctx, _, code, holder := s.claimForPrompt(id)
	if code != 0 {
		switch {
		case code == http.StatusConflict && holder != "":
			writeErr(w, code, fmt.Sprintf("workdir busy: held by session %s", holder))
		case code == http.StatusConflict:
			s.enqueueDurableBusy(w, id, text, body.Seq)
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}

	// Idle: we hold the run slot. Durable-first, then dispatch the queue
	// HEAD — not necessarily this request's prompt (global FIFO, same rule
	// as handlePrompt's idle-with-queue branch).
	ourID, dup, err := st.sess.EnqueuePromptDurable(text, body.Seq)
	if dup {
		s.releasePromptClaim(st)
		writeJSON(w, http.StatusOK, enqueueResponse{Status: "duplicate", Watermark: st.sess.EnqueueSeq()})
		return
	}
	if err != nil {
		s.releasePromptClaim(st)
		writeErr(w, http.StatusInternalServerError, "enqueue not durable: "+err.Error())
		return
	}
	head, ok := s.dispatchQueueHead(id, st, ctx)
	if !ok {
		// Concurrent DELETE /session/{id}/queue cleared everything in the
		// gap — same benign race as handlePrompt's idle-with-queue branch;
		// the prompt WAS durably accepted (watermark advanced), which is
		// exactly what the response must attest.
		writeJSON(w, http.StatusAccepted, enqueueResponse{
			Status: "queued", Watermark: st.sess.EnqueueSeq(), Queued: len(st.sess.QueuedPrompts()),
		})
		return
	}
	resp := enqueueResponse{Status: "queued", Watermark: st.sess.EnqueueSeq()}
	if head.ID == ourID {
		resp.Status = "started"
	} else {
		resp.Queued = len(st.sess.QueuedPrompts())
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// enqueueDurableBusy is handleEnqueue's same-session-busy branch, the
// durable mirror of enqueueOrDispatch: durably enqueue (fsynced, error on
// failure — never a silent 2xx), then ONE claim retry to close the
// freed-slot race. See enqueueOrDispatch's doc comment for the race
// analysis; only the enqueue call and response shape differ.
func (s *Server) enqueueDurableBusy(w http.ResponseWriter, id string, text string, seq int64) {
	sess := s.residentSession(id)
	if sess == nil {
		// Same benign race window as enqueueOrDispatch: busy occupant
		// finished and was evicted between the failed claim and here. The
		// caller retries with the same seq — idempotency makes that free.
		writeErr(w, http.StatusConflict, "session is busy with another prompt")
		return
	}
	ourID, dup, err := sess.EnqueuePromptDurable(text, seq)
	if dup {
		writeJSON(w, http.StatusOK, enqueueResponse{Status: "duplicate", Watermark: sess.EnqueueSeq()})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "enqueue not durable: "+err.Error())
		return
	}
	if s.queueDispatchRace != nil {
		s.queueDispatchRace() // test-only seam, mirrors enqueueOrDispatch
	}
	st, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		writeJSON(w, http.StatusAccepted, enqueueResponse{
			Status: "queued", Watermark: sess.EnqueueSeq(), Queued: len(sess.QueuedPrompts()),
		})
		return
	}
	head, ok := s.dispatchQueueHead(id, st, ctx)
	if !ok {
		writeJSON(w, http.StatusAccepted, enqueueResponse{
			Status: "queued", Watermark: sess.EnqueueSeq(), Queued: len(sess.QueuedPrompts()),
		})
		return
	}
	resp := enqueueResponse{Status: "queued", Watermark: sess.EnqueueSeq()}
	if head.ID == ourID {
		resp.Status = "started"
	} else {
		resp.Queued = len(sess.QueuedPrompts())
	}
	writeJSON(w, http.StatusAccepted, resp)
}
```

**Step 4: Run tests**

Run: `go test ./server/ -run TestEnqueue -v` → PASS; `go test ./server/ -race` → PASS.

**Step 5: Commit**

```bash
git add server/ && git commit -m "feat(server): POST /session/{id}/enqueue — durable idempotent enqueue endpoint"
```

---

### Task 5: Server — watermark survives restart (the headline guarantee)

**Files:**
- Test: `server/enqueue_test.go` (extend)

**Step 1: Write the failing-until-proven test** — mirror `restart_test.go`'s two-server-over-one-dir pattern:

```go
// TestEnqueueWatermarkSurvivesRestart is the primitive's reason to exist: a
// message accepted (2xx) by one serve process must read as a duplicate to
// its successor over the same session dir — the upstream that acked on the
// first 2xx must never cause a double delivery by retrying into the new
// process, and a message NEVER accepted must not read as one.
func TestEnqueueWatermarkSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	h1 := newHarnessDir(t, dir, &scriptedProvider{name: "test", turns: [][]provider.Event{scriptedTurn("ok")}})
	id := h1.createSession(t)
	if resp, body := h1.do("POST", "/session/"+id+"/enqueue", enqueueBody("m1", 1)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first enqueue: %d %s", resp.StatusCode, body)
	}
	h1.waitIdle(t, id)
	h1.ts.Close() // process one gone

	h2 := newHarnessDir(t, dir, &scriptedProvider{name: "test", turns: [][]provider.Event{scriptedTurn("ok")}})
	resp, body := h2.do("POST", "/session/"+id+"/enqueue", enqueueBody("m1", 1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("successor duplicate check: %d %s, want 200", resp.StatusCode, body)
	}
	var er enqueueResponse
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatal(err)
	}
	if er.Status != "duplicate" || er.Watermark != 1 {
		t.Fatalf("successor resp = %+v, want duplicate/1", er)
	}
	// A genuinely new message is accepted normally.
	if resp, _ := h2.do("POST", "/session/"+id+"/enqueue", enqueueBody("m2", 2)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("fresh seq after restart: %d", resp.StatusCode)
	}
}
```

(Adapt harness teardown to the file's actual restart pattern — check `restart_test.go` for how it closes the first server before opening the second.)

**Step 2: Run** `go test ./server/ -run TestEnqueueWatermarkSurvivesRestart -v` → PASS (Tasks 1–4 already provide this; the test pins the end-to-end guarantee against regression).

**Step 3: Commit**

```bash
git add server/enqueue_test.go && git commit -m "test(server): enqueue watermark survives process restart"
```

---

### Task 6: Server — `GET /session/{id}/queue` read surface

The reconciliation read: an upstream recovering from its own crash asks "what is already inside the durability domain" instead of re-sending blind (re-sending is safe but wasteful).

**Files:**
- Modify: `server/server.go` (route: `GET /session/{id}/queue`)
- Modify: `server/handlers.go` (handler near `handleQueueDelete`)
- Test: `server/enqueue_test.go` (extend)

**Step 1: Failing test**

```go
func TestQueueGetReturnsWatermarkAndPending(t *testing.T) {
	p := &queueProv{name: "test", started: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, p)
	id := h.createSession(t)
	h.promptAsync(t, id, "occupy")
	<-p.started
	h.do("POST", "/session/"+id+"/enqueue", enqueueBody("pending", 4))

	resp, body := h.do("GET", "/session/"+id+"/queue", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var q struct {
		Watermark int64 `json:"watermark"`
		Queued    []struct {
			ID   int64  `json:"id"`
			Text string `json:"text"`
			Seq  int64  `json:"seq"`
		} `json:"queued"`
	}
	if err := json.Unmarshal(body, &q); err != nil {
		t.Fatal(err)
	}
	if q.Watermark != 4 || len(q.Queued) != 1 || q.Queued[0].Seq != 4 || q.Queued[0].Text != "pending" {
		t.Fatalf("queue read = %+v", q)
	}
	close(p.release)
	h.waitIdle(t, id)
}
```

Also assert (same test or a sibling) the non-resident path: after `h.ts.Close()` + fresh harness over the same dir, `GET /session/{id}/queue` still reports the watermark.

**Step 2: Run** → FAIL 404.

**Step 3: Implement** — resolve resident-first, else transient `LoadSession` (read-only; check `handleGet` for an existing resolve helper and reuse it if one exists):

```go
// queueGetResponse is GET /session/{id}/queue: the durable-enqueue
// watermark plus the pending (undelivered) prompt queue in FIFO order.
// Queued is always present (empty array, never null) so consumers need no
// nil check. Seq is 0/omitted on plain prompt_async-queued entries.
type queueGetResponse struct {
	Watermark int64            `json:"watermark"`
	Queued    []queuedItemJSON `json:"queued"`
}

type queuedItemJSON struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
	Seq  int64  `json:"seq,omitempty"`
}

// handleQueueGet is the reconciliation read surface for durable enqueue
// (see docs/plans/2026-07-21-durable-enqueue.md): an upstream recovering
// from its own crash reads the watermark to learn which messages are
// already accepted rather than re-sending blind. Resident sessions answer
// from live state; non-resident ones from a transient replay — same
// journal, same fold, so the two can't disagree.
func (s *Server) handleQueueGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	sess := s.residentSession(id)
	if sess == nil {
		loaded, err := s.opts.LoadSession(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		sess = loaded
	}
	resp := queueGetResponse{Watermark: sess.EnqueueSeq(), Queued: []queuedItemJSON{}}
	for _, p := range sess.QueuedPrompts() {
		resp.Queued = append(resp.Queued, queuedItemJSON{ID: p.ID, Text: p.Text, Seq: p.Seq})
	}
	writeJSON(w, http.StatusOK, resp)
}
```

Route: `mux.HandleFunc("GET /session/{id}/queue", s.auth(s.handleQueueGet))` next to the DELETE.

**Step 4: Run** `go test ./server/ -race` → PASS.

**Step 5: Commit**

```bash
git add server/ && git commit -m "feat(server): GET /session/{id}/queue — watermark and pending-queue read surface"
```

---

### Task 7: Docs — OpenAPI, AGENTS.md, full sweep

**Files:**
- Modify: `server/openapi.yaml` — add `POST /session/{id}/enqueue` (request: `parts` + `seq`; responses: 202 started/queued, 200 duplicate, 400, 409 ×2, 500 `enqueue not durable`, 503) and `GET /session/{id}/queue`, mirroring the existing `prompt_async` / `DELETE queue` entries' style.
- Modify: `AGENTS.md` — extend the Prompt queue section: durable enqueue contract (2xx-after-fsync, seq idempotency, watermark, burned IDs, no model override), and that `prompt_async` remains the non-attesting fast path.
- This plan doc doubles as the design record (repo convention).

**Steps:**
1. Write both doc updates.
2. Run: `go vet ./... && go test ./...` → all PASS.
3. `gofmt -l .` → empty.
4. Commit:

```bash
git add server/openapi.yaml AGENTS.md docs/plans/2026-07-21-durable-enqueue.md
git commit -m "docs: durable idempotent enqueue — API surface and queue-section contract"
```

---

## Deferred (explicitly not in this plan)

- The inbox poll loop (`inbox` config, coordinator polling) — next plan, a thin client of this primitive.
- Boot-epoch fencing — coordinator-side concern; the primitive's seq contract already tolerates any interleaving of duplicate sends.
- Watermark in `GET /session/{id}` session JSON — `GET /session/{id}/queue` is the dedicated read; widen later if a consumer actually needs it inline.

---

## Implementation notes (post-plan amendments)

The landed code diverges from this plan's sketches in a few places; the
plan text above is left as the historical record (see
docs/plans/2026-07-19-prompt-queue.md's amendment style) rather than
rewritten to match.

- **Task 1's replay fold became remove+append, not in-place replace.** The
  sketch replaced a same-`seq` entry at its existing slot
  (`s.promptQueue[i] = q`); the landed `store.go` instead removes the old
  entry and appends the new one at the tail. A plain `EnqueuePrompt` can
  land between a torn durable write and its retry (log order
  id1/seq5-torn, id2/seq0-plain, id3/seq5-retry); an in-place replace would
  fold that to `[id3, id2]`, reordering delivery relative to what live
  memory actually did (which only ever appended id2 then id3). Remove+
  append reconstructs live append order faithfully; the common no-
  interposed-record case degenerates to the same single-entry result
  either way.
- **Task 2's `EnqueuePromptDurable` burns the queue ID before `ensureLog`,
  not after.** The sketch burned the ID only once `ensureLog` had already
  succeeded, so an `ensureLog` failure (opening/creating the log file)
  would NOT burn the ID. The landed code burns it first, so every failure
  path — `ensureLog`, the write, or the fsync — advances the counter past
  it, matching the doc comment's "every failure path... advances the
  counter past it" claim exactly.
- **Task 4's `handleEnqueue` idle dup/error branches additionally drain via
  `maybeDispatchQueued`**, not just `releasePromptClaim`. This closes a
  stranded-head liveness gap the sketch didn't cover: releasing the run
  slot after our own enqueue turns out to be a no-op (duplicate) or fails
  can strand a DIFFERENT concurrent request's already-durable prompt — one
  that lost its own claim retry to us inside `enqueueDurableBusy` — on a
  now-idle session with nothing left to dispatch it. See
  `TestEnqueueDuplicateOnIdleWithQueueDrainsHead`.
- **Task 6's `handleQueueGet` resolves via `s.lookup`**, the same
  resident-or-transient-load helper every other read endpoint uses,
  instead of the sketch's inline `s.residentSession` + raw
  `s.opts.LoadSession` pair.
