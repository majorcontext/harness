package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
)

// savedSession is one session in the session file.
type savedSession struct {
	User string `json:"user"`
	CSRF string `json:"csrf"`
}

// load reads the session file. A missing file is no session.
func (s *sessions) load() error {
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
	var saved map[string]savedSession
	if err := json.Unmarshal(b, &saved); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, v := range saved {
		s.m[id] = &session{user: v.User, csrf: v.CSRF}
	}
	return nil
}

// save writes every session to the session file, readable by the owner only.
func (s *sessions) save() {
	if s.file == "" {
		return
	}
	// saveMu covers the copy and the write, so an older copy never writes last.
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	saved := make(map[string]savedSession, len(s.m))
	for id, v := range s.m {
		saved[id] = savedSession{User: v.user, CSRF: v.csrf}
	}
	s.mu.Unlock()
	b, _ := json.Marshal(saved)
	if err := writePrivate(s.file, b); err != nil {
		slog.Warn("the session file was not written", "file", s.file, "err", err)
	}
}
