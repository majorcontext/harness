package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// persistCfg returns a Config wired to a scripted provider and a session dir.
func persistCfg(dir string, prov *scriptedProvider) Config {
	return Config{
		Providers:  provider.Registry{prov.name: prov},
		Model:      message.ModelRef{Provider: prov.name, Model: "m1"},
		SessionDir: dir,
	}
}

func historyJSON(t *testing.T, h []message.Message) string {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			&message.Text{Text: "running"},
			toolCall("tc1", "bash", `{"command":"echo round-trip"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != s.ID {
		t.Errorf("loaded ID = %q, want %q", loaded.ID, s.ID)
	}
	if got, want := historyJSON(t, loaded.History()), historyJSON(t, s.History()); got != want {
		t.Errorf("loaded history = %s\nwant %s", got, want)
	}
	if loaded.Model() != s.Model() {
		t.Errorf("loaded model = %v, want %v", loaded.Model(), s.Model())
	}
}

func TestPersistModelChangeReplay(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "one"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "two"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	swapped := message.ModelRef{Provider: "test", Model: "m2"}
	s.SetModel(swapped)
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Model() != swapped {
		t.Errorf("loaded model = %v, want %v", loaded.Model(), swapped)
	}
	if len(loaded.History()) != 4 {
		t.Errorf("loaded history = %d messages, want 4", len(loaded.History()))
	}
}

func TestLoadSessionContinuesSameFile(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "one"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "two"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if err := loaded.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	reloaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.History()) != 4 {
		t.Errorf("reloaded history = %d messages, want 4", len(reloaded.History()))
	}
}

func TestLoadSessionTruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	id := "ses_trunc"
	data := `{"type":"session","id":"ses_trunc","created_at":"2025-01-02T03:04:05Z"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"hi"}]}}
{"type":"message","message":{"id":"msg_2","ro`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir, Model: message.ModelRef{Provider: "p", Model: "m"}}, id)
	if err != nil {
		t.Fatalf("LoadSession = %v, want truncated final line tolerated", err)
	}
	h := s.History()
	if len(h) != 1 || h[0].ID != "msg_1" {
		t.Errorf("history = %+v, want just msg_1", h)
	}
	// Model falls back to Config.Model with no model records.
	if want := (message.ModelRef{Provider: "p", Model: "m"}); s.Model() != want {
		t.Errorf("model = %v, want %v", s.Model(), want)
	}
}

func TestLoadSessionCorruptMiddleLine(t *testing.T) {
	dir := t.TempDir()
	id := "ses_corrupt"
	data := `{"type":"session","id":"ses_corrupt","created_at":"2025-01-02T03:04:05Z"}
{"type":"message","message":{"id":"msg_1","ro
{"type":"message","message":{"id":"msg_2","role":"user","parts":[{"type":"text","text":"hi"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadSession(Config{SessionDir: dir}, id); err == nil {
		t.Fatal("LoadSession succeeded, want error for corrupt middle line")
	}
}

// TestPersistCreatesLogBeforePrompt verifies that Persist gives a
// never-prompted session a durable on-disk backing (header + model record), so
// it survives eviction from memory and reloads with its model intact.
func TestPersistCreatesLogBeforePrompt(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if infos, err := ListSessions(dir); err != nil || len(infos) != 0 {
		t.Fatalf("precondition: ListSessions = %+v, %v; want empty (lazy log)", infos, err)
	}

	if err := s.Persist(); err != nil {
		t.Fatalf("Persist = %v", err)
	}

	infos, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != s.ID {
		t.Fatalf("after Persist: ListSessions = %+v, want [%s]", infos, s.ID)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession after Persist: %v", err)
	}
	if loaded.Model() != cfg.Model {
		t.Errorf("loaded model = %v, want %v", loaded.Model(), cfg.Model)
	}

	// Persist is idempotent: a second call adds no records.
	before, _ := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err := s.Persist(); err != nil {
		t.Fatalf("second Persist = %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if string(before) != string(after) {
		t.Errorf("second Persist changed the log:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestListSessionsSkipsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	good := `{"type":"session","id":"ses_good","created_at":"2025-01-02T03:04:05Z"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"hi"}]}}
`
	// Corrupt middle line: the shared corruption rule (scanLog) must make
	// this file unlistable without breaking the listing of others.
	bad := `{"type":"session","id":"ses_bad","created_at":"2025-01-02T03:04:05Z"}
{"type":"message","message":{"id":"msg_1","ro
{"type":"message","message":{"id":"msg_2","role":"user","parts":[{"type":"text","text":"hi"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, "ses_good.jsonl"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ses_bad.jsonl"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	infos, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != "ses_good" {
		t.Errorf("ListSessions = %+v, want just ses_good", infos)
	}
}

func TestListSessions(t *testing.T) {
	dir := t.TempDir()

	// Missing dir: empty list, no error.
	got, err := ListSessions(filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatalf("ListSessions(missing) = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListSessions(missing) = %v, want empty", got)
	}

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "a"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "b"}),
	}}
	cfg := persistCfg(dir, prov)
	s1 := NewSession(cfg)
	if _, err := s1.Prompt(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	s2 := NewSession(cfg)
	if _, err := s2.Prompt(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}

	infos, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("ListSessions = %d entries, want 2", len(infos))
	}
	byID := make(map[string]SessionInfo)
	for _, in := range infos {
		byID[in.ID] = in
	}
	for _, s := range []*Session{s1, s2} {
		in, ok := byID[s.ID]
		if !ok {
			t.Fatalf("session %s missing from list: %+v", s.ID, infos)
		}
		if in.Messages != 2 {
			t.Errorf("session %s Messages = %d, want 2", s.ID, in.Messages)
		}
		if in.CreatedAt.IsZero() {
			t.Errorf("session %s CreatedAt is zero", s.ID)
		}
	}
}

func TestNoSessionDirWritesNothing(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(dir, prov)
	cfg.SessionDir = ""
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Errorf("PersistErr = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dir has entries %v, want none", entries)
	}
}

func TestLazyFileCreation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	s.SetModel(message.ModelRef{Provider: "test", Model: "m2"})

	// Nothing on disk until the first message append.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("session dir exists before first append (stat err = %v)", err)
	}

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, s.ID+".jsonl")); err != nil {
		t.Errorf("session file missing after first append: %v", err)
	}
}

func TestPersistErrSurfacesWriteFailure(t *testing.T) {
	base := t.TempDir()
	// A file where the session dir should be: MkdirAll must fail.
	blocker := filepath.Join(base, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(filepath.Join(blocker, "sessions"), prov)
	s := NewSession(cfg)

	// The loop must not crash on a persistence failure.
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if s.PersistErr() == nil {
		t.Error("PersistErr = nil, want error")
	}
}

func TestPersistModelSetBeforeFirstAppend(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	// SetModel before anything is on disk: the persisted log must still
	// name the swapped model explicitly.
	swapped := message.ModelRef{Provider: "test", Model: "m2"}
	s.SetModel(swapped)

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Model() != swapped {
		t.Errorf("loaded model = %v, want %v", loaded.Model(), swapped)
	}
}

func TestFirstAppendFileLayout(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	// The header and the initial model record are written with a single
	// Write call: a transient failure between them would otherwise leave a
	// header-only file that retries (gated on size == 0) never complete,
	// permanently dropping the model record. With one write the worst case
	// under a mid-write crash is a truncated final line, which LoadSession
	// already tolerates. This test pins the resulting on-disk layout: line 1
	// session header, line 2 model record, line 3 first message.
	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("session file has %d lines, want at least 3:\n%s", len(lines), data)
	}
	var recs []record
	for i, line := range lines {
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		recs = append(recs, rec)
	}
	if recs[0].Type != recSession || recs[0].ID != s.ID {
		t.Errorf("line 1 = %+v, want session header with ID %q", recs[0], s.ID)
	}
	if recs[1].Type != recModel || recs[1].Model != s.Model() {
		t.Errorf("line 2 = %+v, want model record %v", recs[1], s.Model())
	}
	if recs[2].Type != recMessage || recs[2].Message == nil || recs[2].Message.Role != message.RoleUser {
		t.Errorf("line 3 = %+v, want first (user) message record", recs[2])
	}
}
