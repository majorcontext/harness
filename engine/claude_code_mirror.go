package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errCLIMirror = errors.New("engine: claude-code: transcript mirror failed")

const cliLogSuffix = ".claude-code"

func cliLogID(id string) string {
	return id + cliLogSuffix
}

type claudeCodeMirrorFrame struct {
	FilePath string
	Entries  []json.RawMessage
}

type cliMirrorPath struct {
	Path string `json:"path"`
}

// cliMirror owns the position of one session's mirror log. The backend
// appends to that log directly, so the position is not the journal's.
type cliMirror struct {
	store   SessionStore
	id      string
	scratch string
	pos     int
	hasPath bool
	fence   func(error)

	drainDone chan struct{}
	drainErr  error
}

func newCLIMirror(store SessionStore, id, scratch string, fence func(error)) *cliMirror {
	return &cliMirror{store: store, id: id, scratch: scratch, fence: fence}
}

func (m *cliMirror) fail(err error) error {
	if errors.Is(err, ErrAppendConflict) && m.fence != nil {
		m.fence(err)
	}
	return fmt.Errorf("%w: session %s: %w", errCLIMirror, m.id, err)
}

// startDrain keeps reading stdout after the result event: the CLI sends its
// last transcript frames after it. The caller joins with waitDrain.
func (m *cliMirror) startDrain(scanner *bufio.Scanner) {
	m.drainDone = make(chan struct{})
	go func() {
		defer close(m.drainDone)
		for scanner.Scan() {
			var env claudeCodeEnvelope
			if json.Unmarshal(scanner.Bytes(), &env) != nil || env.Type != "transcript_mirror" {
				continue
			}
			if err := m.append(claudeCodeMirrorFrame{FilePath: env.FilePath, Entries: env.Entries}); err != nil {
				m.drainErr = err
				return
			}
		}
	}()
}

// waitDrain waits for the drain to see EOF, at most grace. It returns false
// on timeout; the caller must then close the pipe and call joinDrain.
func (m *cliMirror) waitDrain(grace time.Duration) bool {
	if m.drainDone == nil {
		return true
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-m.drainDone:
		return true
	case <-t.C:
		return false
	}
}

func (m *cliMirror) joinDrain() error {
	if m.drainDone != nil {
		<-m.drainDone
	}
	return m.drainErr
}

// restore writes the stored CLI transcript under the scratch directory. ok is
// false when no transcript exists yet.
func (m *cliMirror) restore() (cliSessionID string, ok bool, err error) {
	records, err := m.store.Load(cliLogID(m.id))
	if errors.Is(err, fs.ErrNotExist) {
		n, lerr := m.store.Len(cliLogID(m.id))
		if lerr != nil {
			return "", false, m.fail(lerr)
		}
		m.pos = n
		return "", false, nil
	}
	if err != nil {
		return "", false, m.fail(err)
	}
	m.pos = len(records)
	if len(records) == 0 {
		return "", false, nil
	}
	var first cliMirrorPath
	if err := json.Unmarshal(records[0], &first); err != nil || first.Path == "" || !filepath.IsLocal(first.Path) {
		return "", false, m.fail(errors.New("first record is not a valid path record"))
	}
	m.hasPath = true
	var buf bytes.Buffer
	for _, r := range records[1:] {
		buf.Write(r)
		buf.WriteByte('\n')
	}
	dest := filepath.Join(m.scratch, first.Path)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", false, m.fail(err)
	}
	if err := os.WriteFile(dest, buf.Bytes(), 0o600); err != nil {
		return "", false, m.fail(err)
	}
	return strings.TrimSuffix(filepath.Base(first.Path), ".jsonl"), true, nil
}

// append stores one frame's entries, one record each. The first frame also
// stores the path record.
func (m *cliMirror) append(frame claudeCodeMirrorFrame) error {
	if len(frame.Entries) == 0 {
		return nil
	}
	var records [][]byte
	if !m.hasPath {
		rel, err := filepath.Rel(m.scratch, frame.FilePath)
		if err != nil || !filepath.IsLocal(rel) {
			return m.fail(fmt.Errorf("transcript path %q is outside the scratch config dir", frame.FilePath))
		}
		b, err := json.Marshal(cliMirrorPath{Path: rel})
		if err != nil {
			return m.fail(err)
		}
		records = append(records, b)
	}
	for _, e := range frame.Entries {
		var buf bytes.Buffer
		if err := json.Compact(&buf, e); err != nil {
			return m.fail(err)
		}
		records = append(records, buf.Bytes())
	}
	if err := m.store.Append(cliLogID(m.id), m.pos, records...); err != nil {
		return m.fail(err)
	}
	m.pos += len(records)
	m.hasPath = true
	return nil
}

func (m *cliMirror) env() string {
	return "CLAUDE_CONFIG_DIR=" + m.scratch
}
