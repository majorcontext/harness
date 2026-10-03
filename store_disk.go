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
// one per line, and blobs in blobs/<key>. A line number is the seq.
type DiskStore struct {
	root string

	mu       sync.Mutex
	sessions map[string]*diskSession
}

// diskSession guards one session's log. head and size are valid only while
// loaded. size is the byte length of the whole lines; torn reports bytes after it.
type diskSession struct {
	mu     sync.Mutex
	head   uint64
	size   int64
	torn   bool
	loaded bool
}

// NewDiskStore returns a DiskStore rooted at root. It creates root on first write.
func NewDiskStore(root string) *DiskStore {
	return &DiskStore{root: filepath.Clean(root), sessions: map[string]*diskSession{}}
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

// headLocked loads the head on first use. It never writes: a reader may
// share the directory with a live writer.
func (d *DiskStore) headLocked(session string, s *diskSession) (uint64, error) {
	if s.loaded {
		return s.head, nil
	}
	lines, size, total, err := scanLog(d.logPath(session))
	if err != nil {
		return 0, err
	}
	s.head, s.size, s.torn, s.loaded = lines, size, total > size, true
	return lines, nil
}

// scanLog returns the count of newline-terminated lines, their byte length,
// and the file size. A missing file is empty.
func scanLog(path string) (lines uint64, size, total int64, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = f.Close() }()
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
	head, err := d.headLocked(session, s)
	if err != nil {
		return err
	}
	if head != expectedSeq {
		return fmt.Errorf("%w: session %q has %d records, append at %d", ErrConflict, session, head, expectedSeq)
	}
	if len(records) == 0 {
		return nil
	}
	if err := d.writeLocked(session, s, head == 0, records); err != nil {
		s.loaded = false
		return err
	}
	s.head, s.torn = head+uint64(len(records)), false
	return nil
}

// writeLocked repairs a torn tail, then appends and fsyncs. A failure leaves
// the log in an unknown state, so the caller reloads the head from disk.
func (d *DiskStore) writeLocked(session string, s *diskSession, first bool, records [][]byte) error {
	dir := filepath.Dir(d.logPath(session))
	if first {
		if err := mkdirSynced(d.root, dir); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	for _, r := range records {
		buf.Write(r)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(d.logPath(session), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if s.torn {
		err = f.Truncate(s.size)
	}
	if err == nil {
		_, err = f.Write(buf.Bytes())
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && first {
		err = syncDir(dir)
	}
	if err == nil {
		s.size += int64(buf.Len())
	}
	return err
}

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

func (d *DiskStore) Head(ctx context.Context, session string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s, err := d.lock(session)
	if err != nil {
		return 0, err
	}
	defer s.mu.Unlock()
	return d.headLocked(session, s)
}

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

func (d *DiskStore) PutBlob(ctx context.Context, session, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := d.blobPath(session, key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := mkdirSynced(d.root, dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
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

// mkdirSynced creates dir and any missing parent below root, and fsyncs the
// parent of each directory it creates.
func mkdirSynced(root, dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if dir == root {
		return os.MkdirAll(dir, 0o755)
	}
	parent := filepath.Dir(dir)
	if err := mkdirSynced(root, parent); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncDir(parent)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
