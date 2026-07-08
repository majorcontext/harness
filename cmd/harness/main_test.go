package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
)

func TestSessionDir(t *testing.T) {
	t.Run("no-save disables persistence", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "/somewhere")
		dir, err := sessionDir(true)
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		if dir != "" {
			t.Errorf("sessionDir(noSave) = %q, want empty", dir)
		}
	})
	t.Run("env var wins", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "/custom/sessions")
		dir, err := sessionDir(false)
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		if dir != "/custom/sessions" {
			t.Errorf("sessionDir = %q, want /custom/sessions", dir)
		}
	})
	t.Run("defaults to HOME/.harness/sessions", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, err := sessionDir(false)
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		want := filepath.Join(home, ".harness", "sessions")
		if dir != want {
			t.Errorf("sessionDir = %q, want %q", dir, want)
		}
	})
}

// writeSessionFile writes a session log in the JSONL format documented in
// engine/store.go: a session header followed by message records.
func writeSessionFile(t *testing.T, dir, id string, createdAt time.Time, messages int) {
	t.Helper()
	f := fmt.Sprintf("{\"type\":\"session\",\"id\":%q,\"created_at\":%q}\n",
		id, createdAt.Format(time.RFC3339Nano))
	for i := 0; i < messages; i++ {
		f += fmt.Sprintf("{\"type\":\"message\",\"message\":{\"id\":\"msg_%d\",\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"text\":\"hello %d\"}],\"created_at\":%q}}\n",
			i, i, createdAt.Format(time.RFC3339Nano))
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(f), 0o644); err != nil {
		t.Fatalf("writing session file: %v", err)
	}
}

func TestResolveSession(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	writeSessionFile(t, dir, "ses_old", base, 2)
	writeSessionFile(t, dir, "ses_new", base.Add(time.Hour), 4)
	cfg := engine.Config{SessionDir: dir}

	t.Run("new session by default", func(t *testing.T) {
		s, err := resolveSession(cfg, "", false)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if s.ID == "ses_old" || s.ID == "ses_new" {
			t.Errorf("expected fresh session, got existing ID %q", s.ID)
		}
		if len(s.History()) != 0 {
			t.Errorf("fresh session has %d messages, want 0", len(s.History()))
		}
	})
	t.Run("resume by id", func(t *testing.T) {
		s, err := resolveSession(cfg, "ses_old", false)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if s.ID != "ses_old" {
			t.Errorf("s.ID = %q, want ses_old", s.ID)
		}
		if got := len(s.History()); got != 2 {
			t.Errorf("history length = %d, want 2", got)
		}
	})
	t.Run("continue picks most recent", func(t *testing.T) {
		s, err := resolveSession(cfg, "", true)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if s.ID != "ses_new" {
			t.Errorf("s.ID = %q, want ses_new", s.ID)
		}
		if got := len(s.History()); got != 4 {
			t.Errorf("history length = %d, want 4", got)
		}
	})
	t.Run("resume and continue are mutually exclusive", func(t *testing.T) {
		if _, err := resolveSession(cfg, "ses_old", true); err == nil {
			t.Error("expected error for -r with -c")
		}
	})
	t.Run("continue with no sessions errors", func(t *testing.T) {
		empty := engine.Config{SessionDir: t.TempDir()}
		if _, err := resolveSession(empty, "", true); err == nil {
			t.Error("expected error when no sessions exist")
		}
	})
	t.Run("resume unknown id errors", func(t *testing.T) {
		if _, err := resolveSession(cfg, "ses_missing", false); err == nil {
			t.Error("expected error for unknown session id")
		}
	})
}

func TestFormatSessions(t *testing.T) {
	t.Run("empty list yields no output", func(t *testing.T) {
		if got := formatSessions(nil); got != "" {
			t.Errorf("formatSessions(nil) = %q, want empty", got)
		}
	})
	t.Run("one line per session, tab-separated", func(t *testing.T) {
		infos := []engine.SessionInfo{
			{ID: "ses_a", CreatedAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC), Messages: 2},
			{ID: "ses_b", CreatedAt: time.Date(2024, 6, 1, 13, 30, 0, 0, time.UTC), Messages: 5},
		}
		want := "ses_a\t2024-06-01T12:00:00Z\t2\nses_b\t2024-06-01T13:30:00Z\t5\n"
		if got := formatSessions(infos); got != want {
			t.Errorf("formatSessions = %q, want %q", got, want)
		}
	})
}

func TestResolveSessionNoSave(t *testing.T) {
	// -no-save yields an empty SessionDir; resuming must fail with a
	// clear error before touching the engine.
	cfg := engine.Config{SessionDir: ""}
	t.Run("resume with no-save errors clearly", func(t *testing.T) {
		_, err := resolveSession(cfg, "ses_x", false)
		if err == nil {
			t.Fatal("expected error for -r with -no-save")
		}
		if !strings.Contains(err.Error(), "-no-save") {
			t.Errorf("error = %q, want mention of -no-save", err)
		}
	})
	t.Run("continue with no-save errors clearly", func(t *testing.T) {
		_, err := resolveSession(cfg, "", true)
		if err == nil {
			t.Fatal("expected error for -c with -no-save")
		}
		if !strings.Contains(err.Error(), "-no-save") {
			t.Errorf("error = %q, want mention of -no-save", err)
		}
	})
	t.Run("new session with no-save is fine", func(t *testing.T) {
		if _, err := resolveSession(cfg, "", false); err != nil {
			t.Errorf("resolveSession: %v", err)
		}
	})
}
