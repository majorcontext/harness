package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"sort"
	"sync"

	"github.com/majorcontext/harness"
)

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu       sync.Mutex
	sessions map[string]*memSession
}

type memSession struct {
	records        [][]byte
	blobs          map[string][]byte
	checkpointSeq  uint64
	checkpointData []byte
	checkpointSet  bool
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{sessions: map[string]*memSession{}}
}

// Append implements Store.
func (m *MemStore) Append(ctx context.Context, session string, expectedSeq uint64, records ...[]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckName(session); err != nil {
		return err
	}
	if err := CheckRecords(records); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var have uint64
	if s := m.sessions[session]; s != nil {
		have = uint64(len(s.records))
	}
	if have != expectedSeq {
		return fmt.Errorf("%w: session %q has %d records, append at %d", harness.ErrConflict, session, have, expectedSeq)
	}
	s := m.session(session)
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

// Read implements Store.
func (m *MemStore) Read(ctx context.Context, session string, afterSeq uint64, limit int) ([]harness.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckName(session); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []harness.Record
	if s := m.sessions[session]; s != nil {
		for i := afterSeq; i < uint64(len(s.records)) && len(out) < limit; i++ {
			out = append(out, harness.Record{Seq: i + 1, Data: slices.Clone(s.records[i])})
		}
	}
	return out, nil
}

// ReadSeqs returns the existing records for the requested sequences in ascending order.
func (m *MemStore) ReadSeqs(ctx context.Context, session string, seqs []uint64) ([]harness.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckName(session); err != nil {
		return nil, err
	}
	seqs = slices.Clone(seqs)
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []harness.Record
	if s := m.sessions[session]; s != nil {
		for _, seq := range seqs {
			if seq > 0 && seq <= uint64(len(s.records)) {
				out = append(out, harness.Record{Seq: seq, Data: slices.Clone(s.records[seq-1])})
			}
		}
	}
	return out, nil
}

// LoadCheckpoint returns the latest checkpoint, or zero and nil when absent.
func (m *MemStore) LoadCheckpoint(ctx context.Context, session string) (seq uint64, data []byte, err error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if err := CheckName(session); err != nil {
		return 0, nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[session]; s != nil && s.checkpointSet {
		return s.checkpointSeq, slices.Clone(s.checkpointData), nil
	}
	return 0, nil, nil
}

// SaveCheckpoint stores a checkpoint if its sequence is at least the saved sequence.
func (m *MemStore) SaveCheckpoint(ctx context.Context, session string, seq uint64, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckName(session); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.session(session)
	if !s.checkpointSet || seq >= s.checkpointSeq {
		s.checkpointSeq = seq
		s.checkpointData = slices.Clone(data)
		s.checkpointSet = true
	}
	return nil
}

// Head implements Store.
func (m *MemStore) Head(ctx context.Context, session string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := CheckName(session); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[session]; s != nil {
		return uint64(len(s.records)), nil
	}
	return 0, nil
}

// Sessions implements Store.
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

// PutBlob implements Store.
func (m *MemStore) PutBlob(ctx context.Context, session, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckName(session); err != nil {
		return err
	}
	if err := CheckName(key); err != nil {
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

// GetBlob implements Store.
func (m *MemStore) GetBlob(ctx context.Context, session, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckName(session); err != nil {
		return nil, err
	}
	if err := CheckName(key); err != nil {
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
