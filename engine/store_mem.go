package engine

import (
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"sync"
)

// MemStore is an in-memory SessionStore. It copies every buffer that crosses
// its boundary, so a caller can never change stored data.
type MemStore struct {
	mu    sync.Mutex
	logs  map[string][][]byte
	blobs map[string]map[string][]byte
}

func NewMemStore() *MemStore {
	return &MemStore{logs: map[string][][]byte{}, blobs: map[string]map[string][]byte{}}
}

func (m *MemStore) Append(id string, at int, records ...[]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.logs[id]); n != at {
		return fmt.Errorf("%w: %s holds %d records, append at %d", ErrAppendConflict, id, n, at)
	}
	for _, r := range records {
		m.logs[id] = append(m.logs[id], slices.Clone(r))
	}
	return nil
}

func (m *MemStore) Sync(string) error { return nil }

func (m *MemStore) Load(id string) ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	log, ok := m.logs[id]
	if !ok {
		return nil, fs.ErrNotExist
	}
	out := make([][]byte, len(log))
	for i, r := range log {
		out[i] = slices.Clone(r)
	}
	return out, nil
}

func (m *MemStore) Len(id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.logs[id]), nil
}

func (m *MemStore) Header(id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	log := m.logs[id]
	if len(log) == 0 {
		return nil, fs.ErrNotExist
	}
	return slices.Clone(log[0]), nil
}

func (m *MemStore) List() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.logs))
	for id, log := range m.logs {
		if len(log) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (m *MemStore) PutBlob(id, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blobs[id] == nil {
		m.blobs[id] = map[string][]byte{}
	}
	m.blobs[id][name] = slices.Clone(data)
	return nil
}

func (m *MemStore) GetBlob(id, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[id][name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return slices.Clone(b), nil
}

func (m *MemStore) Release(string) {}
