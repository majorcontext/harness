package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider/anthropic"
)

func TestSessionDir(t *testing.T) {
	t.Run("no-save disables persistence", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "/somewhere")
		dir, err := sessionDir(true, "/config/sessions")
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		if dir != "" {
			t.Errorf("sessionDir(noSave) = %q, want empty", dir)
		}
	})
	t.Run("env var wins", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "/custom/sessions")
		dir, err := sessionDir(false, "/config/sessions")
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		if dir != "/custom/sessions" {
			t.Errorf("sessionDir = %q, want /custom/sessions", dir)
		}
	})
	t.Run("config dir beats default when env unset", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "")
		dir, err := sessionDir(false, "/config/sessions")
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		if dir != "/config/sessions" {
			t.Errorf("sessionDir = %q, want /config/sessions", dir)
		}
	})
	t.Run("defaults to HOME/.harness/sessions", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, err := sessionDir(false, "")
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		want := filepath.Join(home, ".harness", "sessions")
		if dir != want {
			t.Errorf("sessionDir = %q, want %q", dir, want)
		}
	})
	t.Run("full precedence chain", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		// -no-save beats everything.
		t.Setenv("HARNESS_SESSION_DIR", "/env")
		if dir, _ := sessionDir(true, "/config"); dir != "" {
			t.Errorf("no-save: got %q, want empty", dir)
		}
		// env beats config.
		if dir, _ := sessionDir(false, "/config"); dir != "/env" {
			t.Errorf("env: got %q, want /env", dir)
		}
		// config beats default.
		t.Setenv("HARNESS_SESSION_DIR", "")
		if dir, _ := sessionDir(false, "/config"); dir != "/config" {
			t.Errorf("config: got %q, want /config", dir)
		}
		// default when nothing set.
		want := filepath.Join(home, ".harness", "sessions")
		if dir, _ := sessionDir(false, ""); dir != want {
			t.Errorf("default: got %q, want %q", dir, want)
		}
	})
}

// writeSessionFile writes a session log in the JSONL format documented in
// engine/store.go: a session header, a model record, then message records.
func writeSessionFile(t *testing.T, dir, id string, createdAt time.Time, messages int) {
	t.Helper()
	f := fmt.Sprintf("{\"type\":\"session\",\"id\":%q,\"created_at\":%q}\n",
		id, createdAt.Format(time.RFC3339Nano))
	f += "{\"type\":\"model\",\"model\":\"anthropic/persisted-model\"}\n"
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
		s, err := resolveSession(cfg, "", false, false)
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
		s, err := resolveSession(cfg, "ses_old", false, false)
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
		s, err := resolveSession(cfg, "", true, false)
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
		if _, err := resolveSession(cfg, "ses_old", true, false); err == nil {
			t.Error("expected error for -r with -c")
		}
	})
	t.Run("continue with no sessions errors", func(t *testing.T) {
		empty := engine.Config{SessionDir: t.TempDir()}
		if _, err := resolveSession(empty, "", true, false); err == nil {
			t.Error("expected error when no sessions exist")
		}
	})
	t.Run("resume unknown id errors", func(t *testing.T) {
		if _, err := resolveSession(cfg, "ses_missing", false, false); err == nil {
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
		_, err := resolveSession(cfg, "ses_x", false, false)
		if err == nil {
			t.Fatal("expected error for -r with -no-save")
		}
		if !strings.Contains(err.Error(), "-no-save") {
			t.Errorf("error = %q, want mention of -no-save", err)
		}
	})
	t.Run("continue with no-save errors clearly", func(t *testing.T) {
		_, err := resolveSession(cfg, "", true, false)
		if err == nil {
			t.Fatal("expected error for -c with -no-save")
		}
		if !strings.Contains(err.Error(), "-no-save") {
			t.Errorf("error = %q, want mention of -no-save", err)
		}
	})
	t.Run("new session with no-save is fine", func(t *testing.T) {
		if _, err := resolveSession(cfg, "", false, false); err != nil {
			t.Errorf("resolveSession: %v", err)
		}
	})
}

func TestResolveSessionModelFlag(t *testing.T) {
	persisted := message.ModelRef{Provider: "anthropic", Model: "persisted-model"}
	flagModel := message.ModelRef{Provider: "anthropic", Model: "flag-model"}
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	newCfg := func(t *testing.T) engine.Config {
		t.Helper()
		dir := t.TempDir()
		writeSessionFile(t, dir, "ses_m", base, 2)
		return engine.Config{SessionDir: dir, Model: flagModel}
	}

	t.Run("explicit -model wins on resume", func(t *testing.T) {
		cfg := newCfg(t)
		s, err := resolveSession(cfg, "ses_m", false, true)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if got := s.Model(); got != flagModel {
			t.Errorf("s.Model() = %v, want %v (explicit flag must win)", got, flagModel)
		}
		// SetModel persists a model record, so a subsequent load sees the
		// override too.
		s2, err := engine.LoadSession(cfg, "ses_m")
		if err != nil {
			t.Fatalf("LoadSession after override: %v", err)
		}
		if got := s2.Model(); got != flagModel {
			t.Errorf("reloaded s.Model() = %v, want %v (override must persist)", got, flagModel)
		}
	})
	t.Run("persisted model retained without explicit -model", func(t *testing.T) {
		cfg := newCfg(t)
		s, err := resolveSession(cfg, "ses_m", false, false)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if got := s.Model(); got != persisted {
			t.Errorf("s.Model() = %v, want %v (persisted model must be retained)", got, persisted)
		}
	})
	t.Run("explicit -model on continue wins too", func(t *testing.T) {
		cfg := newCfg(t)
		s, err := resolveSession(cfg, "", true, true)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if got := s.Model(); got != flagModel {
			t.Errorf("s.Model() = %v, want %v (explicit flag must win)", got, flagModel)
		}
	})
	t.Run("fresh session uses flag model regardless", func(t *testing.T) {
		cfg := newCfg(t)
		s, err := resolveSession(cfg, "", false, true)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if got := s.Model(); got != flagModel {
			t.Errorf("s.Model() = %v, want %v", got, flagModel)
		}
	})
}

func TestRegistry(t *testing.T) {
	t.Run("defaults to ANTHROPIC_API_KEY and empty base url", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-default")
		reg := registry(&config.Config{})
		c, ok := reg[anthropic.Family].(*anthropic.Client)
		if !ok {
			t.Fatalf("anthropic provider is %T, want *anthropic.Client", reg[anthropic.Family])
		}
		if c.APIKey != "sk-default" {
			t.Errorf("APIKey = %q, want sk-default", c.APIKey)
		}
		if c.BaseURL != "" {
			t.Errorf("BaseURL = %q, want empty", c.BaseURL)
		}
	})
	t.Run("config api_key_env and base_url are honored", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "ignored")
		t.Setenv("MY_ANTHROPIC_KEY", "sk-custom")
		reg := registry(&config.Config{Providers: map[string]config.Provider{
			"anthropic": {APIKeyEnv: "MY_ANTHROPIC_KEY", BaseURL: "http://proxy"},
		}})
		c := reg[anthropic.Family].(*anthropic.Client)
		if c.APIKey != "sk-custom" {
			t.Errorf("APIKey = %q, want sk-custom", c.APIKey)
		}
		if c.BaseURL != "http://proxy" {
			t.Errorf("BaseURL = %q, want http://proxy", c.BaseURL)
		}
	})
	t.Run("nil config is safe", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-nil")
		reg := registry(nil)
		if c := reg[anthropic.Family].(*anthropic.Client); c.APIKey != "sk-nil" {
			t.Errorf("APIKey = %q, want sk-nil", c.APIKey)
		}
	})
}
