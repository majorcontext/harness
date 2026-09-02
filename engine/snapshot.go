// Session journal snapshots: a seq-anchored checkpoint beside the journal
// that bounds what LoadSession has to replay.
//
// The problem it solves. A session's durable state is one append-only JSONL
// journal (store.go), and LoadSession rebuilds a session by decoding every
// record in it, building the whole history slice, and repairing it. That is
// O(journal size) and grows for the life of the session: on a deployed box
// a single transcript read cost 8 s, and every cold prompt paid the same
// replay before it could start. See docs/design/journal-snapshotting.md.
//
// The shape. A snapshot is an explicitly-defined schema (sessionSnapshot
// below), NOT json.Marshal of a *Session: every field of Session but ID is
// unexported, and several of them — the config's live callbacks, the
// SessionManager pointer, open file handles — must never be serialized at
// all. The schema captures exactly the state LoadSession's folds
// reconstruct, anchored to the 1-based journal LINE NUMBER of the last
// record it covers. Recovery restores it and replays only records after
// that line.
//
// Five rules make it safe. They are the design's §4.5 list, and every one
// of them is load-bearing:
//
//  1. Seq-anchored. A snapshot is valid AS OF seq N; it never claims to be
//     current. The tail replay closes the gap, so a snapshot that lags the
//     journal is correct, merely less of a saving.
//  2. Off the hot path. The capture happens under s.mu at an append
//     boundary and copies a handful of slices and maps; the serialize and
//     the write happen in a background goroutine that holds no lock.
//  3. Atomic write. temp file -> fsync -> rename. A crash mid-write leaves
//     the PREVIOUS snapshot intact; a half-written <id>.snap.tmp is never
//     a file any reader looks at.
//  4. Single in-flight, coalesced. One snapshot per session at a time, so
//     the every-K and on-idle triggers cannot race to write the same path.
//  5. Rebuildable and validated. A snapshot is derived state with a
//     checksum and a version. ANY doubt — missing, torn, wrong version,
//     wrong session, seq ahead of the journal head — discards it and falls
//     back to a full replay. A snapshot bug degrades to slow, never wrong.
//
// The journal is never truncated. Snapshots are pure acceleration and can
// be deleted at any time; deleting them all restores exactly the behavior
// this package had before this file existed.
package engine

import (
	"encoding/json"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// sessionSnapshotVersion is the snapshot format version. Recovery
// DISCARDS — never migrates — a snapshot carrying any other value, so a
// field added to the schema needs no migration path: bump this and every
// stored snapshot falls back to a full replay on its next load and is
// rewritten from the next trigger.
const sessionSnapshotVersion = 1

// sessionSnapshotSuffix names a session's snapshot file. Like the metadata
// index's own suffix it deliberately does not end in ".jsonl", so no
// journal scan can mistake a snapshot for a session.
const sessionSnapshotSuffix = ".snap"

// sessionSnapshotTmpSuffix names the temp file the atomic write goes
// through. Nothing ever READS this path: that is what makes a crash
// mid-write invisible to recovery (rule 3).
const sessionSnapshotTmpSuffix = ".snap.tmp"

// defaultSnapshotEveryRecords is the product default cadence supplied by
// the config/CLI layer (config.Config.SnapshotEveryRecordsValue), not by
// this package: engine.Config.SnapshotEveryRecords' own zero value
// disables snapshotting, so a bare embedder-built Config keeps exactly the
// pre-snapshot behavior. See that field's doc comment.
const defaultSnapshotEveryRecords = 64

// sessionSnapshot is the explicit schema: exactly the state LoadSession's
// folds reconstruct from records after the session header, plus the anchor
// and the identity a reader validates against.
//
// Two exclusions are deliberate and must stay that way.
//
// Header-derived state (created_at, workdir, parent/task lineage) is NOT
// here. The session header is line 1 of every journal and costs one record
// to decode, so recovery replays it unconditionally and the snapshot never
// has to reproduce its subtle "an absent field means keep the loading
// Config's value" restore rules (see LoadSession's recSession case).
//
// Session.turn and Session.lastSystem are NOT here either, though the
// design's §4.1 field list names them. They have no durable source: no
// record carries them, so a full replay reports turn=0 and no system
// segments. Capturing them would make a snapshot-loaded session disagree
// with a full replay of the same journal — breaking the §4.3 invariant this
// whole file is built around, and making an observable field
// (session_info's turn count) depend on whether a snapshot happened to
// exist. The invariant governs; the field list does not.
//
// Every Session field (engine.go), not only these two, must be classified
// as either round-tripped here or deliberately excluded — see
// snapshottedSessionFields/snapshotExcludedSessionFields and
// TestEverySessionFieldIsClassifiedForSnapshotting in
// engine/snapshot_field_coverage_test.go. A new Session field the author
// forgets to add to this struct (and forgets to wire into
// captureSnapshotLocked/restoreSnapshot below) fails that test, closed by
// construction rather than by remembering to update it: this is the guard
// that would have caught claudeCodeCLISessionID/claudeCodeHistoryWatermark/
// claudeCodeSessionCostUSD/haveClaudeCodeCost going missing before they
// shipped missing.
type sessionSnapshot struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	// Seq is the 1-based journal line number of the LAST record this
	// snapshot covers. Recovery replays strictly greater lines.
	Seq int64 `json:"seq"`
	// CreatedAt is when the snapshot itself was written — diagnostics
	// only; nothing validates against it.
	CreatedAt time.Time `json:"created_at"`

	History []message.Message `json:"history"`

	Model       message.ModelRef `json:"model,omitzero"`
	Effort      message.Effort   `json:"effort,omitempty"`
	ServiceTier string           `json:"service_tier,omitempty"`

	Usage         provider.Usage `json:"usage,omitzero"`
	LastUsage     provider.Usage `json:"last_usage,omitzero"`
	HaveLastUsage bool           `json:"have_last_usage,omitempty"`

	GoalActive    bool   `json:"goal_active,omitempty"`
	GoalCondition string `json:"goal_condition,omitempty"`

	CompactCount    int       `json:"compact_count,omitempty"`
	LastCompactedAt time.Time `json:"last_compacted_at,omitzero"`

	PromptQueue       []QueuedPrompt `json:"prompt_queue,omitempty"`
	PromptQueueNextID int64          `json:"prompt_queue_next_id,omitempty"`
	EnqueueSeq        int64          `json:"enqueue_seq,omitempty"`

	ToolResults      map[string]toolResultMeta `json:"tool_results,omitempty"`
	ToolResultNextID int64                     `json:"tool_result_next_id,omitempty"`
	ToolResultBytes  int                       `json:"tool_result_bytes,omitempty"`

	MCPSelected []string `json:"mcp_selected,omitempty"`

	SpawnedChildIDs []string `json:"spawned_child_ids,omitempty"`

	// TaskNotifications is the UNDELIVERED set a full replay reconstructs:
	// the in-flight entries first, then the still-pending ones, which is
	// the same order requeueTaskNotifications restores them in and the
	// same order the journal holds them. taskNotificationsInFlight has no
	// record of its own — a checked-out notification is only "delivered"
	// once commitTaskNotifications writes for it — so a snapshot that
	// captured only Session.taskNotifications would silently drop the
	// checked-out ones that a full replay keeps.
	TaskNotifications []taskNotification `json:"task_notifications,omitempty"`

	TurnUnsettled    bool              `json:"turn_unsettled,omitempty"`
	CommittedOutcome *taskNotification `json:"committed_outcome,omitempty"`

	// ClaudeCodeCLISessionID, ClaudeCodeHistoryWatermark,
	// ClaudeCodeSessionCostUSD and HaveClaudeCodeCost mirror
	// Session.claudeCodeCLISessionID/claudeCodeHistoryWatermark/
	// claudeCodeSessionCostUSD/haveClaudeCodeCost — see their own doc
	// comments (engine.go). All four are set ONLY by a fold
	// (recClaudeCodeSessionID/recClaudeCodeHistoryWatermark/
	// recClaudeCodeUsage, store.go), so without a snapshot field for them
	// a snapshot-anchored load silently drops whichever of those records
	// fell at or before the anchor: --resume is never passed on the next
	// delegated turn (a needless fresh CLI session), the watermark resets
	// to 0 (a needless get_conversation_history directive), and the
	// cumulative claude-code dollar cost resets to 0/unset (as if no
	// delegated turn had ever completed). Empty/zero on an OLD snapshot
	// written before these fields existed — the same pre-fix behavior,
	// not a load failure.
	ClaudeCodeCLISessionID     string `json:"claude_code_cli_session_id,omitempty"`
	ClaudeCodeHistoryWatermark int    `json:"claude_code_history_watermark,omitempty"`

	ClaudeCodeSessionCostUSD float64 `json:"claude_code_session_cost_usd,omitempty"`
	HaveClaudeCodeCost       bool    `json:"have_claude_code_cost,omitempty"`
}

// sessionSnapshotFile is the on-disk wrapper: the snapshot bytes plus a
// checksum over exactly those bytes.
//
// The checksum is what turns a torn or tampered file into a miss instead of
// a wrong session. CRC-32 detects corruption; it does not prove its
// absence, and it is not a trust boundary — this is a derived cache file
// written by this package alone, and a collision costs a load that would
// otherwise have been a full replay anyway. Same reasoning, same algorithm
// as the metadata index sidecar (index.go).
type sessionSnapshotFile struct {
	CRC32    uint32          `json:"crc32"`
	Snapshot json.RawMessage `json:"snapshot"`
}

func sessionSnapshotPath(dir, id string) string {
	return filepath.Join(dir, id+sessionSnapshotSuffix)
}

func sessionSnapshotTmpPath(dir, id string) string {
	return filepath.Join(dir, id+sessionSnapshotTmpSuffix)
}

// marshalSessionSnapshot renders a snapshot as its on-disk bytes, checksum
// and all.
func marshalSessionSnapshot(snap *sessionSnapshot) ([]byte, error) {
	inner, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	return json.Marshal(sessionSnapshotFile{CRC32: crc32.ChecksumIEEE(inner), Snapshot: inner})
}

// readSessionSnapshot loads a session's stored snapshot. It returns nil for
// every failure — absent, unreadable, malformed, checksum mismatch, wrong
// version, wrong session id — because a snapshot has no repair path: the
// caller full-replays instead. The seq-versus-head check is the caller's,
// since only it knows the journal head.
func readSessionSnapshot(dir, id string) *sessionSnapshot {
	// The temp path is deliberately never consulted: a crash mid-write
	// leaves a half-written <id>.snap.tmp behind, and rule 3 is that no
	// reader ever looks at it.
	data, err := os.ReadFile(sessionSnapshotPath(dir, id))
	if err != nil {
		return nil
	}
	var file sessionSnapshotFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	if crc32.ChecksumIEEE(file.Snapshot) != file.CRC32 {
		return nil
	}
	var snap sessionSnapshot
	if err := json.Unmarshal(file.Snapshot, &snap); err != nil {
		return nil
	}
	if snap.Version != sessionSnapshotVersion || snap.ID != id || snap.Seq <= 0 {
		return nil
	}
	return &snap
}

// writeSessionSnapshot replaces id's snapshot atomically: write a temp
// file, fsync it, rename it over the old one. A crash at any point leaves
// either the old snapshot or the new one, never a mix (rule 3).
//
// sync selects whether the temp file is fsynced before the rename. It is
// skipped in volume mode for the same reason ensureLog skips its directory
// fsync there — see Config.SessionSync. Losing an unsynced snapshot to a
// crash costs one full replay, which is the behavior this package had
// before snapshots existed.
func writeSessionSnapshot(dir, id string, snap *sessionSnapshot, sync bool) error {
	b, err := marshalSessionSnapshot(snap)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := sessionSnapshotTmpPath(dir, id)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if sync {
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, sessionSnapshotPath(dir, id)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// snapshotEvery reports this session's every-K cadence, or 0 when
// snapshotting is off. See Config.SnapshotEveryRecords.
func (s *Session) snapshotEvery() int64 {
	if s.cfg.SnapshotEveryRecords <= 0 {
		return 0
	}
	return int64(s.cfg.SnapshotEveryRecords)
}

// snapshotEnabled reports whether this session can write a snapshot at all:
// it needs a cadence and somewhere to put the file.
func (s *Session) snapshotEnabled() bool {
	return s.snapshotEvery() > 0 && s.cfg.SessionDir != ""
}

// snapshotSafeLocked reports whether memory currently AGREES with the
// journal, which is the precondition every capture has: a snapshot pairs a
// memory image with a journal position, and recovery skips every record at
// or before that position.
//
// Two shapes break the agreement, and both are deliberate elsewhere in this
// package:
//
//   - A mutation whose durable record is DEFERRED (Session.durableDebt):
//     memory is ahead, and a snapshot would carry the mutation while
//     leaving its record in the tail for the reload to apply again —
//     duplicating a message or a task notification.
//   - A prompt-queue record parked on the session
//     (deferredQueueRecords, see queueRecordDeferredLocked in queue.go):
//     the same shape, tracked by the parked record itself rather than by a
//     counter.
//
// The opposite direction — a record written BEFORE its memory mutation, as
// EnqueuePromptDurable does deliberately — is handled by placing the
// trigger at the append boundary rather than inside writeRecord; see the
// comment there.
//
// Refusing to capture merely postpones a snapshot to the next boundary, so
// a guard that is too conservative costs a longer tail replay and nothing
// else. Caller holds s.mu.
func (s *Session) snapshotSafeLocked() bool {
	return s.durableDebt == 0 && len(s.deferredQueueRecords) == 0
}

// settleDurableDebtLocked records that a deferred durable write has landed.
// It clamps at zero: a mispaired call site must not be able to push the
// count negative and permanently ARM a capture in an unsafe window. The
// other direction — a leaked increment — merely stops this session from
// snapshotting, which degrades to the full replay this package did before
// snapshots existed. Caller holds s.mu.
func (s *Session) settleDurableDebtLocked() {
	if s.durableDebt > 0 {
		s.durableDebt--
	}
}

// maybeSnapshotLocked is the every-K trigger, called at an append boundary
// once the appending caller has applied BOTH its memory mutation and its
// durable record. Caller holds s.mu.
func (s *Session) maybeSnapshotLocked() {
	if !s.snapshotEnabled() || !s.snapshotSafeLocked() {
		return
	}
	if s.recordsWritten-s.snapshotSeq < s.snapshotEvery() {
		return
	}
	s.startSnapshotLocked()
}

// snapshotOnIdle is the on-idle trigger: a session that has just gone
// quiescent snapshots at the current journal head, so the next cold load —
// a wake from hibernation, a read after eviction — starts from there. It is
// a no-op when nothing has been written since the last snapshot.
func (s *Session) snapshotOnIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotIdleLocked()
}

// snapshotIdleLocked is snapshotOnIdle for a caller that already holds
// s.mu (ReleaseFiles, which snapshots on eviction).
func (s *Session) snapshotIdleLocked() {
	if !s.snapshotEnabled() || !s.snapshotSafeLocked() || s.recordsWritten <= s.snapshotSeq {
		return
	}
	s.startSnapshotLocked()
}

// startSnapshotLocked captures a consistent copy of the fold state and the
// seq it is anchored to, then serializes and writes it in a background
// goroutine. Caller holds s.mu.
//
// Coalescing (rule 4) is the snapshotting flag: while one write is in
// flight every other trigger returns immediately, so the two triggers can
// never race to write the same path and a burst of appends produces one
// write, not one per record.
//
// s.snapshotSeq advances at SCHEDULING time, not on success. A write that
// fails therefore does not re-arm the trigger on the very next record: the
// stored snapshot stays whatever it was, the next trigger fires K records
// later, and recovery replays a longer tail. Slower, never wrong.
func (s *Session) startSnapshotLocked() {
	if s.snapshotting {
		return
	}
	snap := s.captureSnapshotLocked()
	s.snapshotting = true
	s.snapshotSeq = snap.Seq
	dir, id, sync := s.cfg.SessionDir, s.ID, !s.volumeSync()
	s.snapshotWG.Add(1)
	go func() {
		defer s.snapshotWG.Done()
		inFlight := s.snapshotInFlight.Add(1)
		for {
			peak := s.snapshotConcurrentPeak.Load()
			if inFlight <= peak || s.snapshotConcurrentPeak.CompareAndSwap(peak, inFlight) {
				break
			}
		}
		err := writeSessionSnapshot(dir, id, snap, sync)
		s.snapshotInFlight.Add(-1)
		if err == nil {
			s.snapshotWrites.Add(1)
		}
		s.mu.Lock()
		s.snapshotting = false
		if err != nil {
			// Never lastPersistErr: a snapshot is derived acceleration,
			// and its loss is not a durability failure any caller should
			// be told about. The journal itself is untouched.
			s.lastSnapshotErr = err
		}
		s.mu.Unlock()
	}()
}

// waitSnapshots blocks until every snapshot write this session started has
// finished. It is what a shutdown path (and a test that wants a settled
// disk) uses; it is NOT a barrier a new snapshot cannot start behind.
func (s *Session) waitSnapshots() {
	s.snapshotWG.Wait()
}

// captureSnapshotLocked builds the snapshot value for the state as it
// stands. Caller holds s.mu.
//
// Every slice and map is COPIED, so the background goroutine serializes a
// value nothing else can mutate. The messages themselves are copied by
// value and still share their Parts pointers with live history — the same
// sharing every other reader of s.history in this package accepts, and safe
// for the same reason: a message's parts are normalized before the append
// that publishes them and are not mutated in place afterwards.
func (s *Session) captureSnapshotLocked() *sessionSnapshot {
	snap := &sessionSnapshot{
		Version:           sessionSnapshotVersion,
		ID:                s.ID,
		Seq:               s.recordsWritten,
		CreatedAt:         time.Now().UTC(),
		History:           append([]message.Message(nil), s.history...),
		Model:             s.model,
		Effort:            s.effort,
		ServiceTier:       s.serviceTier,
		Usage:             s.usage,
		LastUsage:         s.lastUsage,
		HaveLastUsage:     s.haveLastUsage,
		GoalActive:        s.goalActive,
		GoalCondition:     s.goalCondition,
		CompactCount:      s.compactCount,
		LastCompactedAt:   s.lastCompactedAt,
		PromptQueue:       append([]QueuedPrompt(nil), s.promptQueue...),
		PromptQueueNextID: s.promptQueueNextID,
		EnqueueSeq:        s.enqueueSeq,
		ToolResultNextID:  s.toolResultNextID,
		ToolResultBytes:   s.toolResultBytes,
		SpawnedChildIDs:   append([]string(nil), s.spawnedChildIDs...),
		TurnUnsettled:     s.turnUnsettled,

		ClaudeCodeCLISessionID:     s.claudeCodeCLISessionID,
		ClaudeCodeHistoryWatermark: s.claudeCodeHistoryWatermark,

		ClaudeCodeSessionCostUSD: s.claudeCodeSessionCostUSD,
		HaveClaudeCodeCost:       s.haveClaudeCodeCost,
	}
	if len(s.toolResults) > 0 {
		snap.ToolResults = make(map[string]toolResultMeta, len(s.toolResults))
		for k, v := range s.toolResults {
			snap.ToolResults[k] = v
		}
	}
	if len(s.mcpSelected) > 0 {
		for name := range s.mcpSelected {
			snap.MCPSelected = append(snap.MCPSelected, name)
		}
	}
	// In-flight first, then pending — see TaskNotifications' doc comment.
	if n := len(s.taskNotificationsInFlight) + len(s.taskNotifications); n > 0 {
		snap.TaskNotifications = make([]taskNotification, 0, n)
		snap.TaskNotifications = append(snap.TaskNotifications, s.taskNotificationsInFlight...)
		snap.TaskNotifications = append(snap.TaskNotifications, s.taskNotifications...)
	}
	if s.committedOutcome != nil {
		oc := *s.committedOutcome
		snap.CommittedOutcome = &oc
	}
	return snap
}

// restoreSnapshot writes a snapshot's state into a freshly-constructed
// session, in place of the records it covers. LoadSession calls it after
// applying the session header and before replaying the tail, so a later
// record still wins over anything here.
//
// Every message is Normalized, exactly as the record replay path
// normalizes each message it decodes: the snapshot was written from
// already-normalized history, so this is a no-op in practice, but making it
// unconditional is what keeps the two paths' output identical by
// construction rather than by assumption.
func (s *Session) restoreSnapshot(snap *sessionSnapshot) {
	s.history = make([]message.Message, len(snap.History))
	for i, m := range snap.History {
		m.Normalize()
		s.history[i] = m
	}
	if !snap.Model.IsZero() {
		s.model = snap.Model
	}
	s.effort = snap.Effort
	s.serviceTier = snap.ServiceTier
	s.usage = snap.Usage
	s.lastUsage = snap.LastUsage
	s.haveLastUsage = snap.HaveLastUsage
	s.goalActive = snap.GoalActive
	s.goalCondition = snap.GoalCondition
	s.compactCount = snap.CompactCount
	s.lastCompactedAt = snap.LastCompactedAt
	s.promptQueue = append([]QueuedPrompt(nil), snap.PromptQueue...)
	if snap.PromptQueueNextID > 0 {
		s.promptQueueNextID = snap.PromptQueueNextID
	}
	s.enqueueSeq = snap.EnqueueSeq
	if len(snap.ToolResults) > 0 {
		s.toolResults = make(map[string]toolResultMeta, len(snap.ToolResults))
		for k, v := range snap.ToolResults {
			s.toolResults[k] = v
		}
	}
	if snap.ToolResultNextID > 0 {
		s.toolResultNextID = snap.ToolResultNextID
	}
	s.toolResultBytes = snap.ToolResultBytes
	for _, name := range snap.MCPSelected {
		// Same defensive shape the recMCPToolsSelected fold applies: a
		// name that is not mcp__<server>__<tool> shaped is skipped, so one
		// rule holds however the state arrives.
		if _, _, ok := splitMCPToolName(name); !ok {
			continue
		}
		if s.mcpSelected == nil {
			s.mcpSelected = map[string]bool{}
		}
		s.mcpSelected[name] = true
	}
	s.spawnedChildIDs = append([]string(nil), snap.SpawnedChildIDs...)
	s.taskNotifications = append([]taskNotification(nil), snap.TaskNotifications...)
	s.turnUnsettled = snap.TurnUnsettled
	// Mirrors recClaudeCodeSessionID/recClaudeCodeHistoryWatermark/
	// recClaudeCodeUsage's own unconditional folds (store.go) — an
	// empty/zero snapshot value (an old snapshot predating these fields,
	// or a session never delegated) restores to exactly the zero value a
	// full replay would also leave.
	s.claudeCodeCLISessionID = snap.ClaudeCodeCLISessionID
	s.claudeCodeHistoryWatermark = snap.ClaudeCodeHistoryWatermark
	s.claudeCodeSessionCostUSD = snap.ClaudeCodeSessionCostUSD
	s.haveClaudeCodeCost = snap.HaveClaudeCodeCost
	if snap.CommittedOutcome != nil {
		oc := *snap.CommittedOutcome
		s.committedOutcome = &oc
	}
}

// snapshotStartAfter reports the journal line recovery may skip up to, and
// restores the snapshot's state into s when there is a usable one.
//
// It returns 0 — full replay — for every doubt: no snapshot, a snapshot for
// another session, a wrong version, a torn file, a seq that runs AHEAD of
// the journal head (a journal replaced or rolled back underneath a stale
// snapshot), or a journal whose first record is not a session header (in
// which case there is no header to apply before the restore, and the
// snapshot's own "the header is replayed separately" premise does not
// hold).
func (s *Session) snapshotStartAfter(dir, id string, data []byte, head int64) int64 {
	snap := readSessionSnapshot(dir, id)
	if snap == nil || snap.Seq > head {
		return 0
	}
	hdr, ok := firstJournalRecord(data)
	if !ok || hdr.Type != recSession {
		return 0
	}
	// The header first, then the snapshot on top of it: the header carries
	// the session's CREATE-time effort, which a later recEffort record (and
	// so the snapshot) supersedes.
	s.applySessionHeader(hdr)
	s.restoreSnapshot(snap)
	return snap.Seq
}

// firstJournalRecord decodes a journal's first non-empty line. It exists so
// recovery can apply the session header without decoding the records the
// snapshot already covers.
func firstJournalRecord(data []byte) (record, bool) {
	var out record
	found := false
	err := scanLogRaw(data, func(raw []byte, line int, isLast bool) error {
		if err := json.Unmarshal(raw, &out); err != nil {
			return errStopScan
		}
		found = true
		return errStopScan
	})
	if err != nil && !errors.Is(err, errStopScan) {
		return record{}, false
	}
	return out, found
}

// errStopScan ends a scanLogRaw walk early. scanLogRaw propagates every
// error but errTruncatedFinalRecord, so the caller matches on this
// sentinel rather than treating an early stop as a failure.
var errStopScan = errors.New("engine: stop scan")

// countJournalRecords counts a journal's records without decoding any of
// them — the journal head, for validating a snapshot's anchor against.
//
// It counts LINES, which can exceed the number of records a fold applies by
// at most one: a crash mid-write leaves a torn final line scanLog drops.
// That cannot admit a bad snapshot, because a torn write never advanced the
// writer's own record counter, so no snapshot's seq can name it.
func countJournalRecords(data []byte) int64 {
	var n int64
	_ = scanLogRaw(data, func(raw []byte, line int, isLast bool) error {
		n = int64(line)
		return nil
	})
	return n
}
