// Session persistence: an append-only JSONL log, one file per session at
// <SessionDir>/<session-id>.jsonl. Each line is one record: a "session"
// header (always followed by a "model" record naming the session's model at
// creation), a "message" for every appended message (canonical message
// JSON), or a "model" written when SetModel changes the model.
//
// Nothing touches disk until the first message append (startup budget rule);
// the directory and file are created lazily on first write.

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// Record types, one JSON object per line.
const (
	recSession      = "session"
	recMessage      = "message"
	recModel        = "model"
	recGoalSet      = "goal.set"
	recGoalUpdated  = "goal.updated"
	recGoalEval     = "goal.eval"
	recGoalStalled  = "goal.stalled"
	recGoalAchieved = "goal.achieved"
	recGoalCleared  = "goal.cleared"
	// recGoalEvalFailed is one failed evaluator boundary (see goal.go's
	// "Round 6" doc section): a provider error the in-boundary retryable
	// retry couldn't ride out, or two consecutive unparseable replies. Like
	// recGoalStalled it is a pure trace record — it never by itself changes
	// goalActive (see LoadSession's fold below); only a later goal.cleared
	// (the terminal horizon) or goal.eval/goal.achieved (a recovered
	// boundary) does that.
	recGoalEvalFailed = "goal.eval_failed"
	// recPromptQueued/recPromptDequeued are the prompt-queue records (see
	// queue.go and docs/plans/2026-07-19-prompt-queue.md): one prompt.queued
	// per EnqueuePrompt call, one prompt.dequeued per pop (whatever the
	// reason — delivered/injected/cleared). Queued text never becomes a
	// recMessage until delivered, so these are the only durable trace of a
	// pending prompt.
	recPromptQueued   = "prompt.queued"
	recPromptDequeued = "prompt.dequeued"
	// recCompact is the compaction record (see compact.go and docs/design/
	// context-compaction.md §2 "Journal shape"): one per successful
	// Session.Compact call, carrying the full summary message inline (not
	// a separate recMessage) and the summarization call's own Usage.
	recCompact = "compact"
)

// record is one line of a session log file.
type record struct {
	Type      string    `json:"type"`
	ID        string    `json:"id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	// WorkDir carries Config.WorkDir on the session header record only. It is
	// omitted (and so absent from every record written before this field
	// existed) when empty, which is also how LoadSession recognizes a legacy
	// header with nothing to restore.
	WorkDir string `json:"workdir,omitempty"`
	// ParentSession carries Config.ParentSession on the session header
	// record only, same rule as WorkDir: omitted when empty, and an empty
	// value on load means "nothing to restore" (a legacy header, or a
	// session created with no lineage) rather than "the caller's Config.
	// ParentSession should be cleared".
	ParentSession string           `json:"parent_session,omitempty"`
	Message       *message.Message `json:"message,omitempty"`
	Model         message.ModelRef `json:"model,omitzero"`
	Goal          *goalRecord      `json:"goal,omitempty"`
	// Prompt carries a prompt.queued/prompt.dequeued record's payload (see
	// promptRecord and queue.go). nil on every other record type.
	Prompt *promptRecord `json:"prompt,omitempty"`
	// Usage carries the provider's per-turn Usage on the message record for
	// the assistant message ending a model turn (nil for every other
	// message: user, tool, or an interrupted partial assistant message —
	// see Session.appendWithUsage). It is the only way Session.Usage() and
	// Session.LastUsage() survive a process restart: LoadSession sums every
	// record's Usage back into cumulative usage and keeps the last one seen
	// (see issue #62 layer 2) — the log carries no separate cumulative-
	// usage record to replay instead.
	//
	// On a recCompact record, Usage instead carries the SUMMARIZATION
	// call's own spend (see compact.go's Session.Compact and docs/design/
	// context-compaction.md's "Usage accounting"): LoadSession's replay
	// adds it into cumulative usage ONLY, never into lastUsage/
	// haveLastUsage — unlike recMessage replay, which sets both. A
	// reloaded session must not report the small summarization call as its
	// "last request size", or the automatic trigger's re-fire check would
	// misread the session as small right after a reload.
	Usage *provider.Usage `json:"usage,omitempty"`
	// Compact carries a recCompact record's payload (see compactRecord).
	// nil on every other record type.
	Compact *compactRecord `json:"compact,omitempty"`
}

// compactRecord carries the durable payload of a "compact" record (see
// compact.go's Session.Compact and docs/design/context-compaction.md §2
// "Journal shape"). Summary is the full message.Message to splice in,
// carried inline — not a lightweight marker followed by a separate
// recMessage — so a crash between two records can never leave a dangling
// reference (see §3 "Crash discipline").
type compactRecord struct {
	FirstID     string          `json:"first_id"`
	LastID      string          `json:"last_id"`
	TurnsFolded int             `json:"turns_folded"`
	Summary     message.Message `json:"summary"`
}

// goalRecord carries the durable payload of a goal.* record (see goal.go).
type goalRecord struct {
	Condition string `json:"condition,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Met       bool   `json:"met,omitempty"`
	Turn      int    `json:"turn,omitempty"`
	Turns     int    `json:"turns,omitempty"`
	// Attempt is the 1-based worker-turn retry attempt on a goal.stalled
	// record (see promptTurnWithRetry in goal.go).
	Attempt int `json:"attempt,omitempty"`
	// Retryable marks a goal.stalled record whose failure was classified as
	// provider-retryable weather (see provider.RetryableError and GitHub
	// issue #61) rather than a deterministic failure — set on every
	// retryable-class stalled record, including the final one recorded when
	// promptTurnWithRetry's retryable budget (goalRetryableMaxAttempts) is
	// exhausted. RetryableClass names the provider's classification
	// (overloaded/rate_limited/server_error, see provider.RetryableClass).
	// Waiting is true for every retryable-class stall EXCEPT that final
	// exhausted one, so a reader distinguishes "still waiting out provider
	// weather" from "gave up waiting and is parking the turn" without
	// decoding Reason text. Both are false/empty on an ordinary
	// deterministic-path stall, unchanged from before this field existed.
	Retryable      bool   `json:"retryable,omitempty"`
	RetryableClass string `json:"retryable_class,omitempty"`
	Waiting        bool   `json:"waiting,omitempty"`
	// EvalFailures carries a goal.eval_failed record's consecutive-failure
	// count (see goal.go's recordGoalEvalFailed and "Round 6" doc section):
	// the number of CONSECUTIVE failed evaluator boundaries as of this one,
	// inclusive, reset to zero the moment a later boundary parses a verdict.
	// It also names the count on the terminal goal.cleared record that
	// fires once this reaches goalEvalFailureLimit.
	EvalFailures int `json:"eval_failures,omitempty"`
}

// promptRecord carries the durable payload of a prompt.queued/
// prompt.dequeued record (see queue.go). String-only, mirroring goalRecord —
// v1's prompt contract is text parts only (see AGENTS.md), so no attachment
// machinery is needed here. ID is the queue-assigned, session-monotonic
// prompt ID. Text is the queued prompt, carried on BOTH record types (not
// just prompt.queued) so a prompt.dequeued record is self-describing without
// cross-referencing the matching prompt.queued one earlier in the log.
// Reason is empty on prompt.queued and one of "delivered"/"injected"/
// "cleared" on prompt.dequeued (see DequeuePrompt/dequeueAllLocked).
type promptRecord struct {
	ID     int64  `json:"id,omitempty"`
	Text   string `json:"text,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// SessionInfo summarizes one persisted session for listings.
type SessionInfo struct {
	ID        string
	CreatedAt time.Time
	Messages  int
	// Usage is cumulative token usage summed from every message record's
	// optional Usage (see record.Usage, persistMessage), computed by the
	// same cheap header-only scan that counts Messages — no full
	// LoadSession/message.Message replay required (issue #62 layer 2:
	// GET /session/status needs this without paying for a full session
	// load per entry).
	Usage provider.Usage
	// LastInputTokens is the input-token count of the most recent message
	// record carrying Usage (0 if none do).
	LastInputTokens int
}

func sessionPath(dir, id string) string {
	return filepath.Join(dir, id+".jsonl")
}

// PersistErr returns the most recent persistence failure, or nil. Write
// errors never crash the agent loop; callers decide what to do with them.
func (s *Session) PersistErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPersistErr
}

// Persist forces the session log to exist on disk now (header plus a model
// record), rather than waiting for the first message append. NewSession creates
// the log lazily, so a session that is created but never prompted has no
// on-disk backing; callers that must be able to reload such a session — the
// serve API, which may evict an idle session from memory — call Persist to give
// it durable state. It is a no-op when SessionDir is empty or the log already
// exists, and is safe to call repeatedly.
func (s *Session) Persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.SessionDir == "" {
		return nil
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return err
	}
	return nil
}

// persistMessage appends a message record to the session log, carrying
// usage (nil for every message except the assistant message ending a model
// turn — see appendWithUsage). Caller holds s.mu.
func (s *Session) persistMessage(m *message.Message, usage *provider.Usage) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recMessage, Message: m, Usage: usage}); err != nil {
		s.lastPersistErr = err
	}
}

// persistModel appends a model record to the session log. It is a no-op
// until the log exists (lazy creation: nothing is written before the first
// message append). Caller holds s.mu.
func (s *Session) persistModel(ref message.ModelRef) {
	if s.cfg.SessionDir == "" || !s.logStarted {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recModel, Model: ref}); err != nil {
		s.lastPersistErr = err
	}
}

// persistGoalLocked appends a goal.* record to the session log. It forces the
// log to exist (a goal.set may be the first thing written to a fresh session).
// Caller holds s.mu.
func (s *Session) persistGoalLocked(recType string, g goalRecord) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recType, Goal: &g}); err != nil {
		s.lastPersistErr = err
	}
}

// persistPromptQueueLocked appends a prompt.queued or prompt.dequeued record
// to the session log (see queue.go's EnqueuePrompt/DequeuePrompt). It forces
// the log to exist — a prompt.queued may be the first thing ever written to
// a fresh session — mirroring persistGoalLocked exactly. Caller holds s.mu.
func (s *Session) persistPromptQueueLocked(recType string, p promptRecord) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recType, Prompt: &p}); err != nil {
		s.lastPersistErr = err
	}
}

// persistCompactLocked appends a compact record to the session log: one
// json.Marshal, one Write call, exactly like every other record (see
// docs/design/context-compaction.md §3 "Crash discipline" — a torn write
// degrades to "compaction never happened", never a partially-spliced or
// ambiguous history). Caller holds s.mu and has already spliced s.history.
func (s *Session) persistCompactLocked(firstID, lastID string, turnsFolded int, summary message.Message, usage provider.Usage) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	rec := record{
		Type:      recCompact,
		CreatedAt: summary.CreatedAt,
		Usage:     &usage,
		Compact: &compactRecord{
			FirstID:     firstID,
			LastID:      lastID,
			TurnsFolded: turnsFolded,
			Summary:     summary,
		},
	}
	if err := s.writeRecord(rec); err != nil {
		s.lastPersistErr = err
	}
}

// ensureLog opens the session log, creating the directory and file — and
// writing the header — on first use. Caller holds s.mu.
func (s *Session) ensureLog() error {
	if s.logFile != nil {
		return nil
	}
	if err := os.MkdirAll(s.cfg.SessionDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(sessionPath(s.cfg.SessionDir, s.ID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	s.logFile = f
	if fi.Size() == 0 {
		// Header plus a model record for the session's current model, so
		// every persisted session names its model explicitly — a SetModel
		// before the first append would otherwise be silently lost and
		// LoadSession would wrongly fall back to Config.Model.
		//
		// Both records go out in ONE Write call: written separately, a
		// transient failure after the header would leave a non-empty file
		// that retries (gated on size == 0) never complete, permanently
		// dropping the model record. With a single write the worst case
		// under a mid-write crash is a truncated final line, which
		// LoadSession already tolerates.
		var buf bytes.Buffer
		for _, rec := range []record{
			{Type: recSession, ID: s.ID, CreatedAt: s.createdAt, WorkDir: s.cfg.WorkDir, ParentSession: s.cfg.ParentSession},
			{Type: recModel, Model: s.model},
		} {
			b, err := json.Marshal(rec)
			if err != nil {
				f.Close()
				s.logFile = nil
				return err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		if _, err := f.Write(buf.Bytes()); err != nil {
			f.Close()
			s.logFile = nil
			return err
		}
	}
	s.logStarted = true
	return nil
}

// writeRecord marshals one record and appends it as a line. Caller holds
// s.mu and has called ensureLog.
func (s *Session) writeRecord(rec record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.logFile.Write(append(b, '\n'))
	return err
}

// ErrInvalidSessionID is returned (wrapped) by LoadSession when id fails
// ValidSessionID's two-format rule. It is checked before sessionPath ever
// builds a filesystem path, so a path-traversal-shaped id (e.g.
// "../../etc/passwd") is rejected without touching disk — defense in depth
// alongside the HTTP boundary's own ValidSessionID check (server/handlers.go),
// since not every caller (e.g. the CLI's -r/-c resume flags) goes through
// that boundary.
var ErrInvalidSessionID = errors.New("engine: invalid session id")

// LoadSession rebuilds a session from its log file: history and current
// model (the last model record wins; Config.Model otherwise), preserving the
// session ID. Subsequent appends continue the same file.
//
// A corrupt or incomplete final line (crash mid-write) is ignored; a corrupt
// line anywhere else is an error.
func LoadSession(cfg Config, id string) (*Session, error) {
	if cfg.SessionDir == "" {
		return nil, errors.New("engine: LoadSession requires Config.SessionDir")
	}
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	data, err := os.ReadFile(sessionPath(cfg.SessionDir, id))
	if err != nil {
		return nil, err
	}

	s := newSession(cfg)
	s.ID = id
	s.logStarted = true

	err = scanLog(data, func(rec record, line int, isLast bool) error {
		switch rec.Type {
		case recSession:
			s.createdAt = rec.CreatedAt
			// A restored WorkDir wins over the loading Config.WorkDir: the
			// header is the durable truth for a resumed session. A legacy
			// header (written before this field existed) omits it, so an
			// empty value here means "nothing to restore" — the loading
			// Config.WorkDir is kept unchanged.
			if rec.WorkDir != "" {
				s.cfg.WorkDir = rec.WorkDir
			}
			// Same restore rule as WorkDir above: the header is the durable
			// truth for a resumed session, and an empty value here means
			// nothing to restore (legacy header, or no lineage recorded),
			// never "clear the loading Config's ParentSession".
			if rec.ParentSession != "" {
				s.cfg.ParentSession = rec.ParentSession
			}
		case recMessage:
			if rec.Message == nil {
				if isLast {
					return nil
				}
				return fmt.Errorf("message record without message at line %d", line)
			}
			s.history = append(s.history, *rec.Message)
			// Replay this record's per-turn Usage (if any — see
			// persistMessage) into cumulative usage/lastUsage, exactly as
			// appendWithUsage does live: this is what makes Session.Usage()
			// and LastUsage() survive a process restart (issue #62 layer 2).
			if rec.Usage != nil {
				s.usage.InputTokens += rec.Usage.InputTokens
				s.usage.OutputTokens += rec.Usage.OutputTokens
				s.usage.CacheReadTokens += rec.Usage.CacheReadTokens
				s.usage.CacheWriteTokens += rec.Usage.CacheWriteTokens
				s.lastUsage = *rec.Usage
				s.haveLastUsage = true
			}
		case recModel:
			s.model = rec.Model
		case recGoalSet:
			// An active goal is one set without a later achieved/cleared. The
			// condition is restored; per Claude Code semantics the run counters
			// reset, so nothing else carries over.
			s.goalActive = true
			if rec.Goal != nil {
				s.goalCondition = rec.Goal.Condition
			}
		case recGoalUpdated:
			// Only meaningful while active (see UpdateGoal): rewrites the
			// restored condition in place, same as the live path.
			if s.goalActive && rec.Goal != nil {
				s.goalCondition = rec.Goal.Condition
			}
		case recGoalAchieved, recGoalCleared:
			s.goalActive = false
			s.goalCondition = ""
		case recGoalEval, recGoalStalled, recGoalEvalFailed:
			// Per-turn evaluation/stall/eval-failure trace; no resume state
			// (counters reset). None of these ever change goalActive by
			// itself — either a later record of the same kind follows, or a
			// later goal.cleared/goal.eval/goal.achieved settles it, all
			// handled above.
		case recPromptQueued:
			// Append to the folded queue and advance the next-ID counter past
			// whatever this record used, so a resumed session's next
			// EnqueuePrompt continues the same monotonic sequence instead of
			// colliding with (or repeating) an ID already on disk — even if a
			// prior process crashed right after writing this record without
			// ever incrementing its own in-memory counter further.
			if rec.Prompt != nil {
				s.promptQueue = append(s.promptQueue, QueuedPrompt{ID: rec.Prompt.ID, Text: rec.Prompt.Text})
				if rec.Prompt.ID >= s.promptQueueNextID {
					s.promptQueueNextID = rec.Prompt.ID + 1
				}
			}
		case recPromptDequeued:
			// Remove the matching queued entry (by ID, not position — see
			// promptRecord's doc comment) so the folded queue ends up exactly
			// the undelivered set, in ID order, regardless of how queued and
			// dequeued records interleave in the log.
			if rec.Prompt != nil {
				for i, p := range s.promptQueue {
					if p.ID == rec.Prompt.ID {
						s.promptQueue = append(s.promptQueue[:i], s.promptQueue[i+1:]...)
						break
					}
				}
			}
		case recCompact:
			// See docs/design/context-compaction.md §2 "LoadSession
			// replay": find FirstID/LastID within s.history accumulated so
			// far (guaranteed present, in order, since a compact record can
			// only be written chronologically after those messages were
			// themselves durably appended) and splice — the identical
			// function the live path uses (spliceCompact, compact.go), so
			// the two can never drift apart. Not found is corruption, an
			// explicit error, never a silent best-effort guess.
			if rec.Compact == nil {
				return fmt.Errorf("compact record without payload at line %d", line)
			}
			spliced, err := spliceCompact(s.history, rec.Compact.FirstID, rec.Compact.LastID, rec.Compact.Summary)
			if err != nil {
				return fmt.Errorf("%w at line %d", err, line)
			}
			s.history = spliced
			s.compactCount++
			s.lastCompactedAt = rec.CreatedAt
			// Cumulative usage ONLY (see record.Usage's doc comment above
			// and the "Usage accounting" section of the design doc):
			// lastUsage/haveLastUsage must never be touched by a compact
			// record's usage, or a reload would report the small
			// summarization call as the session's "last request size" and
			// defeat the automatic trigger's re-fire check.
			if rec.Usage != nil {
				s.usage.InputTokens += rec.Usage.InputTokens
				s.usage.OutputTokens += rec.Usage.OutputTokens
				s.usage.CacheReadTokens += rec.Usage.CacheReadTokens
				s.usage.CacheWriteTokens += rec.Usage.CacheWriteTokens
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("engine: session %s: %w", id, err)
	}
	// A log from an older binary or an external writer can carry an
	// assistant tool_call whose turn died before a result was recorded.
	// Repair at ingest — the load-path counterpart of Session.append's
	// Normalize — so every downstream consumer sees a protocol-valid
	// history, not just the transcoders' wire-time backstop. The repair is
	// re-derived deterministically on every load; the log itself stays
	// append-only and unmodified.
	s.history = message.ResolveOrphanToolCalls(s.history)
	return s, nil
}

// scanLog iterates the JSONL records of a session log, decoding each line
// into T and calling fn with the 1-based line number. It owns the log's
// corruption discipline — shared by every reader so the rules cannot drift:
// a corrupt or truncated final line (crash mid-write) ends iteration
// silently; corruption anywhere else is an error.
func scanLog[T any](data []byte, fn func(rec T, line int, isLast bool) error) error {
	lines := bytes.Split(data, []byte("\n"))
	last := len(lines) - 1
	for last >= 0 && len(bytes.TrimSpace(lines[last])) == 0 {
		last--
	}
	for i := range lines[:last+1] {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var rec T
		if err := json.Unmarshal(line, &rec); err != nil {
			if i == last {
				return nil // truncated final line: crash mid-write, ignore
			}
			return fmt.Errorf("corrupt record at line %d: %v", i+1, err)
		}
		if err := fn(rec, i+1, i == last); err != nil {
			return err
		}
	}
	return nil
}

// ListSessions lists persisted sessions in dir, sorted by creation time. A
// missing directory yields an empty list, not an error. Only headers and
// record types are decoded, never message bodies.
func ListSessions(dir string) ([]SessionInfo, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var infos []SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := readSessionInfo(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // unreadable or corrupt header: not listable
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].CreatedAt.Before(infos[j].CreatedAt) })
	return infos, nil
}

func readSessionInfo(path string) (SessionInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionInfo{}, err
	}

	// headRecord decodes only the fields listings need — never message
	// bodies, which keeps ListSessions cheap on large sessions. Usage is a
	// small flat sub-object (sibling to the message body, never nested
	// inside it — see record.Usage), so decoding it here costs nothing
	// like a full message.Message unmarshal would.
	type headRecord struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		CreatedAt time.Time       `json:"created_at"`
		Usage     *provider.Usage `json:"usage,omitempty"`
	}

	var info SessionInfo
	first := true
	err = scanLog(data, func(rec headRecord, line int, isLast bool) error {
		if first {
			if rec.Type != recSession {
				return fmt.Errorf("engine: %s: missing session header", path)
			}
			info.ID = rec.ID
			info.CreatedAt = rec.CreatedAt
			first = false
		} else if rec.Type == recMessage {
			info.Messages++
			if rec.Usage != nil {
				info.Usage.InputTokens += rec.Usage.InputTokens
				info.Usage.OutputTokens += rec.Usage.OutputTokens
				info.Usage.CacheReadTokens += rec.Usage.CacheReadTokens
				info.Usage.CacheWriteTokens += rec.Usage.CacheWriteTokens
				info.LastInputTokens = rec.Usage.InputTokens
			}
		}
		return nil
	})
	if err != nil {
		return SessionInfo{}, err
	}
	if first {
		return SessionInfo{}, fmt.Errorf("engine: %s: empty session file", path)
	}
	return info, nil
}
