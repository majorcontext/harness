package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestParentSessionHeaderRoundTrip verifies that Config.ParentSession is
// persisted into the session header record and restored by LoadSession into
// cfg, exposed via Session.ParentSession() — mirroring
// TestWorkDirHeaderRoundTrip (see workdir_test.go), since the two fields
// live on the same durable header record and share the same restore rule.
func TestParentSessionHeaderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(dir, prov)
	cfg.ParentSession = "ses_0000000000000001"
	s := NewSession(cfg)

	if got := s.ParentSession(); got != cfg.ParentSession {
		t.Fatalf("fresh session ParentSession() = %q, want %q", got, cfg.ParentSession)
	}

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ParentSession(); got != cfg.ParentSession {
		t.Errorf("loaded ParentSession() = %q, want %q", got, cfg.ParentSession)
	}
}

// TestParentSessionAbsentByDefault verifies a session created without
// Config.ParentSession round-trips as empty — the common case, and the one
// that must never regress: most sessions have no lineage.
func TestParentSessionAbsentByDefault(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	if got := s.ParentSession(); got != "" {
		t.Fatalf("fresh session ParentSession() = %q, want empty", got)
	}
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ParentSession(); got != "" {
		t.Errorf("loaded ParentSession() = %q, want empty", got)
	}
}

// TestParentSessionRestoreWinsOverLoadingConfig mirrors
// TestWorkDirRestoreWinsOverLoadingConfig: the persisted header value is the
// durable truth for a resumed session, overriding whatever ParentSession the
// LoadSession caller happens to supply.
func TestParentSessionRestoreWinsOverLoadingConfig(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(dir, prov)
	cfg.ParentSession = "ses_0000000000000001"
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	loadCfg := cfg
	loadCfg.ParentSession = "ses_0000000000000002"
	loaded, err := LoadSession(loadCfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ParentSession(); got != "ses_0000000000000001" {
		t.Errorf("loaded ParentSession() = %q, want restored value %q", got, "ses_0000000000000001")
	}
}

// TestParentSessionLegacyHeaderCompat verifies a session log written before
// this field existed (no "parent_session" in its header) keeps current
// behavior on load: absent, regardless of the loading Config.
func TestParentSessionLegacyHeaderCompat(t *testing.T) {
	dir := t.TempDir()
	id := "ses_4444444444444445"
	data := `{"type":"session","id":"ses_4444444444444445","created_at":"2025-01-02T03:04:05Z"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"hi"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{SessionDir: dir, ParentSession: "ses_0000000000000003", Model: message.ModelRef{Provider: "test", Model: "m1"}}
	s, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ParentSession(); got != "ses_0000000000000003" {
		t.Errorf("legacy header ParentSession() = %q, want caller's Config.ParentSession %q unchanged", got, "ses_0000000000000003")
	}
}
