package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"pgregory.net/rapid"
)

// refSessionState is sessionReferenceFold's result: everything
// TestSessionReplayModel checks live session state against — the same
// queue/watermark/nextID fields refQueueState carries (queue_replay_model_
// test.go), plus goal, message-history, and usage state.
type refSessionState struct {
	queue     []QueuedPrompt
	watermark int64
	nextID    int64

	goalActive    bool
	goalCondition string

	// historyIDs carries exactly enough of each folded message — its ID and
	// Role — to (a) count messages after every fold and (b) let recCompact's
	// case below locate a FirstID/LastID range via spliceCompact. Parts/
	// CreatedAt/etc. are left zero: nothing here depends on them.
	historyIDs []message.Message

	usage provider.Usage
	// lastUsage/haveLastUsage fold ONLY from recMessage.Usage, never from a
	// recCompact record's Usage (see record.Usage's doc comment in store.go
	// and the recCompact case below) — the compact-usage-never-touches-
	// lastUsage rule.
	lastUsage     provider.Usage
	haveLastUsage bool
}

// sessionReferenceFold is an INDEPENDENT, test-local re-derivation of
// LoadSession's full replay switch (store.go): the queue folding logic is
// duplicated verbatim from referenceFold above (queue_replay_model_test.go)
// rather than shared, so this oracle stands on its own — it extends that
// fold with goal.*, message, and compact records, all re-derived directly
// from store.go's documented per-record-type semantics, deliberately NOT by
// calling or copying LoadSession's own switch. The one deliberate exception
// is spliceCompact itself (see the recCompact case below): the plan's own
// carve-out, since the differential value this test provides is WHERE
// compact records land relative to crashes, not re-deriving the splice's
// mechanics a second time.
//
// Shares scanLog's corruption discipline with referenceFold: split on '\n',
// drop trailing empty lines, and treat a trailing line that fails to parse
// as JSON as a write-in-progress torn by a crash (silently dropped) rather
// than corruption. Every write in this package is a single ordered append
// ending in exactly one '\n' (writeRecord), so folding only lines that parse
// is exact for a torn (suffix-truncated) file, not an approximation.
func sessionReferenceFold(tb rapid.TB, data []byte) refSessionState {
	st := refSessionState{nextID: 1}
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
			tb.Fatalf("sessionReferenceFold: unexpected corrupt non-final line %d: %v\ndata: %s", i+1, err, data)
		}
		switch rec.Type {
		case recPromptQueued:
			// Identical to referenceFold's recPromptQueued case (queue_
			// replay_model_test.go) — see that function's doc comment for
			// the full remove-then-append-at-tail rationale.
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
		case recGoalSet:
			// An active goal is one set without a later achieved/cleared —
			// see store.go's LoadSession recGoalSet case.
			st.goalActive = true
			if rec.Goal != nil {
				st.goalCondition = rec.Goal.Condition
			}
		case recGoalUpdated:
			// Only meaningful while active, rewriting the condition in
			// place — see store.go's LoadSession recGoalUpdated case.
			if st.goalActive && rec.Goal != nil {
				st.goalCondition = rec.Goal.Condition
			}
		case recGoalAchieved, recGoalCleared:
			st.goalActive = false
			st.goalCondition = ""
		case recGoalEval, recGoalStalled, recGoalEvalFailed, recGoalParked:
			// Pure trace records: never change goalActive/goalCondition by
			// themselves — see store.go's LoadSession case for the same
			// group.
		case recMessage:
			if rec.Message == nil {
				continue // tolerated exactly like LoadSession's isLast check; never produced mid-file by this model
			}
			st.historyIDs = append(st.historyIDs, message.Message{ID: rec.Message.ID, Role: rec.Message.Role})
			if rec.Usage != nil {
				st.usage.InputTokens += rec.Usage.InputTokens
				st.usage.OutputTokens += rec.Usage.OutputTokens
				st.usage.CacheReadTokens += rec.Usage.CacheReadTokens
				st.usage.CacheWriteTokens += rec.Usage.CacheWriteTokens
				st.lastUsage = *rec.Usage
				st.haveLastUsage = true
			}
		case recCompact:
			if rec.Compact == nil {
				continue // torn/corrupt compact payload dropped by the last-line tolerance above in the only way this model produces one
			}
			spliced, err := spliceCompact(st.historyIDs, rec.Compact.FirstID, rec.Compact.LastID, rec.Compact.Summary)
			if err != nil {
				tb.Fatalf("sessionReferenceFold: recCompact splice: %v\ndata: %s", err, data)
			}
			st.historyIDs = spliced
			// Cumulative usage only — see record.Usage's doc comment
			// (store.go) and this struct's lastUsage field comment above.
			if rec.Usage != nil {
				st.usage.InputTokens += rec.Usage.InputTokens
				st.usage.OutputTokens += rec.Usage.OutputTokens
				st.usage.CacheReadTokens += rec.Usage.CacheReadTokens
				st.usage.CacheWriteTokens += rec.Usage.CacheWriteTokens
			}
		}
	}
	return st
}

// sessionModel is the rapid.StateMachine driving TestSessionReplayModel: it
// extends queueModel's queue/crash-reload mechanics (queue_replay_model_
// test.go) with goal ops (RegisterGoal/UpdateGoal/ClearGoal), message turns
// (PromptTurn), and compaction (CompactTurn) — see this file's package doc
// comment on TestSessionReplayModel for the invariant this all feeds.
//
// prov is a single *scriptedProvider shared across every crash-reload (a
// provider connection is process-level, never persisted — see cfg below):
// PromptTurn/CompactTurn append the next deterministic turn to prov.turns
// immediately before driving it through the live *Session, so the provider
// is always exactly one scripted reply ahead of the call about to consume
// it.
type sessionModel struct {
	dir  string
	id   string
	s    *Session
	prov *scriptedProvider
	// cfg is reused, unchanged, on every CrashReload/TornCrashReload: unlike
	// queueModel's bare Config{SessionDir: dir} (fine there, since that
	// machine never calls Prompt/Compact after a reload), this machine's
	// reloaded session must still be able to run turns and compact, which
	// needs the same Providers/Model/CompactionKeepTurns LoadSession itself
	// does not (and cannot) restore from the journal.
	cfg Config

	msgSeq int // gives every scripted turn's message a fresh, unique ID — see nextMsgID
}

func newSessionModel(dir string) *sessionModel {
	prov := &scriptedProvider{name: "test"}
	cfg := Config{
		SessionDir: dir,
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		// Lower than the package default (2, see defaultCompactionKeepTurns)
		// so CompactTurn's precondition — more than keepTurns complete turns
		// on hand — is reachable well within rapid's default ~30
		// actions/check budget.
		CompactionKeepTurns: 1,
	}
	s := NewSession(cfg)
	return &sessionModel{dir: dir, id: s.ID, s: s, prov: prov, cfg: cfg}
}

// nextMsgID returns a fresh, unique message ID for a scripted turn's
// assistant reply — mirroring compact_test.go's compactTurnSeq convention:
// spliceCompact and this test's own historyIDs bookkeeping both key on
// message ID, so distinct turns need distinguishable IDs, exactly like
// production's newID() gives every real message.
func (m *sessionModel) nextMsgID(prefix string) string {
	m.msgSeq++
	return fmt.Sprintf("%s_%d", prefix, m.msgSeq)
}

var sessionDequeueReasons = dequeueReasons

// DurableEnqueue mirrors queueModel.DurableEnqueue exactly (queue_replay_
// model_test.go's doc comment covers the seqKind rationale) — duplicated
// rather than shared because the two machines' actions are methods on
// different receiver types; the helpers underneath (readJournalOrEmpty,
// compareQueueState, nextQueueID, dequeueReasons) are shared unchanged.
func (m *sessionModel) DurableEnqueue(t *rapid.T) {
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

// PlainEnqueue mirrors queueModel.PlainEnqueue.
func (m *sessionModel) PlainEnqueue(t *rapid.T) {
	text := fmt.Sprintf("plain-%d", rapid.IntRange(0, 1<<20).Draw(t, "textSeed"))
	if _, err := m.s.EnqueuePrompt(text); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}
}

// DequeueOne mirrors queueModel.DequeueOne.
func (m *sessionModel) DequeueOne(t *rapid.T) {
	reason := rapid.SampledFrom(sessionDequeueReasons).Draw(t, "reason")
	m.s.DequeuePrompt(reason)
}

// DequeueAll mirrors queueModel.DequeueAll.
func (m *sessionModel) DequeueAll(t *rapid.T) {
	reason := rapid.SampledFrom(sessionDequeueReasons).Draw(t, "reason")
	m.s.DequeueAllPrompts(reason)
}

// RegisterGoal exercises RegisterGoal with a fresh random condition every
// call. RegisterGoal errors (a no-op, nothing journaled) when a goal is
// already active — per goal.go's documented API semantics, that is an
// expected, non-fatal outcome here, not a test failure; any OTHER error is.
func (m *sessionModel) RegisterGoal(t *rapid.T) {
	cond := fmt.Sprintf("goal-cond-%d", rapid.IntRange(0, 1<<20).Draw(t, "cond"))
	if err := m.s.RegisterGoal(cond); err != nil && !strings.Contains(err.Error(), "already active") {
		t.Fatalf("RegisterGoal: unexpected error: %v", err)
	}
}

// UpdateGoal exercises UpdateGoal with a fresh random condition every call.
// UpdateGoal errors (a no-op) when no goal is active, and is a silent no-op
// (nil error, nothing journaled) when the drawn condition happens to match
// the current one — both expected, non-fatal outcomes; any other error is.
func (m *sessionModel) UpdateGoal(t *rapid.T) {
	cond := fmt.Sprintf("goal-cond-%d", rapid.IntRange(0, 1<<20).Draw(t, "cond"))
	if err := m.s.UpdateGoal(cond); err != nil && !strings.Contains(err.Error(), "no active goal") {
		t.Fatalf("UpdateGoal: unexpected error: %v", err)
	}
}

// ClearGoal exercises ClearGoal, a clean no-op (returns false) when no goal
// is active.
func (m *sessionModel) ClearGoal(t *rapid.T) {
	m.s.ClearGoal()
}

// drawUsage draws a small, arbitrary provider.Usage for one scripted turn.
func drawUsage(t *rapid.T, label string) provider.Usage {
	return provider.Usage{
		InputTokens:      rapid.IntRange(0, 500).Draw(t, label+"In"),
		OutputTokens:     rapid.IntRange(0, 500).Draw(t, label+"Out"),
		CacheReadTokens:  rapid.IntRange(0, 500).Draw(t, label+"CacheRead"),
		CacheWriteTokens: rapid.IntRange(0, 500).Draw(t, label+"CacheWrite"),
	}
}

// PromptTurn scripts and drives one deterministic single-turn Prompt call —
// the engine-test scripted-provider pattern engine_test.go's scriptedProvider/
// asstTurn and compact_test.go's compactTurn already establish: a plain-text
// assistant reply, no tool calls, ending StopEndTurn, carrying an explicit
// Usage. This appends exactly two messages to history (user, assistant) and
// folds this turn's Usage into cumulative usage AND lastUsage — the ordinary
// recMessage replay rule (see record.Usage's doc comment in store.go).
func (m *sessionModel) PromptTurn(t *rapid.T) {
	text := fmt.Sprintf("prompt-%d", rapid.IntRange(0, 1<<20).Draw(t, "text"))
	reply := fmt.Sprintf("reply-%d", rapid.IntRange(0, 1<<20).Draw(t, "reply"))
	usage := drawUsage(t, "turn")
	msg := &message.Message{
		ID:    m.nextMsgID("msg_asst"),
		Role:  message.RoleAssistant,
		Parts: message.Parts{&message.Text{Text: reply}},
	}
	m.prov.turns = append(m.prov.turns, []provider.Event{
		{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn, Usage: usage},
	})
	if _, err := m.s.Prompt(context.Background(), text); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
}

// CompactTurn scripts and drives one Session.Compact call, gated on exactly
// the precondition Compact itself checks (turnBoundaries(history) > the
// effective keep-turns floor — see compact.go's Compact and
// effectiveKeepTurns). This is a precondition GATE on the action, per the
// plan, not dropping compaction from the machine: without it, Compact's own
// documented no-op path (TurnsFolded==0, no provider call at all, when too
// few turns exist yet) would leave the scripted reply appended below
// stranded at the front of m.prov.turns, silently misdelivered to whatever
// provider call runs next — a test-harness bookkeeping hazard, not a
// production bug, that gating here avoids entirely.
func (m *sessionModel) CompactTurn(t *rapid.T) {
	history := m.s.History()
	keep := m.s.effectiveKeepTurns(0)
	if len(turnBoundaries(history)) <= keep {
		t.Skip("not enough complete turns to compact")
	}
	summary := fmt.Sprintf("summary-%d", rapid.IntRange(0, 1<<20).Draw(t, "summary"))
	usage := drawUsage(t, "compact")
	msg := &message.Message{
		ID:    m.nextMsgID("msg_summary"),
		Role:  message.RoleAssistant,
		Parts: message.Parts{&message.Text{Text: summary}},
	}
	m.prov.turns = append(m.prov.turns, []provider.Event{
		{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn, Usage: usage},
	})
	res, err := m.s.Compact(context.Background(), CompactOptions{})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.TurnsFolded == 0 {
		t.Fatalf("Compact: TurnsFolded = 0 despite passing the same precondition Compact itself checks (history=%d messages, keep=%d)", len(history), keep)
	}
}

// CrashReload mirrors queueModel.CrashReload, but reloads with m.cfg (not a
// bare Config{SessionDir: dir}) so the reloaded session still has Providers/
// Model wired — needed here, unlike the queue-only machine, since later
// actions in the same run may call Prompt/Compact against the reloaded
// handle.
func (m *sessionModel) CrashReload(t *rapid.T) {
	if _, err := os.Stat(sessionPath(m.dir, m.id)); os.IsNotExist(err) {
		t.Skip("no journal yet")
	}
	reloaded, err := LoadSession(m.cfg, m.id)
	if err != nil {
		t.Fatalf("crash-reload: LoadSession: %v", err)
	}
	m.s = reloaded
}

// TornCrashReload mirrors queueModel.TornCrashReload — see that method's doc
// comment (queue_replay_model_test.go) for why any suffix truncation must
// always reload cleanly — again reloading with m.cfg instead of a bare
// Config{SessionDir: dir}.
func (m *sessionModel) TornCrashReload(t *rapid.T) {
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
	reloaded, err := LoadSession(m.cfg, m.id)
	if err != nil {
		t.Fatalf("torn-crash-reload: LoadSession errored on a torn file (truncated to %d/%d bytes) — suffix truncation must always be tolerated: %v",
			truncTo, size, err)
	}
	m.s = reloaded
}

// Check is rapid's StateMachine invariant hook (see queueModel.Check's doc
// comment for the general shape): folds the on-disk journal exactly as it
// stands right now through sessionReferenceFold and compares every piece of
// live session state this task extended the machine to cover — queue,
// goal active/condition, message count (live history length vs the oracle's
// spliced length), cumulative usage, and lastUsage — against that
// independent replay.
func (m *sessionModel) Check(t *rapid.T) {
	data := readJournalOrEmpty(t, m.dir, m.id)
	want := sessionReferenceFold(t, data)

	gotWatermark, gotQueue := m.s.QueueState()
	compareQueueState(t, "live-vs-replay",
		refQueueState{queue: want.queue, watermark: want.watermark, nextID: want.nextID},
		gotWatermark, gotQueue, nextQueueID(m.s))

	gotCondition, gotActive := m.s.ActiveGoal()
	if gotActive != want.goalActive || gotCondition != want.goalCondition {
		t.Fatalf("live-vs-replay goal: active=%v condition=%q, want active=%v condition=%q",
			gotActive, gotCondition, want.goalActive, want.goalCondition)
	}

	gotHistory := m.s.History()
	if len(gotHistory) != len(want.historyIDs) {
		t.Fatalf("live-vs-replay message count: live history len = %d, want (oracle spliced) %d\n live:  %+v\n want:  %+v",
			len(gotHistory), len(want.historyIDs), gotHistory, want.historyIDs)
	}
	for i := range gotHistory {
		if gotHistory[i].ID != want.historyIDs[i].ID {
			t.Fatalf("live-vs-replay history[%d].ID = %q, want %q\n live:  %+v\n want:  %+v",
				i, gotHistory[i].ID, want.historyIDs[i].ID, gotHistory, want.historyIDs)
		}
	}

	if gotUsage := m.s.Usage(); gotUsage != want.usage {
		t.Fatalf("live-vs-replay cumulative usage = %+v, want %+v", gotUsage, want.usage)
	}
	gotLast, gotHaveLast := m.s.LastUsage()
	if gotHaveLast != want.haveLastUsage || (gotHaveLast && gotLast != want.lastUsage) {
		t.Fatalf("live-vs-replay lastUsage = %+v (ok=%v), want %+v (ok=%v)",
			gotLast, gotHaveLast, want.lastUsage, want.haveLastUsage)
	}
}

// TestSessionReplayModel is TestQueueReplayModel's whole-session extension
// (queue_replay_model_test.go): the same rapid state-machine, differential-
// oracle approach, now driving goal ops, deterministic scripted-provider
// Prompt turns, and Compact calls alongside the original queue/crash-reload
// ops, all under the same live-vs-replay Check after every single action.
// See sessionModel's and sessionReferenceFold's doc comments for what each
// side independently derives.
//
// Default rapid tuning keeps this comfortably under the plan's ~10s budget
// (see docs/plans/2026-07-21-fuzz-property-coverage.md's Conventions): the
// only per-action I/O is the same handful of small appends/fsyncs
// TestQueueReplayModel already pays, plus in-process scripted-provider calls
// with no real network or disk I/O of their own.
func TestSessionReplayModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dir, err := os.MkdirTemp("", "session-replay-model-*")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })

		m := newSessionModel(dir)
		t.Repeat(rapid.StateMachineActions(m))
	})
}
