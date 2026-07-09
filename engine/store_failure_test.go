package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// unwritableSessionDir returns a SessionDir path guaranteed to make
// (*Session).ensureLog fail on its first disk touch, deterministically,
// regardless of which user runs the test.
//
// Under a normal (non-root) user, chmod 0555 (read+execute, no write) on an
// already-existing directory is enough: ensureLog's os.MkdirAll is a no-op on
// a directory that already exists, so the os.OpenFile(..., O_CREATE, ...)
// that follows hits a permission-denied error, since DAC permission bits are
// enforced normally.
//
// Root bypasses DAC permission checks (a chmod 0555 directory is still
// writable by root on Linux), so chmod alone would make this test silently
// exercise a real, successful write when the suite runs as root — as many CI
// containers and this sandbox do. The deterministic alternative used in that
// case: pre-create a plain FILE at the exact path where SessionDir itself
// needs to be a directory, so every os.MkdirAll of it fails with ENOTDIR — a
// structural filesystem error no privilege level can bypass (you cannot
// mkdir through a path component that already exists as a non-directory).
func unwritableSessionDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if os.Geteuid() == 0 {
		blocked := filepath.Join(base, "blocked")
		if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("seed blocking file: %v", err)
		}
		// SessionDir names a path *through* the file above; os.MkdirAll can
		// never create it.
		return filepath.Join(blocked, "sessions")
	}
	dir := filepath.Join(base, "sessions")
	if err := os.MkdirAll(dir, 0o555); err != nil {
		t.Fatalf("seed unwritable session dir: %v", err)
	}
	return dir
}

// TestPromptSurvivesUnwritableSessionDir covers engine/store.go's promise
// that a persistence write error never crashes the agent loop: Prompt must
// complete normally (its return value governed only by the provider/tool
// loop, never by disk state) and report the failure exclusively through
// PersistErr.
func TestPromptSurvivesUnwritableSessionDir(t *testing.T) {
	dir := unwritableSessionDir(t)
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if s.PersistErr() != nil {
		t.Fatalf("PersistErr() before any write = %v, want nil", s.PersistErr())
	}

	msg, err := s.Prompt(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Prompt returned an error from a disk-write failure — the package promises write errors never crash the loop: %v", err)
	}
	if msg == nil {
		t.Fatal("Prompt returned a nil assistant message")
	}
	if perr := s.PersistErr(); perr == nil {
		t.Fatal("PersistErr() = nil after Prompt against an unwritable SessionDir, want the write failure reported")
	}
}

// TestRegisterGoalSurvivesUnwritableSessionDir covers the same disk-failure
// guarantee on RegisterGoal's persistGoalLocked path: a goal.set record that
// cannot be written must not stop the goal from registering (RegisterGoal
// only errors when a goal is already active) or panic — it surfaces solely
// through PersistErr.
func TestRegisterGoalSurvivesUnwritableSessionDir(t *testing.T) {
	dir := unwritableSessionDir(t)
	prov := &scriptedProvider{name: "test"}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if err := s.RegisterGoal("reach the goal"); err != nil {
		t.Fatalf("RegisterGoal returned an error from a disk-write failure: %v", err)
	}
	if cond, ok := s.ActiveGoal(); !ok || cond != "reach the goal" {
		t.Errorf("ActiveGoal = %q, %v; want the goal active in memory despite the persist failure", cond, ok)
	}
	if perr := s.PersistErr(); perr == nil {
		t.Fatal("PersistErr() = nil after RegisterGoal against an unwritable SessionDir, want the write failure reported")
	}
}
