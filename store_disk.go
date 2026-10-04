package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// DiskStore keeps each session under <root>/<session>: records in log.jsonl,
// one per line, and blobs in blobs/<key>. A line number is the seq. Where
// flock does not exist, only the instance fences its appends.
type DiskStore struct {
	root string

	mu       sync.Mutex
	sessions map[string]*diskSession
	durable  map[string]bool
}

// diskSession guards the log of one session within one DiskStore. head,
// size, and total hold the last scan of the file: size is the byte length of
// the whole lines and total the file size, so bytes after size are torn.
type diskSession struct {
	mu     sync.Mutex
	head   uint64
	size   int64
	total  int64
	loaded bool
}

// NewDiskStore returns a DiskStore rooted at root. It creates root on first write.
func NewDiskStore(root string) *DiskStore {
	return &DiskStore{root: filepath.Clean(root), sessions: map[string]*diskSession{}, durable: map[string]bool{}}
}

func (d *DiskStore) logPath(session string) string {
	return filepath.Join(d.root, session, "log.jsonl")
}

// lock returns the session locked; the caller unlocks it.
func (d *DiskStore) lock(session string) (*diskSession, error) {
	if err := checkName("session", session); err != nil {
		return nil, err
	}
	d.mu.Lock()
	s := d.sessions[session]
	if s == nil {
		s = &diskSession{}
		d.sessions[session] = s
	}
	d.mu.Unlock()
	s.mu.Lock()
	return s, nil
}

// headLocked returns the head under a shared lock of the log file, which a
// writer holds exclusively from its first byte to its fsync or rollback, so
// the scan never reads bytes that a failed append cuts again. It never
// writes: a reader may share the directory with a live writer.
func (d *DiskStore) headLocked(ctx context.Context, session string, s *diskSession) (uint64, error) {
	f, err := os.Open(d.logPath(session))
	if errors.Is(err, fs.ErrNotExist) {
		return d.scanHead(session, s)
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if err := lockFile(ctx, f, true); err != nil {
		return 0, err
	}
	return d.scanHead(session, s)
}

// scanHead returns the head. Another DiskStore, in this process or in
// another, can append to the file, so it scans the bytes after the whole
// lines again when the file size differs from the last scan or the last
// scan ended in torn bytes. The caller holds a lock of the file.
func (d *DiskStore) scanHead(session string, s *diskSession) (uint64, error) {
	path := d.logPath(session)
	var total int64
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return 0, err
	default:
		total = fi.Size()
	}
	if s.loaded && total == s.total && s.total == s.size {
		return s.head, nil
	}
	if !s.loaded || total < s.size {
		s.head, s.size = 0, 0
	}
	lines, size, total, err := scanLog(path, s.size)
	if err != nil {
		s.loaded = false
		return 0, err
	}
	s.head, s.size, s.total, s.loaded = s.head+lines, size, total, true
	return s.head, nil
}

// scanLog returns the count of newline-terminated lines after offset from,
// the end offset of the last of them (from when there is none), and the
// file size. A missing file is empty.
func scanLog(path string, from int64) (lines uint64, size, total int64, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, 0, 0, err
	}
	size, total = from, from
	buf := make([]byte, 64<<10)
	for {
		n, rerr := f.Read(buf)
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			size = total + int64(i) + 1
		}
		lines += uint64(bytes.Count(buf[:n], []byte{'\n'}))
		total += int64(n)
		if rerr == io.EOF {
			return lines, size, total, nil
		}
		if rerr != nil {
			return 0, 0, 0, rerr
		}
	}
}

// Append implements Store. It checks the head again under an exclusive
// lock of the log file, so an instance whose head is stale conflicts.
func (d *DiskStore) Append(ctx context.Context, session string, expectedSeq uint64, records ...[]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkRecords(records); err != nil {
		return err
	}
	s, err := d.lock(session)
	if err != nil {
		return err
	}
	defer s.mu.Unlock()
	head, err := d.headLocked(ctx, session, s)
	if err != nil {
		return err
	}
	if head != expectedSeq {
		return conflict(session, head, expectedSeq)
	}
	if len(records) == 0 {
		return nil
	}
	if err := d.writeLocked(ctx, session, s, expectedSeq, records); err != nil {
		s.loaded = false
		return err
	}
	return nil
}

func conflict(session string, head, at uint64) error {
	return fmt.Errorf("%w: session %q has %d records, append at %d", ErrConflict, session, head, at)
}

// writeLocked locks the log file exclusively, checks the head, appends, and
// fsyncs. A failure leaves the log in an unknown state, so the caller scans
// it again. A wait for the lock ends with ctx.
func (d *DiskStore) writeLocked(ctx context.Context, session string, s *diskSession, expectedSeq uint64, records [][]byte) error {
	path := d.logPath(session)
	dir := filepath.Dir(path)
	if err := d.mkdirSynced(dir); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	err = lockFile(ctx, f, false)
	var head uint64
	if err == nil {
		head, err = d.scanHead(session, s)
	}
	if err == nil && head != expectedSeq {
		err = conflict(session, head, expectedSeq)
	}
	if err == nil {
		err = d.durably(path, func() error { return syncDir(dir) })
	}
	if err == nil {
		err = appendSynced(f, s, bytes.Join(records, []byte{'\n'}))
	}
	if err == nil {
		s.head += uint64(len(records))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// appendSynced cuts torn bytes, writes b and a final newline after the whole
// lines, then fsyncs. On failure it cuts the log back so the append reads
// as not landed.
func appendSynced(f *os.File, s *diskSession, b []byte) error {
	var err error
	if s.total > s.size {
		err = f.Truncate(s.size)
	}
	if err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		_ = f.Truncate(s.size)
		_ = f.Sync()
		return err
	}
	s.size += int64(len(b)) + 1
	s.total = s.size
	return nil
}

// Read implements Store.
func (d *DiskStore) Read(ctx context.Context, session string, afterSeq uint64, limit int) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	head, err := d.Head(ctx, session)
	if err != nil || head <= afterSeq || limit <= 0 {
		return nil, err
	}
	f, err := os.Open(d.logPath(session))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Record
	r := bufio.NewReader(f)
	for seq := uint64(1); seq <= head && len(out) < limit; seq++ {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		if seq > afterSeq {
			out = append(out, Record{Seq: seq, Data: line[:len(line)-1]})
		}
	}
	return out, nil
}

// Head implements Store.
func (d *DiskStore) Head(ctx context.Context, session string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s, err := d.lock(session)
	if err != nil {
		return 0, err
	}
	defer s.mu.Unlock()
	return d.headLocked(ctx, session, s)
}

// Sessions implements Store.
func (d *DiskStore) Sessions(ctx context.Context, after string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.Name() > after && hasRecord(d.logPath(e.Name())) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids[:max(0, min(limit, len(ids)))], nil
}

// hasRecord reports whether the file holds one newline-terminated line.
func hasRecord(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		_, err := r.ReadSlice('\n')
		if err != bufio.ErrBufferFull {
			return err == nil
		}
	}
}

func (d *DiskStore) blobPath(session, key string) (string, error) {
	if err := checkName("session", session); err != nil {
		return "", err
	}
	if err := checkName("blob key", key); err != nil {
		return "", err
	}
	return filepath.Join(d.root, session, "blobs", key), nil
}

// PutBlob implements Store.
func (d *DiskStore) PutBlob(ctx context.Context, session, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := d.blobPath(session, key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := d.mkdirSynced(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = io.Copy(tmp, r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err == nil {
		err = syncDir(dir)
	}
	return err
}

// GetBlob implements Store.
func (d *DiskStore) GetBlob(ctx context.Context, session, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := d.blobPath(session, key)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

// durably runs fn until it succeeds once for key.
func (d *DiskStore) durably(key string, fn func() error) error {
	d.mu.Lock()
	done := d.durable[key]
	d.mu.Unlock()
	if done {
		return nil
	}
	if err := fn(); err != nil {
		return err
	}
	d.mu.Lock()
	d.durable[key] = true
	d.mu.Unlock()
	return nil
}

// mkdirSynced creates dir and any missing parent below root, and fsyncs the
// parent of each directory until one fsync succeeds.
func (d *DiskStore) mkdirSynced(dir string) error {
	return d.durably(dir, func() error {
		if dir == d.root {
			return os.MkdirAll(dir, 0o755)
		}
		parent := filepath.Dir(dir)
		if err := d.mkdirSynced(parent); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		return syncDir(parent)
	})
}
