package external

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Mirror is the state of one external session: its ID and the transcript
// that the external harness mirrors as it writes it. A backend state holds it:
// the fields as its head and the entries as its entries.
type Mirror struct {
	SessionID string
	// Turn is the harness turn whose input the external session holds.
	Turn string
	// Parked is the tool call that the external session waits on for an answer.
	Parked string
	// Path is the transcript file, relative to the config directory.
	Path    string
	Entries []json.RawMessage
}

type mirrorHead struct {
	SessionID string `json:"session_id,omitempty"`
	Turn      string `json:"turn,omitempty"`
	Parked    string `json:"parked,omitempty"`
	Path      string `json:"path,omitempty"`
}

// MirrorOf returns the Mirror of a saved head and entries. An empty head is
// an empty Mirror.
func MirrorOf(head json.RawMessage, entries []json.RawMessage) (Mirror, error) {
	var h mirrorHead
	if len(head) > 0 {
		if err := json.Unmarshal(head, &h); err != nil {
			return Mirror{}, fmt.Errorf("external: mirror head: %w", err)
		}
	}
	if h.Path != "" && !filepath.IsLocal(h.Path) {
		return Mirror{}, fmt.Errorf("external: mirror path %q is not local", h.Path)
	}
	return Mirror{SessionID: h.SessionID, Turn: h.Turn, Parked: h.Parked, Path: h.Path, Entries: entries}, nil
}

// Head returns the fields of m other than its entries as a JSON object.
func (m Mirror) Head() (json.RawMessage, error) {
	return json.Marshal(mirrorHead{SessionID: m.SessionID, Turn: m.Turn, Parked: m.Parked, Path: m.Path})
}

// Encode returns the one-blob form of m: a head line, then one line per entry.
func (m Mirror) Encode() ([]byte, error) {
	var buf bytes.Buffer
	head, err := m.Head()
	if err != nil {
		return nil, err
	}
	buf.Write(head)
	buf.WriteByte('\n')
	for _, e := range m.Entries {
		if err := json.Compact(&buf, e); err != nil {
			return nil, err
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// Add appends the entries of one mirror frame for the file at path, which
// must be inside dir. Only the first file is kept: a resume reads the
// transcript of the main session alone.
func (m *Mirror) Add(dir, path string, entries []json.RawMessage) error {
	rel, err := filepath.Rel(dir, path)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("external: transcript %q is outside %q", path, dir)
	}
	if m.Path != "" && m.Path != rel {
		return nil
	}
	m.Path = rel
	m.Entries = append(m.Entries, entries...)
	return nil
}

// Restore writes the transcript under dir.
func (m Mirror) Restore(dir string) error {
	if m.Path == "" {
		return nil
	}
	var buf bytes.Buffer
	for _, e := range m.Entries {
		buf.Write(e)
		buf.WriteByte('\n')
	}
	dest := filepath.Join(dir, m.Path)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dest, buf.Bytes(), 0o600)
}
