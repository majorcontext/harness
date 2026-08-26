// Session metadata index: a per-session summary a reader can answer
// GET /session and GET /session/{id} from, without replaying the journal.
//
// The problem it solves. A session's durable state lives in one append-only
// JSONL journal (store.go). Before this file, the only way to read a
// non-resident session's model, usage, or message count was LoadSession —
// a full replay that decodes every message body, builds the whole history
// slice, and runs message.ResolveOrphanToolCalls over it. A read endpoint
// then used four fields of the result and dropped the rest. On a long
// production session (1.4 MB, ~500 messages) that replay cost about 7 s
// per read, and the list endpoint paid it once per non-resident session.
//
// The shape. SessionIndex is a memoized FOLD of the journal, keyed by the
// journal's byte length. Two rules make it safe:
//
//  1. The index is derived from RECORDS, never from live Session memory.
//     Every record reaches disk through Session.writeRecord (store.go), so
//     applyIndexRecord folds exactly what the log holds. This matters for
//     EnqueuePromptDurable (queue.go), which deliberately writes its
//     record BEFORE it mutates memory: a fold of live memory taken at that
//     instant would disagree with the log it claims to summarize.
//
//  2. The index is a CACHE, never an authority. ReadSessionIndex trusts a
//     stored index only when it still describes the journal on disk: same
//     byte length, same modification time, and a checksum that covers the
//     stored bytes. Anything else — a missing index, a torn one, an older
//     format, a shorter journal, a journal a second writer grew — is
//     refolded from byte 0. No repair path exists, so no repair path can be
//     wrong.
//
//     Byte length plus modification time is a staleness key, not a proof.
//     It rests on the journal's own contract: one writer, append only.
//     Nothing in this package rewrites a journal in place. The one repair
//     that touches existing bytes, ensureLog's torn-tail repair, always
//     changes the length. An external rewrite that preserved both length
//     and modification time would defeat the key, and is outside that
//     contract.
//
// The fold is deliberately SLIM: it decodes message ids, roles, and
// timestamps, never message bodies (indexRecord below). A full refold of
// the 1.4 MB session above costs milliseconds, not seconds, so even the
// cold path — a journal written by an older binary, or the first read after
// a crash — is cheap. See engine/index_test.go's oracle test, which pins
// every field against the value a full LoadSession produces.
//
// The fold counts messages twice, on purpose. Messages is what a full
// LoadSession reports: the durable messages PLUS the synthetic tool results
// message.ResolveOrphanToolCalls adds for a tool call whose result never
// reached the log. The fold gets that count by running that exact function
// over a skeleton of the history — ids, roles, and tool-call ids, no
// bodies — so the index can never disagree with the repair about how many
// messages a reader sees. DurableMessages counts only the records
// themselves. A reader that must map a message to a byte offset needs the
// second number, because a repair message has no record to map to; that is
// what paginated message reads are numbered against.
package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// sessionIndexVersion is the sidecar format version. ReadSessionIndex
// refolds — never guesses — when a stored index carries any other value, so
// a field added here needs no migration: bump this and every stale sidecar
// is rebuilt on its next read.
const sessionIndexVersion = 1

// sessionIndexSuffix is appended to a session id to name its sidecar. It
// deliberately does NOT end in ".jsonl", so ListSessionIndexes' own scan for
// session journals can never pick a sidecar up as a session.
const sessionIndexSuffix = ".index.json"

// SessionIndex summarizes one persisted session: everything GET /session
// and GET /session/{id} report about it that has a durable source.
//
// Fields with no durable source are absent by construction, not omitted by
// accident: a session's live/idle status, its in-process goal presentation
// (server/journal.go's goalTracker), and its last turn outcome all belong
// to the process that ran the turn, not to the log.
type SessionIndex struct {
	Version int    `json:"version"`
	ID      string `json:"id"`

	CreatedAt time.Time `json:"created_at"`
	// LastActivityAt is the CreatedAt of the newest durable message, or
	// CreatedAt when the session has none — the same fallback
	// Session.LastActivityAt applies for a log whose message records
	// predate the timestamp field.
	LastActivityAt time.Time `json:"last_activity_at"`

	Model  message.ModelRef `json:"model,omitzero"`
	Effort message.Effort   `json:"effort,omitempty"`

	WorkDir       string `json:"workdir,omitempty"`
	ParentSession string `json:"parent_session,omitempty"`

	// TaskParentID, TaskAgentType, TaskDepth, and SpawnedChildIDs are the
	// durable half of a session's subagent lineage — the same fields
	// lineageJSONFor's cold branch (server/handlers.go) reads off a loaded
	// Session. The live half (status, result, fail reason) has no durable
	// source and is not here.
	TaskParentID    string   `json:"task_parent_id,omitempty"`
	TaskAgentType   string   `json:"task_agent_type,omitempty"`
	TaskDepth       int      `json:"task_depth,omitempty"`
	SpawnedChildIDs []string `json:"spawned_child_ids,omitempty"`

	// Messages is the message count a full LoadSession reports: durable
	// messages after compaction splices, plus the synthetic tool results
	// message.ResolveOrphanToolCalls adds. DurableMessages counts the
	// records alone. The two differ only for a journal that carries a tool
	// call whose result never reached the log — see this file's header
	// comment.
	Messages        int            `json:"messages"`
	DurableMessages int            `json:"durable_messages"`
	Usage           provider.Usage `json:"usage,omitzero"`
	LastInputTokens int            `json:"last_input_tokens,omitempty"`

	// GoalActive and GoalCondition are the durable goal state LoadSession
	// restores (store.go's recGoalSet fold): the condition of a goal set
	// without a later goal.achieved or goal.cleared. The run counters
	// deliberately do not survive, live or here.
	GoalActive    bool   `json:"goal_active,omitempty"`
	GoalCondition string `json:"goal_condition,omitempty"`

	// Queued is the durable prompt-queue depth (queue.go): prompts
	// enqueued and not yet dequeued.
	Queued int `json:"queued,omitempty"`

	CompactionCount int       `json:"compaction_count,omitempty"`
	LastCompactedAt time.Time `json:"last_compacted_at,omitzero"`

	// Complete reports whether the journal recorded a model and a workdir.
	// Both are fields LoadSession will otherwise take from the loading
	// Config: a legacy header carries no workdir, and a crash can tear away
	// the initial model record a fresh log writes beside its header. A fold
	// has no Config, so it reports the gap instead of an empty value, and
	// the caller uses the authoritative load path for that session (see
	// server.Server.coldSessionJSON).
	//
	// Model and workdir are the WHOLE fallback surface for a reader, and
	// that rests on a contract the reader owes this package: the Config it
	// loads a session with must not name state that belongs to one specific
	// session. LoadSession applies the same "absent means keep the Config
	// value" rule to ParentSession, TaskParentID, TaskAgentType, and
	// TaskDepth. Absence is the NORMAL case for all four — most sessions
	// have no parent and no task lineage — so treating an absent one as
	// incomplete would send every read back to the load path and delete the
	// point of this index. A Config that carries a generic model and
	// workdir, as harness serve's own loader does (loadSessionFn,
	// cmd/harness/main.go), therefore diverges nowhere. See
	// server.Options.LoadSession for the same rule stated to the embedder
	// who supplies that Config.
	Complete bool `json:"complete"`

	// LogSize and LogModTime are the journal length and modification time
	// this index folds, and together they are the staleness key: an index
	// is current when, and only when, both still match the journal on disk.
	// Journals are append-only, so a longer journal means records this fold
	// never saw, and a shorter one means a torn-tail repair (ensureLog)
	// rewrote history under it. Both refold. See this file's header comment
	// for what the key rests on and what it does not prove.
	LogSize    int64     `json:"log_size"`
	LogModTime time.Time `json:"log_mod_time,omitzero"`
}

// indexMessage is the slim decode of a record's message body: identity,
// role, timestamp, and the call id of every tool call and tool result.
// Nothing else is decoded — a Text part's text and a Blob part's bytes are
// walked by encoding/json and dropped, never turned into message.Part
// values, which is what keeps a refold milliseconds rather than seconds.
//
// The call ids are here for one reason: message.ResolveOrphanToolCalls
// decides its repair from roles and call ids alone, so a skeleton carrying
// them lets the fold ask that exact function how many messages a full load
// would produce (see skeleton and indexFold.snapshot).
type indexMessage struct {
	ID        string       `json:"id"`
	Role      message.Role `json:"role"`
	CreatedAt time.Time    `json:"created_at"`
	Parts     []indexPart  `json:"parts"`
}

// indexPart is the slim decode of one part: its kind and, for the two kinds
// that pair a call with its result, the call id.
type indexPart struct {
	Type   message.PartType `json:"type"`
	CallID string           `json:"call_id"`
}

// skeleton builds the message.Message the fold keeps for one record: the
// identity fields, plus a ToolCall or ToolResult part for each call id, and
// nothing else. It is never served to a reader. It exists so the fold can
// run the real repair and the real compaction splice over it.
func (m indexMessage) skeleton() message.Message {
	out := message.Message{ID: m.ID, Role: m.Role, CreatedAt: m.CreatedAt}
	for _, p := range m.Parts {
		switch p.Type {
		case message.PartToolCall:
			out.Parts = append(out.Parts, &message.ToolCall{CallID: p.CallID})
		case message.PartToolResult:
			out.Parts = append(out.Parts, &message.ToolResult{CallID: p.CallID})
		}
	}
	return out
}

// indexCompact is the slim decode of a compact record's payload (see
// compactRecord). Summary carries only the summary message's identity —
// the fold splices by id, never by content.
type indexCompact struct {
	FirstID     string       `json:"first_id"`
	LastID      string       `json:"last_id"`
	TurnsFolded int          `json:"turns_folded"`
	Summary     indexMessage `json:"summary"`
}

// indexRecord is one journal line, decoded to just the fields the fold
// reads. It mirrors record (store.go) field for field where they overlap,
// so a record type the fold ignores costs a type-string compare and
// nothing else.
type indexRecord struct {
	Type          string           `json:"type"`
	ID            string           `json:"id,omitempty"`
	CreatedAt     time.Time        `json:"created_at,omitzero"`
	WorkDir       string           `json:"workdir,omitempty"`
	ParentSession string           `json:"parent_session,omitempty"`
	TaskParentID  string           `json:"task_parent_id,omitempty"`
	TaskAgentType string           `json:"task_agent_type,omitempty"`
	TaskDepth     int              `json:"task_depth,omitempty"`
	Model         message.ModelRef `json:"model,omitzero"`
	Effort        message.Effort   `json:"effort,omitempty"`
	Message       *indexMessage    `json:"message,omitempty"`
	Usage         *provider.Usage  `json:"usage,omitempty"`
	Goal          *goalRecord      `json:"goal,omitempty"`
	Prompt        *promptRecord    `json:"prompt,omitempty"`
	TaskSpawn     *taskSpawnRecord `json:"task_spawn,omitempty"`
	Compact       *indexCompact    `json:"compact,omitempty"`
}

// indexRecordOf projects a full record (the shape the write path and
// LoadSession already hold) onto the fold's input. The two paths therefore
// share ONE fold: a record folded as it is written and the same record
// folded from disk take the identical branch.
func indexRecordOf(rec record) indexRecord {
	out := indexRecord{
		Type:          rec.Type,
		ID:            rec.ID,
		CreatedAt:     rec.CreatedAt,
		WorkDir:       rec.WorkDir,
		ParentSession: rec.ParentSession,
		TaskParentID:  rec.TaskParentID,
		TaskAgentType: rec.TaskAgentType,
		TaskDepth:     rec.TaskDepth,
		Model:         rec.Model,
		Effort:        rec.Effort,
		Usage:         rec.Usage,
		Goal:          rec.Goal,
		Prompt:        rec.Prompt,
		TaskSpawn:     rec.TaskSpawn,
	}
	if rec.Message != nil {
		out.Message = indexMessageOf(*rec.Message)
	}
	if rec.Compact != nil {
		out.Compact = &indexCompact{
			FirstID:     rec.Compact.FirstID,
			LastID:      rec.Compact.LastID,
			TurnsFolded: rec.Compact.TurnsFolded,
			Summary:     *indexMessageOf(rec.Compact.Summary),
		}
	}
	return out
}

// indexMessageOf projects a full message onto the fold's slim shape — the
// write path's counterpart to the slim JSON decode a refold performs. It
// keeps the call ids for the same reason indexMessage carries them.
func indexMessageOf(m message.Message) *indexMessage {
	out := &indexMessage{ID: m.ID, Role: m.Role, CreatedAt: m.CreatedAt}
	for _, p := range m.Parts {
		switch v := p.(type) {
		case *message.ToolCall:
			out.Parts = append(out.Parts, indexPart{Type: message.PartToolCall, CallID: v.CallID})
		case *message.ToolResult:
			out.Parts = append(out.Parts, indexPart{Type: message.PartToolResult, CallID: v.CallID})
		}
	}
	return out
}

// indexFold accumulates a SessionIndex from journal records in log order.
//
// It keeps a message SKELETON — one message.Message per durable message,
// carrying id, role, and timestamp and no parts — rather than a plain
// counter, because compaction folds a RANGE of messages named by their
// first and last id. Holding the skeleton lets the fold call the same
// spliceCompact and healCompactFoldEnd that LoadSession calls (compact.go),
// so the two can never disagree about what a compact record does to a
// history.
type indexFold struct {
	ix       SessionIndex
	messages []message.Message
	queue    promptQueueFold
	// header is set by the session header record. A journal whose first
	// record is not a header is not a session log (events.jsonl is the one
	// in-tree example), and snapshot refuses it.
	header bool
	// broken records a fold that hit a record it could not apply — today,
	// only a compact record whose range is absent from the skeleton. Such
	// a fold is never written to disk and never served: the caller falls
	// back to the authority, LoadSession.
	broken bool
}

// applyIndexRecord folds one record. It mirrors LoadSession's own switch
// (store.go) case for case, and shares its helpers for the three folds with
// real state machines behind them: compaction (applyCompactRecord),
// goals (applyGoalRecord), and the prompt queue (promptQueueFold).
func (f *indexFold) applyIndexRecord(rec indexRecord, isLast bool) error {
	switch rec.Type {
	case recSession:
		f.header = true
		f.ix.ID = rec.ID
		f.ix.CreatedAt = rec.CreatedAt
		f.ix.WorkDir = rec.WorkDir
		f.ix.ParentSession = rec.ParentSession
		f.ix.TaskParentID = rec.TaskParentID
		f.ix.TaskAgentType = rec.TaskAgentType
		f.ix.TaskDepth = rec.TaskDepth
		f.ix.Effort = rec.Effort
	case recMessage:
		if rec.Message == nil {
			if isLast {
				return nil
			}
			return errors.New("message record without message")
		}
		f.messages = append(f.messages, rec.Message.skeleton())
		if rec.Usage != nil {
			f.addUsage(*rec.Usage)
			f.ix.LastInputTokens = rec.Usage.InputTokens
		}
	case recModel:
		f.ix.Model = rec.Model
	case recEffort:
		f.ix.Effort = rec.Effort
	case recGoalSet, recGoalUpdated, recGoalAchieved, recGoalCleared:
		f.ix.GoalActive, f.ix.GoalCondition = applyGoalRecord(f.ix.GoalActive, f.ix.GoalCondition, rec.Type, rec.Goal)
	case recPromptQueued:
		if rec.Prompt != nil {
			f.queue.queued(*rec.Prompt)
		}
	case recPromptDequeued:
		if rec.Prompt != nil {
			f.queue.dequeued(*rec.Prompt)
		}
	case recTaskSpawned:
		if rec.TaskSpawn != nil && rec.TaskSpawn.ChildID != "" {
			f.ix.SpawnedChildIDs = append(f.ix.SpawnedChildIDs, rec.TaskSpawn.ChildID)
		}
	case recCompact:
		if rec.Compact == nil {
			return errors.New("compact record without payload")
		}
		spliced, err := applyCompactRecord(f.messages, rec.Compact.FirstID, rec.Compact.LastID, rec.Compact.TurnsFolded, rec.Compact.Summary.skeleton())
		if err != nil {
			// The same corruption LoadSession fails on. Mark the fold
			// broken rather than returning: a listing must still report
			// every OTHER session, and this one's reader falls back to
			// LoadSession, which produces the authoritative error.
			f.broken = true
			return nil
		}
		f.messages = spliced
		f.ix.CompactionCount++
		f.ix.LastCompactedAt = rec.CreatedAt
		if rec.Usage != nil {
			// Cumulative usage only — never LastInputTokens. See
			// record.Usage's doc comment (store.go): a reloaded session
			// must not report the small summarization call as its last
			// request size.
			f.addUsage(*rec.Usage)
		}
	}
	return nil
}

// applyIndexRecordBestEffort is the live write path's entry point: it folds
// a record and treats any failure as "this fold can no longer be trusted"
// rather than as an error to propagate. Nothing on the write path may fail
// a session because a cache could not be updated — see writeRecord.
func (f *indexFold) applyIndexRecordBestEffort(rec indexRecord, isLast bool) {
	if f.broken {
		return
	}
	if err := f.applyIndexRecord(rec, isLast); err != nil {
		f.broken = true
	}
}

func (f *indexFold) addUsage(u provider.Usage) {
	f.ix.Usage.InputTokens += u.InputTokens
	f.ix.Usage.OutputTokens += u.OutputTokens
	f.ix.Usage.CacheReadTokens += u.CacheReadTokens
	f.ix.Usage.CacheWriteTokens += u.CacheWriteTokens
}

// snapshot renders the fold as an index covering a journal of logSize
// bytes last modified at modTime. ok is false for a fold that never saw a
// session header, or one a record broke — neither is a summary anything may
// serve or store.
//
// Messages and LastActivityAt are computed through
// message.ResolveOrphanToolCalls, the same repair LoadSession applies after
// its own replay, so both fields equal what a full load reports. The repair
// runs over a copy of the skeleton: it may append parts to the messages it
// is given, and the fold must keep accumulating from unrepaired state.
func (f *indexFold) snapshot(logSize int64, modTime time.Time) (SessionIndex, bool) {
	if !f.header || f.broken {
		return SessionIndex{}, false
	}
	ix := f.ix
	ix.Version = sessionIndexVersion
	ix.DurableMessages = len(f.messages)
	ix.Queued = len(f.queue.queue)
	ix.LogSize = logSize
	ix.LogModTime = modTime
	// Complete: the journal recorded every field a reader needs, so no
	// Config fallback is involved. See SessionIndex.Complete.
	ix.Complete = !ix.Model.IsZero() && ix.WorkDir != ""

	repaired := message.ResolveOrphanToolCalls(append([]message.Message(nil), f.messages...))
	ix.Messages = len(repaired)
	ix.LastActivityAt = ix.CreatedAt
	if n := len(repaired); n > 0 {
		// Same fallback as Session.LastActivityAt, and the same input: a
		// message record written before the timestamp field existed
		// replays as zero, and so does a synthetic repair message, which
		// is why this reads the REPAIRED tail rather than the durable one.
		if t := repaired[n-1].CreatedAt; !t.IsZero() {
			ix.LastActivityAt = t
		}
	}
	if len(ix.SpawnedChildIDs) > 0 {
		ix.SpawnedChildIDs = append([]string(nil), ix.SpawnedChildIDs...)
	}
	return ix, true
}

// sessionIndexPath names a session's sidecar index file.
func sessionIndexPath(dir, id string) string {
	return filepath.Join(dir, id+sessionIndexSuffix)
}

// foldSessionJournal folds an entire journal's bytes. It is the refold path
// — the one ReadSessionIndex takes whenever a stored index is missing or
// stale — and it applies scanLog's corruption discipline unchanged: a
// corrupt final line is a crash mid-write and ends the fold silently,
// corruption anywhere else is an error.
func foldSessionJournal(data []byte, modTime time.Time) (SessionIndex, error) {
	var f indexFold
	err := scanLog(data, func(rec indexRecord, line int, isLast bool) error {
		if err := f.applyIndexRecord(rec, isLast); err != nil {
			return fmt.Errorf("%w at line %d", err, line)
		}
		return nil
	})
	if err != nil {
		return SessionIndex{}, err
	}
	ix, ok := f.snapshot(int64(len(data)), modTime)
	if !ok {
		return SessionIndex{}, errNotSessionJournal
	}
	return ix, nil
}

// ReadSessionIndex returns id's summary index, reading the journal only
// when the stored sidecar does not already cover it.
//
// The fast path is one stat plus one small read: a sidecar whose LogSize
// equals the journal's current size is returned as it stands, and the
// journal is never opened. Every other case — no sidecar, an unreadable or
// torn one, a different format version, a journal that grew since (records
// this process did not write, or a write whose sidecar flush failed), or a
// journal that SHRANK (ensureLog's torn-tail repair) — refolds from byte 0
// and writes the result back, so the next read takes the fast path.
//
// The write-back is best effort. A read-only session directory, or a
// racing writer, costs a refold on the next read and nothing else.
func ReadSessionIndex(dir, id string) (SessionIndex, error) {
	if dir == "" {
		return SessionIndex{}, errors.New("engine: ReadSessionIndex requires a session dir")
	}
	// The id reaches the filesystem through sessionPath below, so it is
	// validated here rather than trusted — the same defense in depth
	// LoadSession applies, for the callers (the CLI's resume flags) that
	// never pass through an HTTP boundary.
	if !ValidSessionID(id) {
		return SessionIndex{}, fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	return readSessionIndexAt(dir, id)
}

// errNotSessionJournal marks a .jsonl file in the session directory that is
// not a session log at all — the server's own event journal (events.jsonl)
// is the in-tree example. It is a skip signal for ListSessionIndexes, never
// a failure.
var errNotSessionJournal = errors.New("engine: not a session journal")

// readSessionIndexAt is ReadSessionIndex without the id validation, for
// ListSessionIndexes, whose ids come from directory entries (which cannot
// contain a path separator) rather than from a caller.
func readSessionIndexAt(dir, id string) (SessionIndex, error) {
	path := sessionPath(dir, id)
	fi, err := os.Stat(path)
	if err != nil {
		return SessionIndex{}, err
	}
	if ix, ok := readStoredIndex(dir, id, fi.Size(), fi.ModTime()); ok {
		return ix, nil
	}
	// Refold. Check the first record before reading the whole file: a
	// directory holds one session journal per session AND the server's
	// event journal, which can be megabytes and never yields a sidecar, so
	// a listing must not read it end to end every time.
	if err := checkSessionJournalHead(path); err != nil {
		return SessionIndex{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionIndex{}, err
	}
	// Re-stat AFTER the read, and key the index on that: a journal the
	// writer grew between the stat above and this read would otherwise be
	// summarized under a modification time older than its own bytes. The
	// worst case now is a key that looks stale on the next read, which
	// costs one refold.
	modTime := fi.ModTime()
	if after, err := os.Stat(path); err == nil {
		modTime = after.ModTime()
	}
	ix, err := foldSessionJournal(data, modTime)
	if err != nil {
		return SessionIndex{}, fmt.Errorf("engine: session %s: %w", id, err)
	}
	// The FILENAME names the session, not the header record inside it.
	// LoadSession pins the same way (it assigns s.ID = id before replaying),
	// so a journal copied to a new name reports the new name on both paths.
	// Without this, a header that disagrees with its filename would make
	// GET /session/{id} answer with a different id than it was asked about,
	// and every later read would refold, since readStoredIndex requires the
	// stored id to match.
	ix.ID = id
	writeSessionIndex(dir, id, ix)
	return ix, nil
}

// journalHeadPeekBytes bounds checkSessionJournalHead's read. It is far
// larger than an ordinary session header and far smaller than a journal, so
// the peek stays O(1) against the file it exists to avoid reading.
const journalHeadPeekBytes = 64 << 10

// checkSessionJournalHead reports whether a file's FIRST record is a
// session header, reading only that first line. It answers the same
// question the fold answers (see indexFold.header), at O(1) cost instead of
// the file's whole length. A first line too long to peek at is not a
// verdict: it returns nil, and the caller reads the file.
func checkSessionJournalHead(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, journalHeadPeekBytes).ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		// A first line longer than the peek buffer. A session header CAN be
		// long: ensureLog writes task_tool_names into it, and a large
		// restricted tool set has no fixed bound. Report nothing rather
		// than a verdict — the caller reads the whole file and the fold
		// decides. Skipping here instead would hide a loadable session from
		// GET /session/{id} and from every listing.
		return nil
	}
	if err != nil && len(line) == 0 {
		return errNotSessionJournal
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &head) != nil || head.Type != recSession {
		return errNotSessionJournal
	}
	return nil
}

// sessionIndexFile is the sidecar's on-disk shape: the index bytes, plus a
// checksum over exactly those bytes.
//
// The checksum is what makes a torn read detectable. Both writers replace
// the sidecar in place, so a reader in another process can, in principle,
// read part of the old file and part of the new one. Such a mix can parse
// as JSON — the fields are the same and in the same order — and can even
// carry a staleness key that matches the journal. A checksum over the exact
// bytes it covers turns that into a miss, and a miss refolds.
//
// CRC-32 detects that corruption; it does not prove its absence. A mix that
// happens to collide passes. The residual is a cache read, under a
// single-writer contract, of a file this package alone writes — not a
// trust boundary — so a wider hash would buy nothing an operator can use.
type sessionIndexFile struct {
	CRC32 uint32          `json:"crc32"`
	Index json.RawMessage `json:"index"`
}

// readStoredIndex loads the sidecar and reports whether it is usable AND
// still describes a journal of exactly logSize bytes last modified at
// modTime. Every failure — absent, unreadable, malformed, checksum
// mismatch, wrong version, wrong length, wrong time — returns false, which
// means "refold": the sidecar is a cache with no repair path.
func readStoredIndex(dir, id string, logSize int64, modTime time.Time) (SessionIndex, bool) {
	data, err := os.ReadFile(sessionIndexPath(dir, id))
	if err != nil {
		return SessionIndex{}, false
	}
	var file sessionIndexFile
	if err := json.Unmarshal(data, &file); err != nil {
		return SessionIndex{}, false
	}
	if crc32.ChecksumIEEE(file.Index) != file.CRC32 {
		return SessionIndex{}, false
	}
	var ix SessionIndex
	if err := json.Unmarshal(file.Index, &ix); err != nil {
		return SessionIndex{}, false
	}
	if ix.Version != sessionIndexVersion || ix.ID != id {
		return SessionIndex{}, false
	}
	if ix.LogSize != logSize || !ix.LogModTime.Equal(modTime) {
		return SessionIndex{}, false
	}
	return ix, true
}

// marshalSessionIndex renders an index as its sidecar bytes, checksum and
// all.
func marshalSessionIndex(ix SessionIndex) ([]byte, error) {
	inner, err := json.Marshal(ix)
	if err != nil {
		return nil, err
	}
	return json.Marshal(sessionIndexFile{CRC32: crc32.ChecksumIEEE(inner), Index: inner})
}

// writeSessionIndex replaces id's sidecar, from a caller that holds no open
// handle on it (ReadSessionIndex's write-back).
//
// It never fsyncs. The index is a cache: losing an unsynced sidecar in a
// crash costs one refold, and fsync is exactly the call some FUSE/9p
// transports deadlock on (see Config.SessionSync).
func writeSessionIndex(dir, id string, ix SessionIndex) error {
	b, err := marshalSessionIndex(ix)
	if err != nil {
		return err
	}
	return os.WriteFile(sessionIndexPath(dir, id), b, 0o644)
}

// writeIndexTo rewrites an already-open sidecar in place: truncate to zero,
// then write from offset 0. Session.flushIndexLocked takes this path, once
// per journal record, through a handle opened beside the journal itself
// (ensureLog).
//
// In place, not a temporary file and a rename. A rename publishes
// atomically, but it creates a directory entry per write, and a write that
// races a directory removal resurrects an entry the remover already passed.
// The handle here survives an unlinked file exactly like the journal handle
// beside it. A reader that catches a rewrite mid-flight is answered by the
// sidecar's checksum instead (see sessionIndexFile), which is a stronger
// guard than atomic publication alone: it also catches a mixed read of two
// same-length indexes.
func writeIndexTo(f *os.File, ix SessionIndex) error {
	b, err := marshalSessionIndex(ix)
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt(b, 0)
	return err
}

// ListSessionIndexes returns the index of every persisted session in dir,
// sorted by creation time — the read behind GET /session. A missing
// directory yields an empty list, not an error, and a file that is
// unreadable, corrupt, or not a session journal at all (events.jsonl, the
// server's own event journal, lives in the same directory) is skipped
// rather than failing the whole listing.
func ListSessionIndexes(dir string) ([]SessionIndex, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SessionIndex
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		// The id comes from the directory entry, so it names this exact
		// file; sessionIndexSuffix keeps a sidecar from ever matching the
		// ".jsonl" test above.
		ix, err := readSessionIndexAt(dir, strings.TrimSuffix(e.Name(), ".jsonl"))
		if err != nil {
			continue
		}
		out = append(out, ix)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
