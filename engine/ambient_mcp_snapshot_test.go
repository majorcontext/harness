package engine

import "testing"

func TestSnapshotCarriesDelegatedAmbientMCPSourceHashes(t *testing.T) {
	s := NewSession(Config{})
	s.delegatedAmbientMCPSourceHashes = map[string]string{"skills": "hash-1"}
	s.mu.Lock()
	snapshot := s.captureSnapshotLocked()
	s.mu.Unlock()
	loaded := NewSession(Config{})
	loaded.restoreSnapshot(snapshot)
	if got := loaded.delegatedAmbientMCPSourceHashes["skills"]; got != "hash-1" {
		t.Errorf("restored hash = %q, want hash-1", got)
	}
}
