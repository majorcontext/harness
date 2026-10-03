package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"sort"
	"sync"
)

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu       sync.Mutex
	sessions map[string]*memSession
}

type memSession struct {
	records [][]byte
	blobs   map[string][]byte
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{sessions: map[string]*memSession{}}
}

func (m *MemStore) Append(ctx context.Context, session string, expectedSeq uint64, records ...[]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkName("session", session); err != nil {
		return err
	}
	if err := checkRecords(records); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.session(session)
	if n := uint64(len(s.records)); n != expectedSeq {
		return fmt.Errorf("%w: session %q has %d records, append at %d", ErrConflict, session, n, expectedSeq)
	}
	for _, r := range records {
		s.records = append(s.records, slices.Clone(r))
	}
	return nil
}

func (m *MemStore) session(id string) *memSession {
	s := m.sessions[id]
	if s == nil {
		s = &memSession{blobs: map[string][]byte{}}
		m.sessions[id] = s
	}
	return s
}

func (m *MemStore) Read(ctx context.Context, session string, afterSeq uint64, limit int) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	if s := m.sessions[session]; s != nil {
		for i := afterSeq; i < uint64(len(s.records)) && len(out) < limit; i++ {
			out = append(out, Record{Seq: i + 1, Data: slices.Clone(s.records[i])})
		}
	}
	return out, nil
}

func (m *MemStore) Head(ctx context.Context, session string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[session]; s != nil {
		return uint64(len(s.records)), nil
	}
	return 0, nil
}

func (m *MemStore) Sessions(ctx context.Context, after string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, s := range m.sessions {
		if id > after && len(s.records) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids[:max(0, min(limit, len(ids)))], nil
}

func (m *MemStore) PutBlob(ctx context.Context, session, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkName("session", session); err != nil {
		return err
	}
	if err := checkName("blob key", key); err != nil {
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.session(session).blobs[key] = b
	return nil
}

func (m *MemStore) GetBlob(ctx context.Context, session, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[session]; s != nil {
		if b, ok := s.blobs[key]; ok {
			return io.NopCloser(bytes.NewReader(slices.Clone(b))), nil
		}
	}
	return nil, fmt.Errorf("harness: blob %q of session %q: %w", key, session, fs.ErrNotExist)
}
