package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
)

// savedLogin is one login in the token file.
type savedLogin struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Account string `json:"account"`
}

// load reads the token file. A missing file is no login.
func (s *tokenStore) load() error {
	if s.file == "" {
		return nil
	}
	b, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved map[string]savedLogin
	if err := json.Unmarshal(b, &saved); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for user, l := range saved {
		s.m[user] = &userTokens{t: tokens(l), refreshed: s.now()}
	}
	return nil
}

// save writes every login to the token file, readable by the owner only. It
// replaces the file in one rename, so a crash leaves the old or the new file.
func (s *tokenStore) save() {
	if s.file == "" {
		return
	}
	// saveMu covers the copy and the write, so a save that copied older
	// logins never writes after one that copied newer logins.
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	entries := make(map[string]*userTokens, len(s.m))
	for user, e := range s.m {
		entries[user] = e
	}
	s.mu.Unlock()
	saved := make(map[string]savedLogin, len(entries))
	for user, e := range entries {
		e.mu.Lock()
		saved[user] = savedLogin(e.t)
		e.mu.Unlock()
	}
	b, _ := json.Marshal(saved)
	if err := writePrivate(s.file, b); err != nil {
		slog.Warn("the token file was not written", "file", s.file, "err", err)
	}
}

func writePrivate(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".private-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
