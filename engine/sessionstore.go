package engine

import "errors"

// ErrAppendConflict is returned (wrapped) by SessionStore.Append when the log
// does not hold exactly the expected number of records.
var ErrAppendConflict = errors.New("engine: session store append position conflict")

// SessionStore persists a session journal as an ordered list of JSON
// records. Records passed to Append and returned by Load carry no trailing
// newline. The index sidecar, snapshots, and tool-result retention are disk
// caches outside this interface.
//
// Every implementation keeps five invariants:
//
//  1. Appends are ordered. The caller serializes Append calls for one id
//     (Session.mu). A record is visible only after every earlier record is.
//  2. One Append call with several records is atomic.
//  3. A torn trailing record never happened. Load never returns it, and the
//     next Append repairs the tail first. A complete valid tail record that
//     lost only its newline is kept and terminated.
//  4. A failed Append leaves no visible partial record. The next call on the
//     same id recovers.
//  5. Sync makes every record appended so far survive a crash. Append alone
//     does not promise this.
//  6. Append is a compare-and-append. It succeeds only when the log holds
//     exactly at records, and otherwise returns an error wrapping
//     ErrAppendConflict with nothing written. A store whose commit outcome
//     can be ambiguous (a network store that times out after a commit)
//     relies on this: the engine never blindly retries.
//
// List returns every log id, including logs that are not sessions, such as
// the events log. Callers filter with ValidSessionID.
type SessionStore interface {
	Append(id string, at int, records ...[]byte) error
	Sync(id string) error
	// Load returns fs.ErrNotExist when id is unknown.
	Load(id string) ([][]byte, error)
	// Len returns the record count: 0, nil when id is unknown.
	Len(id string) (int, error)
	// Header returns the first record: fs.ErrNotExist when id is unknown.
	Header(id string) ([]byte, error)
	List() ([]string, error)
	// PutBlob replaces the blob.
	PutBlob(id, name string, data []byte) error
	// GetBlob returns fs.ErrNotExist when the blob is absent.
	GetBlob(id, name string) ([]byte, error)
	// Release drops per-session handles. The data stays.
	Release(id string)
}

func (c Config) journalStore() SessionStore {
	if c.SessionStore != nil {
		return c.SessionStore
	}
	if c.SessionDir != "" {
		return NewDiskStore(c.SessionDir, DiskStoreOptions{
			Sync:         c.SessionSync,
			OnPhase:      c.OnStorePhase,
			OnPhaseStart: c.OnStorePhaseStart,
		})
	}
	return nil
}

// appendRecords appends at the session's own log position. An error that is
// not ErrAppendConflict may hide a commit that landed, so Len settles it once.
// A conflict fences the session: another writer owns the log.
func (s *Session) appendRecords(records ...[]byte) error {
	err := s.store.Append(s.ID, s.logLen, records...)
	if err == nil {
		s.logLen += len(records)
		return nil
	}
	if errors.Is(err, ErrAppendConflict) {
		s.fenced = err
		return err
	}
	if n, lerr := s.store.Len(s.ID); lerr == nil && n == s.logLen+len(records) {
		s.logLen = n
		return nil
	}
	return err
}

func (s *Session) diskStore() *DiskStore {
	d, _ := s.store.(*DiskStore)
	return d
}

func (s *Session) sidecarDir() string {
	if d := s.diskStore(); d != nil {
		return d.Dir()
	}
	return ""
}
