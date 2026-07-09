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
)

// Record types, one JSON object per line.
const (
	recSession      = "session"
	recMessage      = "message"
	recModel        = "model"
	recGoalSet      = "goal.set"
	recGoalEval     = "goal.eval"
	recGoalAchieved = "goal.achieved"
	recGoalCleared  = "goal.cleared"
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
	WorkDir string           `json:"workdir,omitempty"`
	Message *message.Message `json:"message,omitempty"`
	Model   message.ModelRef `json:"model,omitzero"`
	Goal    *goalRecord      `json:"goal,omitempty"`
}

// goalRecord carries the durable payload of a goal.* record (see goal.go).
type goalRecord struct {
	Condition string `json:"condition,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Met       bool   `json:"met,omitempty"`
	Turn      int    `json:"turn,omitempty"`
	Turns     int    `json:"turns,omitempty"`
}

// SessionInfo summarizes one persisted session for listings.
type SessionInfo struct {
	ID        string
	CreatedAt time.Time
	Messages  int
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

// persistMessage appends a message record to the session log. Caller holds
// s.mu.
func (s *Session) persistMessage(m *message.Message) {
	if s.cfg.SessionDir == "" {
		return
	}
	if err := s.ensureLog(); err != nil {
		s.lastPersistErr = err
		return
	}
	if err := s.writeRecord(record{Type: recMessage, Message: m}); err != nil {
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
			{Type: recSession, ID: s.ID, CreatedAt: s.createdAt, WorkDir: s.cfg.WorkDir},
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
		case recMessage:
			if rec.Message == nil {
				if isLast {
					return nil
				}
				return fmt.Errorf("message record without message at line %d", line)
			}
			s.history = append(s.history, *rec.Message)
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
		case recGoalAchieved, recGoalCleared:
			s.goalActive = false
			s.goalCondition = ""
		case recGoalEval:
			// Per-turn evaluation trace; no resume state (counters reset).
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("engine: session %s: %w", id, err)
	}
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
	// bodies, which keeps ListSessions cheap on large sessions.
	type headRecord struct {
		Type      string    `json:"type"`
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
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
