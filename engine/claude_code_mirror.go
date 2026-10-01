package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
}

func newCLIMirror(store SessionStore, id, scratch string) *cliMirror {
	return &cliMirror{store: store, id: id, scratch: scratch}
}

func (m *cliMirror) fail(err error) error {
	return fmt.Errorf("%w: session %s: %w", errCLIMirror, m.id, err)
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
