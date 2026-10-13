package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Store is the credential file. Its directory has mode 0700 and the file 0600.
type Store struct {
	// Path is the credential file. A lock file beside it, Path + ".lock", fences the writers.
	Path string
}

// Load reads the login. A missing, unreadable, or empty file is a LoginRequiredError.
func (s Store) Load() (Credentials, error) {
	if s.Path == "" {
		return Credentials{}, &LoginRequiredError{Reason: "the config sets no codex_auth_file"}
	}
	if info, err := os.Stat(s.Path); err == nil && otherUsersCanRead(info.Mode()) {
		return Credentials{}, &LoginRequiredError{Reason: s.Path + " can be read by other users; run chmod 600 on it, or sign in again"}
	}
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Credentials{}, &LoginRequiredError{Reason: "no login file at " + s.Path}
	}
	if err != nil {
		return Credentials{}, &LoginRequiredError{Reason: "cannot read " + s.Path}
	}
	var c Credentials
	if json.Unmarshal(raw, &c) != nil || c.AccessToken == "" || c.AccountID == "" {
		return Credentials{}, &LoginRequiredError{Reason: s.Path + " holds no login"}
	}
	return c, nil
}

// Save writes c over the file: a temporary file in the directory, synced, then renamed.
// A reader sees the old file or the new file, never a part of one. The caller holds Lock.
func (s Store) Save(c Credentials) error {
	dir := filepath.Dir(s.Path)
	if err := s.ensureDir(); err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".codex-*.tmp")
	if err != nil {
		return fmt.Errorf("codex login: %w", err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex login: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex login: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex login: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("codex login: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return fmt.Errorf("codex login: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ensureDir creates the directory with mode 0700 and refuses an existing directory that the
// group or other users can reach (on a platform that has such modes).
func (s Store) ensureDir() error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("codex login: %w", err)
	}
	if info, err := os.Stat(dir); err == nil && otherUsersCanRead(info.Mode()) {
		return fmt.Errorf("codex login: the directory %s has mode %04o and other users can reach it; run chmod 700 on it", dir, info.Mode().Perm())
	}
	return nil
}

// Remove deletes the file under the lock, so a refresh that writes it does not run beside it.
// A missing file is no error.
func (s Store) Remove(ctx context.Context) error {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("codex login: %w", err)
	}
	return nil
}

// Lock takes the exclusive lock of the file, which two processes need before they write a refreshed login.
// The unlock function releases it.
func (s Store) Lock(ctx context.Context) (unlock func(), err error) {
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("codex login: %w", err)
	}
	if err := lockFile(ctx, f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("codex login: lock: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
