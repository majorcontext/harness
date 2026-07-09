package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestWorkDirHeaderRoundTrip verifies that Config.WorkDir is persisted into
// the session header record and restored by LoadSession into cfg, exposed via
// Session.WorkDir().
func TestWorkDirHeaderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(dir, prov)
	cfg.WorkDir = "/some/project/dir"
	s := NewSession(cfg)

	if got := s.WorkDir(); got != cfg.WorkDir {
		t.Fatalf("fresh session WorkDir() = %q, want %q", got, cfg.WorkDir)
	}

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.WorkDir(); got != cfg.WorkDir {
		t.Errorf("loaded WorkDir() = %q, want %q", got, cfg.WorkDir)
	}
}

// TestWorkDirRestoreWinsOverLoadingConfig verifies that a restored WorkDir
// from the header overrides whatever WorkDir the LoadSession caller supplies
// — the persisted value is the durable truth for a resumed session.
func TestWorkDirRestoreWinsOverLoadingConfig(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(dir, prov)
	cfg.WorkDir = "/original/workdir"
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	loadCfg := cfg
	loadCfg.WorkDir = "/different/workdir"
	loaded, err := LoadSession(loadCfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.WorkDir(); got != "/original/workdir" {
		t.Errorf("loaded WorkDir() = %q, want restored value %q (must win over loading Config.WorkDir)", got, "/original/workdir")
	}
}

// TestWorkDirLegacyHeaderCompat verifies that a session log written before
// this feature (no "workdir" field in its header) keeps current behavior on
// load: the loading Config.WorkDir is used unchanged, since there is nothing
// to restore.
func TestWorkDirLegacyHeaderCompat(t *testing.T) {
	dir := t.TempDir()
	id := "ses_legacy"
	data := `{"type":"session","id":"ses_legacy","created_at":"2025-01-02T03:04:05Z"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"hi"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{SessionDir: dir, WorkDir: "/caller/workdir", Model: message.ModelRef{Provider: "test", Model: "m1"}}
	s, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.WorkDir(); got != "/caller/workdir" {
		t.Errorf("legacy header WorkDir() = %q, want caller's Config.WorkDir %q unchanged", got, "/caller/workdir")
	}
}
