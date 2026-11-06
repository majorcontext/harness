package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const maxDiskCheckpointBytes = (128<<20)*4/3 + 2048

func (d *DiskStore) checkpointPath(session string) string {
	return filepath.Join(d.root, session, "checkpoint")
}

func readDiskCheckpoint(path string, load bool) (uint64, []byte, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, nil, false, err
	}
	if info.Size() < 40 || info.Size() > maxDiskCheckpointBytes+40 {
		return 0, nil, false, nil
	}
	var header [40]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return 0, nil, false, nil
	}
	h := sha256.New()
	_, _ = h.Write(header[:8])
	var data []byte
	if load {
		data = make([]byte, info.Size()-40)
		if _, err := io.ReadFull(f, data); err != nil {
			return 0, nil, false, nil
		}
		_, _ = h.Write(data)
	} else if _, err := io.Copy(h, io.LimitReader(f, info.Size()-40)); err != nil {
		return 0, nil, false, err
	}
	if !bytes.Equal(h.Sum(nil), header[8:]) {
		return 0, nil, false, nil
	}
	return binary.BigEndian.Uint64(header[:8]), data, true, nil
}

// LoadCheckpoint returns the latest checkpoint, or zero and nil when absent or corrupt.
func (d *DiskStore) LoadCheckpoint(ctx context.Context, session string) (uint64, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	s, err := d.lock(session)
	if err != nil {
		return 0, nil, err
	}
	defer s.mu.Unlock()
	seq, data, valid, err := readDiskCheckpoint(d.checkpointPath(session), true)
	if err != nil || !valid {
		return 0, nil, err
	}
	head, err := d.headLocked(ctx, session, s)
	if err != nil || seq > head {
		return 0, nil, err
	}
	return seq, data, nil
}

// SaveCheckpoint replaces a checkpoint at an equal or lower sequence.
func (d *DiskStore) SaveCheckpoint(ctx context.Context, session string, seq uint64, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) > maxDiskCheckpointBytes {
		return fmt.Errorf("checkpoint too large")
	}
	s, err := d.lock(session)
	if err != nil {
		return err
	}
	defer s.mu.Unlock()
	dir := filepath.Join(d.root, session)
	if err := d.mkdirSynced(dir); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "checkpoint.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := lockFile(ctx, lock, false); err != nil {
		return err
	}
	head, err := d.headLocked(ctx, session, s)
	if err != nil {
		return err
	}
	if seq > head {
		return fmt.Errorf("checkpoint sequence %d exceeds head %d", seq, head)
	}
	path := d.checkpointPath(session)
	old, _, valid, err := readDiskCheckpoint(path, false)
	if err != nil {
		return err
	}
	if valid && old <= head && old > seq {
		return nil
	}
	return writeDiskCheckpoint(dir, path, seq, data)
}

func writeDiskCheckpoint(dir, path string, seq uint64, data []byte) error {
	f, err := os.CreateTemp(dir, ".checkpoint-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	var header [40]byte
	binary.BigEndian.PutUint64(header[:8], seq)
	h := sha256.New()
	_, _ = h.Write(header[:8])
	_, _ = h.Write(data)
	copy(header[8:], h.Sum(nil))
	_, err = f.Write(header[:])
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err == nil {
		err = syncDir(dir)
	}
	return err
}
