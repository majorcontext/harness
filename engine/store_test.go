package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

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

// TestPersistReasoningEmptyProviderData is the round-2 forensic regression
// guard reconstructed at the full worker-turn level: a scripted provider
// producing the exact incident shape — an assistant message whose Reasoning
// part carries a present-but-zero-length (non-nil) provider_data entry, the
// map-indirected twin of the ToolCall.Arguments footgun #42 fixed (see
// message.ProviderData's doc comment) — must not break the turn. Before
// message.ProviderData grew its MarshalJSON/Get guards, this failed inside
// engine.Session.append's persistMessage -> json.Marshal(rec) call with
// exactly "json: error calling MarshalJSON for type json.RawMessage:
// unexpected end of JSON input", the production error, and (unlike a
// provider-transcode failure) that particular call site swallows its error
// into PersistErr rather than returning it from Prompt — which is why this
// test also asserts PersistErr and a clean reload, not just that Prompt
// itself succeeds. This exercises the exact path the incident logs show:
// worker turn -> assemble message -> persist to the session log, no
// provider or transcoder involved, proving the fix lives at the message
// layer and protects every producer, not just the shipped providers'
// currently-safe ones.
func TestPersistReasoningEmptyProviderData(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn,
			&message.Reasoning{
				Text:         "thinking it through",
				ProviderData: message.ProviderData{"test": json.RawMessage{}},
			},
			&message.Text{Text: "done"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v, want nil", err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got, want := historyJSON(t, loaded.History()), historyJSON(t, s.History()); got != want {
		t.Errorf("loaded history = %s\nwant %s", got, want)
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

// TestLoadSessionRejectsPathTraversalID is the RED test for defense in
// depth: LoadSession builds a filesystem path from id ("<SessionDir>/<id>
// .jsonl"), and callers outside the HTTP boundary — notably the CLI's -r/-c
// resume flags, which call engine.LoadSession directly — never pass through
// server/handlers.go's ValidSessionID check. So LoadSession must validate id
// itself, before sessionPath is ever built.
//
// SessionDir points at a directory that does not exist on disk, so if
// LoadSession reached os.ReadFile before validating, it would fail with a
// generic *fs.PathError ("no such file or directory") instead of the
// validation-specific ErrInvalidSessionID — proving the id is rejected
// without the filesystem ever being touched.
func TestLoadSessionRejectsPathTraversalID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := LoadSession(Config{SessionDir: dir}, "../../etc/passwd")
	if err == nil {
		t.Fatal("LoadSession succeeded, want error for path-traversal-shaped id")
	}
	if !errors.Is(err, ErrInvalidSessionID) {
		t.Errorf("error = %v, want wrapping ErrInvalidSessionID", err)
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		t.Errorf("error = %v is a filesystem error — LoadSession touched disk before validating the id", err)
	}
}

func TestLoadSessionTruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	id := "ses_5555555555555555"
	data := `{"type":"session","id":"ses_5555555555555555","created_at":"2025-01-02T03:04:05Z"}
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
	id := "ses_6666666666666666"
	data := `{"type":"session","id":"ses_6666666666666666","created_at":"2025-01-02T03:04:05Z"}
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

// TestListSessionsOrdersByCreatedAtNotID pins the existing ordering rule
// (created_at, not lexicographic ID) through the switch to TypeID: a legacy
// "ses_" + hex ID sorts alphabetically before any "ses_..." TypeID (both
// share the "ses_" prefix, and hex digits sort before TypeID's base32
// alphabet), so if ListSessions ever regressed to sorting by ID this legacy
// fixture — despite being created LAST — would wrongly land first. Giving it
// the latest created_at and asserting it lands last catches that.
func TestListSessionsOrdersByCreatedAtNotID(t *testing.T) {
	dir := t.TempDir()

	const legacyID = "ses_0000000000000000" // sorts before any "ses_<base32>..." TypeID
	legacy := `{"type":"session","id":"` + legacyID + `","created_at":"2030-01-01T00:00:00Z"}
{"type":"model","model":"test/m1"}
`
	if err := os.WriteFile(filepath.Join(dir, legacyID+".jsonl"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "a"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}

	infos, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("ListSessions = %d entries, want 2", len(infos))
	}
	if legacyID > s.ID {
		t.Fatalf("test fixture invalid: legacy ID %q must sort before TypeID %q for this test to be meaningful", legacyID, s.ID)
	}
	// created_at order: s (created first, in 2025-ish "now") before the
	// legacy fixture (stamped 2030), which is the OPPOSITE of ID order.
	if infos[0].ID != s.ID || infos[1].ID != legacyID {
		t.Errorf("ListSessions order = [%s, %s], want [%s, %s] (created_at order, not ID order)",
			infos[0].ID, infos[1].ID, s.ID, legacyID)
	}
}

// TestLoadLegacySessionFixture is the RED test for legacy read-compat: a
// session log written by the pre-TypeID engine (id "ses_" + 16 hex digits,
// no TypeID in sight) must still LoadSession, appear in ListSessions, and
// accept a new Prompt — the on-disk format of existing sessions never
// changes shape just because newID now mints TypeIDs.
func TestLoadLegacySessionFixture(t *testing.T) {
	dir := t.TempDir()
	const legacyID = "ses_0123456789abcdef"
	fixture := `{"type":"session","id":"ses_0123456789abcdef","created_at":"2025-01-02T03:04:05Z"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_0000000000000001","role":"user","parts":[{"type":"text","text":"hello from the past"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, legacyID+".jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "reply"}),
	}}
	cfg := persistCfg(dir, prov)

	loaded, err := LoadSession(cfg, legacyID)
	if err != nil {
		t.Fatalf("LoadSession(legacy) = %v", err)
	}
	if loaded.ID != legacyID {
		t.Errorf("loaded.ID = %q, want %q", loaded.ID, legacyID)
	}
	if len(loaded.History()) != 1 {
		t.Fatalf("loaded history = %d messages, want 1", len(loaded.History()))
	}

	infos, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != legacyID {
		t.Fatalf("ListSessions = %+v, want just %q", infos, legacyID)
	}
	if infos[0].Messages != 1 {
		t.Errorf("ListSessions Messages = %d, want 1", infos[0].Messages)
	}

	if !ValidSessionID(legacyID) {
		t.Errorf("ValidSessionID(%q) = false, want true", legacyID)
	}

	// Promptable: appending to a legacy-id session must keep working.
	if _, err := loaded.Prompt(context.Background(), "hi again"); err != nil {
		t.Fatalf("Prompt on legacy session = %v", err)
	}
	if len(loaded.History()) != 3 {
		t.Errorf("history after prompt = %d messages, want 3", len(loaded.History()))
	}
}

// TestGoalStalledRecordRoundTrip is the forensic regression guard for the
// goal-supervised session incident (ses_01kx3pvqttfwgbf2n5x1f1y8yh.jsonl): a
// worker turn failed with "json: error calling MarshalJSON for type
// json.RawMessage: unexpected end of JSON input" (see
// TestToolCallEmptyArgumentsMarshal in the message package for the exact
// reproduction), which promptTurnWithRetry records as a goal.stalled record
// via recordGoalStalled -> persistGoalLocked. That record's payload
// (goalRecord) carries the failure only as a plain string Reason field, so
// the failure that produced it can never itself poison the record: every
// goal.stalled line landed by a real transient failure must be complete,
// valid JSON, and must round-trip through LoadSession without disturbing
// the session's resumed goal state.
func TestGoalStalledRecordRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		prov := &goalProvider{
			workerErrN: 1, // one transient failure -> exactly one goal.stalled record
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
			},
			eval: [][]provider.Event{evalTurn("MET: ok")},
		}
		s := goalSession(t, prov, dir)
		cfg := s.cfg

		res, err := s.PursueGoal(context.Background(), "the condition", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal = %v", err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved once the transient stall clears", res)
		}
		if err := s.PersistErr(); err != nil {
			t.Fatalf("PersistErr = %v", err)
		}

		raw, err := os.ReadFile(sessionPath(dir, s.ID))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		var sawStalled bool
		for i, line := range lines {
			// Every line, including the goal.stalled record, must be
			// complete and independently valid JSON — the incident's log
			// was not actually corrupt at this point (a truncated final
			// line is a distinct, already-covered case; see
			// TestLoadSessionTruncatedFinalLine), but this is the
			// assertion that would catch it if persistGoalLocked ever
			// wrote a partial line on a marshal failure.
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("line %d is not valid, complete JSON: %v\nline: %s", i+1, err, line)
			}
			if rec["type"] == "goal.stalled" {
				sawStalled = true
				reason, _ := rec["goal"].(map[string]any)["reason"].(string)
				if reason == "" {
					t.Error("goal.stalled record missing goal.reason")
				}
			}
		}
		if !sawStalled {
			t.Fatalf("session log has no goal.stalled record:\n%s", raw)
		}

		loaded, err := LoadSession(cfg, s.ID)
		if err != nil {
			t.Fatalf("LoadSession = %v", err)
		}
		if _, ok := loaded.ActiveGoal(); ok {
			t.Error("resumed ActiveGoal active after achievement — a goal.stalled record must not itself change resume state")
		}
	})
}

// TestLoadSessionTruncatedGoalStalledFinalLine proves scanLog's corruption
// discipline applies identically to a goal.stalled record: a truncated
// final line (the shape a crash mid-append would leave, per the incident's
// premise) is tolerated exactly like a truncated message record, and the
// resumed goal state reflects only the last complete record — it never lets
// a partial goal.stalled poison the session.
func TestLoadSessionTruncatedGoalStalledFinalLine(t *testing.T) {
	dir := t.TempDir()
	id := "ses_7777777777777777"
	data := `{"type":"session","id":"ses_7777777777777777","created_at":"2025-01-02T03:04:05Z"}
{"type":"goal.set","goal":{"condition":"ship it"}}
{"type":"goal.stalled","goal":{"reason":"json: error calling MarshalJSON f`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("LoadSession = %v, want the truncated final goal.stalled line tolerated", err)
	}
	cond, ok := s.ActiveGoal()
	if !ok || cond != "ship it" {
		t.Errorf("ActiveGoal = %q, %v; want active with the goal.set condition (the truncated goal.stalled contributes nothing)", cond, ok)
	}
}

// TestLoadSessionCorruptMiddleGoalStalledLine proves the other half of the
// same discipline: a corrupt goal.stalled record anywhere but the final line
// is a loud, structural error — never silently skipped like a truncated
// final line — because a record in the middle of the file can only be
// corrupt from a real bug, not an in-flight crash.
func TestLoadSessionCorruptMiddleGoalStalledLine(t *testing.T) {
	dir := t.TempDir()
	id := "ses_8888888888888888"
	data := `{"type":"session","id":"ses_8888888888888888","created_at":"2025-01-02T03:04:05Z"}
{"type":"goal.set","goal":{"condition":"ship it"}}
{"type":"goal.stalled","goal":{"reason":"broke
{"type":"goal.cleared","goal":{"reason":"worker turn failed"}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadSession(Config{SessionDir: dir}, id); err == nil {
		t.Fatal("LoadSession succeeded, want error for a corrupt goal.stalled line before the final line")
	}
}

func TestLoadSessionRepairsOrphanedToolCalls(t *testing.T) {
	// A log written by an older binary (or any external writer) can contain
	// an assistant message whose tool_call never got a tool_result — the
	// turn died between emitting the call and executing it. LoadSession
	// must repair the history at ingest so every downstream consumer (the
	// next prompt's request, GET /message, goal replay) sees a
	// protocol-valid history, durably — not just at transcode time.
	// Incident: ses_01kx48z4rqfkpbwmzfdv1jzeg6 (goal killed by Anthropic
	// 400 "tool_use ids were found without tool_result blocks").
	dir := t.TempDir()
	id := "ses_6666666666666666"
	data := `{"type":"session","id":"ses_6666666666666666","created_at":"2025-01-02T03:04:05Z"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"list files"}]}}
{"type":"message","message":{"id":"msg_2","role":"assistant","parts":[{"type":"tool_call","call_id":"toolu_dead","name":"bash","arguments":{"command":"ls"}}]}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir, Model: message.ModelRef{Provider: "p", Model: "m"}}, id)
	if err != nil {
		t.Fatal(err)
	}
	h := s.History()
	if len(h) != 3 {
		t.Fatalf("history length = %d, want 3 (user, assistant, synthetic tool result)", len(h))
	}
	last := h[2]
	if last.Role != message.RoleTool {
		t.Fatalf("h[2].Role = %q, want %q", last.Role, message.RoleTool)
	}
	var tr *message.ToolResult
	for _, p := range last.Parts {
		if r, ok := p.(*message.ToolResult); ok {
			tr = r
		}
	}
	if tr == nil {
		t.Fatalf("h[2] has no ToolResult part: %+v", last.Parts)
	}
	if tr.CallID != "toolu_dead" {
		t.Errorf("synthetic result CallID = %q, want %q", tr.CallID, "toolu_dead")
	}
	if !tr.IsError {
		t.Error("synthetic result IsError = false, want true")
	}
	if got := tr.Content.Text(); got != message.SyntheticOrphanResultText {
		t.Errorf("synthetic result text = %q, want %q", got, message.SyntheticOrphanResultText)
	}
}
