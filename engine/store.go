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
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	recSession = "session"
	recMessage = "message"
	recModel   = "model"
)

// record is one line of a session log file.
type record struct {
	Type      string           `json:"type"`
	ID        string           `json:"id,omitempty"`
	CreatedAt time.Time        `json:"created_at,omitzero"`
	Message   *message.Message `json:"message,omitempty"`
	Model     message.ModelRef `json:"model,omitzero"`
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
		if err := s.writeRecord(record{Type: recSession, ID: s.ID, CreatedAt: s.createdAt}); err != nil {
			f.Close()
			s.logFile = nil
			return err
		}
		if err := s.writeRecord(record{Type: recModel, Model: s.model}); err != nil {
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
	data, err := os.ReadFile(sessionPath(cfg.SessionDir, id))
	if err != nil {
		return nil, err
	}

	s := newSession(cfg)
	s.ID = id
	s.logStarted = true

	lines := bytes.Split(data, []byte("\n"))
	last := len(lines) - 1
	for last >= 0 && len(bytes.TrimSpace(lines[last])) == 0 {
		last--
	}
	for i, line := range lines[:last+1] {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			if i == last {
				break // truncated final line: crash mid-write, ignore
			}
			return nil, fmt.Errorf("engine: session %s: corrupt record at line %d: %v", id, i+1, err)
		}
		switch rec.Type {
		case recSession:
			s.createdAt = rec.CreatedAt
		case recMessage:
			if rec.Message == nil {
				if i == last {
					break
				}
				return nil, fmt.Errorf("engine: session %s: message record without message at line %d", id, i+1)
			}
			s.history = append(s.history, *rec.Message)
		case recModel:
			s.model = rec.Model
		}
	}
	return s, nil
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
	f, err := os.Open(path)
	if err != nil {
		return SessionInfo{}, err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	var info SessionInfo
	first := true
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var rec struct {
				Type      string    `json:"type"`
				ID        string    `json:"id"`
				CreatedAt time.Time `json:"created_at"`
			}
			if uerr := json.Unmarshal(line, &rec); uerr != nil {
				if first || err == nil {
					return SessionInfo{}, uerr
				}
				// truncated final line: ignore
			} else {
				if first {
					if rec.Type != recSession {
						return SessionInfo{}, fmt.Errorf("engine: %s: missing session header", path)
					}
					info.ID = rec.ID
					info.CreatedAt = rec.CreatedAt
					first = false
				} else if rec.Type == recMessage {
					info.Messages++
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return SessionInfo{}, err
		}
	}
	if first {
		return SessionInfo{}, fmt.Errorf("engine: %s: empty session file", path)
	}
	return info, nil
}
