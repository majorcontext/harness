package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DiskStoreOptions configures a DiskStore.
type DiskStoreOptions struct {
	// Sync is SessionSyncFsync (the zero value) or SessionSyncVolume. Volume
	// mode skips the directory fsync after a log file is created, because on
	// some FUSE and 9p transports fsync(dirfd) deadlocks the mount.
	Sync         string
	OnPhase      func(op, phase string, elapsed time.Duration)
	OnPhaseStart func(op, phase string)
}

// DiskStore keeps each journal as one JSONL file, <dir>/<id>.jsonl, and each
// blob as <dir>/<id>.blob/<name>. It implements SessionStore.
type DiskStore struct {
	dir  string
	opts DiskStoreOptions

	mu      sync.Mutex
	handles map[string]*diskHandle
	// dirPending holds ids whose directory entry may not be durable. A
	// failed sync_dir leaves its records landed, so Append cannot report
	// the failure as a lost write; Sync retries it instead.
	dirPending map[string]bool
}

type diskHandle struct {
	f    *os.File
	size int64
	n    int
}

func NewDiskStore(dir string, opts DiskStoreOptions) *DiskStore {
	return &DiskStore{dir: dir, opts: opts, handles: map[string]*diskHandle{}, dirPending: map[string]bool{}}
}

func (d *DiskStore) Dir() string { return d.dir }

func checkStoreName(kind, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("engine: invalid store %s %q", kind, name)
	}
	return nil
}

func appendConflict(at, have int) error {
	return fmt.Errorf("%w: append at %d, log holds %d records", ErrAppendConflict, at, have)
}

func (d *DiskStore) phase(op, phase string, fn func() error) error {
	if d.opts.OnPhaseStart != nil {
		d.opts.OnPhaseStart(op, phase)
	}
	t0 := time.Now()
	err := fn()
	if d.opts.OnPhase != nil {
		d.opts.OnPhase(op, phase, time.Since(t0))
	}
	return err
}

// open returns the id's handle, creating the directory and file and
// repairing a torn tail on first use. The fast path reports no phases.
// Caller holds d.mu.
func (d *DiskStore) open(id string) (*diskHandle, error) {
	if h := d.handles[id]; h != nil {
		return h, nil
	}
	const op = "ensure_log"
	if err := d.phase(op, "mkdir", func() error {
		return os.MkdirAll(d.dir, 0o755)
	}); err != nil {
		return nil, err
	}
	// O_RDWR, not O_WRONLY: the tail repair reads the file's last byte.
	// O_APPEND still governs every Write.
	path := sessionPath(d.dir, id)
	var f *os.File
	if err := d.phase(op, "open", func() error {
		var err error
		f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
		return err
	}); err != nil {
		return nil, err
	}
	var size int64
	var data []byte
	if err := d.phase(op, "stat", func() error {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		size = fi.Size()
		if size > 0 {
			data, err = os.ReadFile(path)
		}
		return err
	}); err != nil {
		f.Close()
		return nil, err
	}
	n := len(splitRecords(data))
	if size > 0 && data[len(data)-1] != '\n' {
		if err := d.phase(op, "tail_repair", func() error {
			var err error
			size, err = repairTail(f, data)
			return err
		}); err != nil {
			f.Close()
			return nil, err
		}
	}
	h := &diskHandle{f: f, size: size, n: n}
	d.handles[id] = h
	return h, nil
}

// repairTail fixes a journal whose last byte is not '\n'. Appending a record
// straight after such a tail concatenates the two into one unparseable line.
// That line silently drops the new record while it is the last line, and
// becomes a hard load error once a later record follows it. Two causes need
// opposite repairs, decided by whether the tail is valid JSON, the rule Load
// uses:
//
//   - The tail is torn: Load already treats it as never written, so truncate
//     back to just after the last '\n'.
//   - The tail is a complete record that lost only its newline: Load keeps
//     it, so append the '\n'. Truncating would destroy a durable record.
func repairTail(f *os.File, data []byte) (int64, error) {
	size := int64(len(data))
	tailStart := bytes.LastIndexByte(data, '\n') + 1
	tail := bytes.TrimSpace(data[tailStart:])
	if len(tail) > 0 && json.Valid(tail) {
		if _, err := f.Write([]byte("\n")); err != nil {
			return 0, err
		}
		return size + 1, nil
	}
	if err := f.Truncate(int64(tailStart)); err != nil {
		return 0, err
	}
	return int64(tailStart), nil
}

// openForEngine opens (and repairs) the id's journal and returns its size.
func (d *DiskStore) openForEngine(id string) (int64, error) {
	if err := checkStoreName("id", id); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	h, err := d.open(id)
	if err != nil {
		return 0, err
	}
	return h.size, nil
}

func (d *DiskStore) modTime(id string) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var fi os.FileInfo
	var err error
	if h := d.handles[id]; h != nil {
		fi, err = h.f.Stat()
	} else {
		fi, err = os.Stat(sessionPath(d.dir, id))
	}
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

func (d *DiskStore) dropLocked(id string, h *diskHandle) {
	h.f.Close()
	delete(d.handles, id)
}

func (d *DiskStore) Append(id string, at int, records ...[]byte) error {
	if err := checkStoreName("id", id); err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, r := range records {
		if len(r) == 0 || bytes.IndexByte(r, '\n') >= 0 {
			return errors.New("engine: store record is empty or contains a newline")
		}
		buf.Write(r)
		buf.WriteByte('\n')
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if at != 0 && d.handles[id] == nil {
		if _, err := os.Stat(sessionPath(d.dir, id)); errors.Is(err, fs.ErrNotExist) {
			return appendConflict(at, 0)
		}
	}
	h, err := d.open(id)
	if err != nil {
		return err
	}
	if h.n != at {
		return appendConflict(at, h.n)
	}
	first := h.size == 0
	write := func() error {
		_, err := h.f.Write(buf.Bytes())
		return err
	}
	if first {
		err = d.phase("ensure_log", "header_write", write)
	} else {
		err = write()
	}
	if err != nil {
		// Best effort: remove whatever part of the write landed. The
		// closed handle makes the next call reopen and repair a tail this
		// truncate could not remove.
		_ = h.f.Truncate(h.size)
		d.dropLocked(id, h)
		return err
	}
	h.size += int64(buf.Len())
	h.n += len(records)
	// A file fsync commits contents, not the directory entry. On a new
	// file the entry only just appeared, so Sync alone would not make the
	// first records durable on some filesystems.
	if first && d.opts.Sync != SessionSyncVolume {
		d.dirPending[id] = true
		if err := d.syncDirPending(id); err != nil {
			return err
		}
	}
	return nil
}

func (d *DiskStore) syncDirPending(id string) error {
	if !d.dirPending[id] {
		return nil
	}
	if err := d.phase("ensure_log", "sync_dir", func() error {
		return syncDirFn(d.dir)
	}); err != nil {
		return err
	}
	delete(d.dirPending, id)
	return nil
}

var syncDirFn = syncDir

func (d *DiskStore) Sync(id string) error {
	if err := checkStoreName("id", id); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.syncDirPending(id); err != nil {
		return err
	}
	if h := d.handles[id]; h != nil {
		return h.f.Sync()
	}
	f, err := os.OpenFile(sessionPath(d.dir, id), os.O_RDWR, 0o644)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// splitRecords returns the journal's records: each non-blank line, trimmed.
// An unterminated final line is a record only when it is valid JSON. A
// terminated corrupt line is returned as is, so the fold's corruption rule
// stays the one place that decides what it means.
func splitRecords(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		line := data
		terminated := false
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data, terminated = data[:i], data[i+1:], true
		} else {
			data = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || (!terminated && !json.Valid(line)) {
			continue
		}
		out = append(out, line)
	}
	return out
}

func (d *DiskStore) Load(id string) ([][]byte, error) {
	if err := checkStoreName("id", id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(sessionPath(d.dir, id))
	if err != nil {
		return nil, err
	}
	return splitRecords(data), nil
}

func (d *DiskStore) Len(id string) (int, error) {
	if err := checkStoreName("id", id); err != nil {
		return 0, err
	}
	d.mu.Lock()
	if h := d.handles[id]; h != nil {
		n := h.n
		d.mu.Unlock()
		return n, nil
	}
	d.mu.Unlock()
	recs, err := d.Load(id)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return len(recs), err
}

func (d *DiskStore) Header(id string) ([]byte, error) {
	if err := checkStoreName("id", id); err != nil {
		return nil, err
	}
	f, err := os.Open(sessionPath(d.dir, id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		terminated := err == nil
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		line = bytes.TrimSpace(line)
		if len(line) > 0 && (terminated || json.Valid(line)) {
			return line, nil
		}
		if !terminated {
			return nil, &fs.PathError{Op: "header", Path: sessionPath(d.dir, id), Err: fs.ErrNotExist}
		}
	}
}

func (d *DiskStore) List() ([]string, error) {
	entries, err := os.ReadDir(d.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".jsonl"))
	}
	return ids, nil
}

func (d *DiskStore) blobPath(id, name string) (string, error) {
	if err := checkStoreName("id", id); err != nil {
		return "", err
	}
	if err := checkStoreName("blob name", name); err != nil {
		return "", err
	}
	return filepath.Join(d.dir, id+".blob", name), nil
}

func (d *DiskStore) PutBlob(id, name string, data []byte) error {
	path, err := d.blobPath(id, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".put-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Close()
	} else {
		tmp.Close()
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func (d *DiskStore) GetBlob(id, name string) ([]byte, error) {
	path, err := d.blobPath(id, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (d *DiskStore) Release(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h := d.handles[id]; h != nil {
		d.dropLocked(id, h)
	}
}
