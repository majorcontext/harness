package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

var errMirrorBoom = errors.New("mirror boom")

type mirrorFixture struct {
	run1, run2 string
	sid        string
	path       string
	run1Lines  []any
	final      []any
}

func decodeLines(t *testing.T, data []byte) []any {
	t.Helper()
	var out []any
	for _, l := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var v any
		if err := json.Unmarshal(l, &v); err != nil {
			t.Fatalf("decoding fixture line: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func loadMirrorFixture(t *testing.T) mirrorFixture {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "claude-code-mirror"))
	if err != nil {
		t.Fatal(err)
	}
	f := mirrorFixture{run1: filepath.Join(dir, "run1.stdout.jsonl"), run2: filepath.Join(dir, "run2.stdout.jsonl")}
	raw, err := os.ReadFile(f.run1)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range decodeLines(t, raw) {
		m := v.(map[string]any)
		f.run1Lines = append(f.run1Lines, m)
		if m["type"] == "system" {
			f.sid = m["session_id"].(string)
		}
		if m["type"] == "transcript_mirror" && f.path == "" {
			f.path = strings.TrimPrefix(m["filePath"].(string), "/home/u/cfg/")
		}
	}
	final, err := os.ReadFile(filepath.Join(dir, "final.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	f.final = decodeLines(t, final)
	return f
}

func (f mirrorFixture) run1Entries() []any {
	var out []any
	for _, v := range f.run1Lines {
		m := v.(map[string]any)
		if m["type"] == "transcript_mirror" {
			out = append(out, m["entries"].([]any)...)
		}
	}
	return out
}

type mirrorEnv struct {
	t    *testing.T
	f    mirrorFixture
	seen string
	log  string
	root string
}

func newMirrorEnv(t *testing.T) *mirrorEnv {
	t.Helper()
	e := &mirrorEnv{t: t, f: loadMirrorFixture(t), root: t.TempDir()}
	e.seen = filepath.Join(t.TempDir(), "seen.jsonl")
	e.log = filepath.Join(t.TempDir(), "argv.jsonl")
	t.Setenv("FAKE_CLAUDE_MODE", "mirror")
	t.Setenv("FAKE_CLAUDE_SESSION_ID", e.f.sid)
	t.Setenv("FAKE_CLAUDE_MIRROR_SEEN", e.seen)
	t.Setenv("FAKE_CLAUDE_LOG", e.log)
	t.Setenv("FAKE_CLAUDE_MIRROR_FIXTURE", e.f.run1)
	return e
}

func (e *mirrorEnv) session(store SessionStore) *Session {
	e.t.Helper()
	return NewSession(e.config(store))
}

func (e *mirrorEnv) config(store SessionStore) Config {
	return Config{
		SessionStore: store,
		Model:        message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		ClaudeCode:   ClaudeCodeConfig{BinaryPath: buildFakeClaude(e.t), MirrorCLISession: true, ConfigRoot: e.root},
	}
}

type seenInvocation struct {
	ConfigDir string            `json:"config_dir"`
	Files     map[string]string `json:"files"`
}

func (e *mirrorEnv) seenInvocations() []seenInvocation {
	e.t.Helper()
	data, err := os.ReadFile(e.seen)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []seenInvocation
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var s seenInvocation
		if err := dec.Decode(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func prompt(t *testing.T, s *Session, text string) {
	t.Helper()
	if _, err := s.Prompt(context.Background(), text); err != nil {
		t.Fatalf("Prompt(%q): %v", text, err)
	}
}

func mirrorEntries(t *testing.T, store SessionStore, id string) (path string, entries []any) {
	t.Helper()
	recs, err := store.Load(cliLogID(id))
	if err != nil {
		t.Fatalf("Load(cliLogID): %v", err)
	}
	var p cliMirrorPath
	if err := json.Unmarshal(recs[0], &p); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs[1:] {
		var v any
		if err := json.Unmarshal(r, &v); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, v)
	}
	return p.Path, entries
}

func TestMirrorArgv(t *testing.T) {
	e := newMirrorEnv(t)
	s := e.session(NewMemStore())
	prompt(t, s, "hi")
	cfg := e.config(NewMemStore())
	cfg.ClaudeCode.MirrorCLISession = false
	off := NewSession(cfg)
	prompt(t, off, "hi")
	inv := readInvocations(t, e.log)
	if !argvContains(inv[0], "--session-mirror") {
		t.Errorf("argv with MirrorCLISession lacks --session-mirror: %v", inv[0])
	}
	if argvContains(inv[1], "--session-mirror") {
		t.Errorf("argv without MirrorCLISession has --session-mirror: %v", inv[1])
	}
}

func TestMirrorAppendsFrameEntries(t *testing.T) {
	e := newMirrorEnv(t)
	store := NewMemStore()
	s := e.session(store)
	prompt(t, s, "hi")
	path, entries := mirrorEntries(t, store, s.ID)
	if want := e.f.path; path != want {
		t.Errorf("path record = %q, want %q", path, want)
	}
	if want := e.f.run1Entries(); !reflect.DeepEqual(entries, want) {
		t.Errorf("mirrored entries = %v\nwant %v", entries, want)
	}
}

func TestMirrorResumeAppendsOnlyNewLines(t *testing.T) {
	e := newMirrorEnv(t)
	store := NewMemStore()
	s := e.session(store)
	prompt(t, s, "one")
	t.Setenv("FAKE_CLAUDE_MIRROR_FIXTURE", e.f.run2)
	prompt(t, s, "two")
	_, entries := mirrorEntries(t, store, s.ID)
	if !reflect.DeepEqual(entries, e.f.final) {
		t.Errorf("mirrored entries = %v\nwant the final file %v", entries, e.f.final)
	}
}

func TestMirrorRestoredBeforeResume(t *testing.T) {
	e := newMirrorEnv(t)
	store := NewMemStore()
	s := e.session(store)
	prompt(t, s, "one")

	fresh, err := LoadSession(e.config(store), s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	e.root = t.TempDir()
	fresh.cfg.ClaudeCode.ConfigRoot = e.root
	t.Setenv("FAKE_CLAUDE_MIRROR_FIXTURE", e.f.run2)
	prompt(t, fresh, "two")

	seen := e.seenInvocations()
	got, ok := seen[1].Files[e.f.path]
	if !ok {
		t.Fatalf("second spawn saw files %v, want %s", reflect.ValueOf(seen[1].Files).MapKeys(), e.f.path)
	}
	var restored []any
	for _, v := range decodeLines(t, []byte(got)) {
		restored = append(restored, v)
	}
	if want := e.f.run1Entries(); !reflect.DeepEqual(restored, want) {
		t.Errorf("restored file = %v\nwant the stored entries %v", restored, want)
	}
	if v, ok := argvValueAfter(readInvocations(t, e.log)[1], "--resume"); !ok || v != e.f.sid {
		t.Errorf("--resume = %q, ok=%v, want %s", v, ok, e.f.sid)
	}
}

func TestMirrorScratchDirIsPerTurnAndRemoved(t *testing.T) {
	e := newMirrorEnv(t)
	s := e.session(NewMemStore())
	prompt(t, s, "one")
	prompt(t, s, "two")
	seen := e.seenInvocations()
	if seen[0].ConfigDir == seen[1].ConfigDir {
		t.Errorf("both turns used config dir %s", seen[0].ConfigDir)
	}
	for _, inv := range seen {
		if filepath.Dir(inv.ConfigDir) != filepath.Clean(e.root) && filepath.Dir(inv.ConfigDir) != resolvedPath(t, e.root) {
			t.Errorf("config dir %s is not under ConfigRoot %s", inv.ConfigDir, e.root)
		}
		if _, err := os.Stat(inv.ConfigDir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("config dir %s survived the turn: %v", inv.ConfigDir, err)
		}
	}
}

func resolvedPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type mirrorFailStore struct {
	SessionStore
	appendErr error
}

func (m *mirrorFailStore) Append(id string, at int, recs ...[]byte) error {
	if strings.HasSuffix(id, cliLogSuffix) && m.appendErr != nil {
		return m.appendErr
	}
	return m.SessionStore.Append(id, at, recs...)
}

func TestMirrorAppendFailureFailsTurn(t *testing.T) {
	e := newMirrorEnv(t)
	s := e.session(&mirrorFailStore{SessionStore: NewMemStore(), appendErr: errMirrorBoom})
	_, err := s.Prompt(context.Background(), "hi")
	if !errors.Is(err, errMirrorBoom) {
		t.Fatalf("Prompt error = %v, want it to wrap errMirrorBoom", err)
	}
}

type mirrorRacingStore struct {
	SessionStore
	raced bool
}

func (m *mirrorRacingStore) Append(id string, at int, recs ...[]byte) error {
	if strings.HasSuffix(id, cliLogSuffix) && !m.raced {
		m.raced = true
		if err := m.SessionStore.Append(id, at, []byte(`{"path":"other/x.jsonl"}`)); err != nil {
			return err
		}
	}
	return m.SessionStore.Append(id, at, recs...)
}

func TestMirrorAppendConflictFailsTurn(t *testing.T) {
	e := newMirrorEnv(t)
	s := e.session(&mirrorRacingStore{SessionStore: NewMemStore()})
	_, err := s.Prompt(context.Background(), "hi")
	if !errors.Is(err, ErrAppendConflict) {
		t.Fatalf("Prompt error = %v, want it to wrap ErrAppendConflict", err)
	}
	s.mu.Lock()
	fenced := s.fenced
	s.mu.Unlock()
	if !errors.Is(fenced, ErrAppendConflict) {
		t.Fatalf("fenced = %v, want ErrAppendConflict", fenced)
	}
	if _, err := s.Prompt(context.Background(), "again"); !errors.Is(err, ErrAppendConflict) {
		t.Errorf("Prompt on a fenced session = %v, want it to wrap ErrAppendConflict", err)
	}
	if n := len(readInvocations(t, e.log)); n != 1 {
		t.Errorf("CLI spawns = %d, want 1: a fenced session runs no turn", n)
	}
}

func TestMirrorGoalLoopDoesNotRetryMirrorFailure(t *testing.T) {
	e := newMirrorEnv(t)
	s := e.session(&mirrorFailStore{SessionStore: NewMemStore(), appendErr: errMirrorBoom})
	_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: message.ModelRef{Provider: "p", Model: "e"}})
	if !errors.Is(err, errMirrorBoom) {
		t.Fatalf("PursueGoal error = %v, want it to wrap errMirrorBoom", err)
	}
	if n := len(readInvocations(t, e.log)); n != 1 {
		t.Errorf("CLI spawns = %d, want exactly 1 after a mirror failure", n)
	}
}

func TestMirrorFailedFirstAppendLeavesNextTurnFresh(t *testing.T) {
	e := newMirrorEnv(t)
	store := &mirrorFailStore{SessionStore: NewMemStore(), appendErr: errMirrorBoom}
	s := e.session(store)
	if _, err := s.Prompt(context.Background(), "one"); !errors.Is(err, errMirrorBoom) {
		t.Fatalf("first Prompt = %v, want errMirrorBoom", err)
	}
	if s.claudeCodeSessionID() == "" {
		t.Fatal("journal holds no CLI session id after init, the premise of this test")
	}
	store.appendErr = nil
	prompt(t, s, "two")
	argv := readInvocations(t, e.log)[1]
	if argvContains(argv, "--resume") {
		t.Errorf("second turn argv has --resume with no mirror log: %v", argv)
	}
	if !argvContains(argv, claudeCodeHistoryDirective) {
		t.Errorf("second turn argv lacks the history directive: %v", argv)
	}
}

func TestMirrorRestoreFailureFailsTurn(t *testing.T) {
	e := newMirrorEnv(t)
	store := NewMemStore()
	s := e.session(store)
	if err := store.Append(cliLogID(s.ID), 0, []byte(`{"type":"user"}`)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Prompt(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), s.ID) {
		t.Fatalf("Prompt error = %v, want one naming session %s", err, s.ID)
	}
	if _, serr := os.Stat(e.log); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("child was spawned: stat(%s) = %v", e.log, serr)
	}
}

func TestMirrorFirstTurnHasNoResume(t *testing.T) {
	e := newMirrorEnv(t)
	store := NewMemStore()
	s := e.session(store)
	prompt(t, s, "hi")
	if argvContains(readInvocations(t, e.log)[0], "--resume") {
		t.Error("first turn argv has --resume")
	}
	if files := e.seenInvocations()[0].Files; len(files) != 0 {
		t.Errorf("first turn saw restored files %v, want none", files)
	}
	if n, err := store.Len(cliLogID(s.ID)); err != nil || n == 0 {
		t.Errorf("Len(cliLogID) = %d, %v, want a non-empty log", n, err)
	}
}

func TestMirrorRequiresStore(t *testing.T) {
	cfg := Config{
		Model:      message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		ClaudeCode: ClaudeCodeConfig{MirrorCLISession: true},
	}
	if err := NewSession(cfg).ConfigErr(); err == nil {
		t.Error("ConfigErr() = nil with MirrorCLISession and no store")
	}
	cfg.SessionStore = NewMemStore()
	if err := NewSession(cfg).ConfigErr(); err != nil {
		t.Errorf("ConfigErr() = %v with a store", err)
	}
}

func TestCLILogIDIsNotASessionID(t *testing.T) {
	if ValidSessionID(cliLogID("abc")) {
		t.Error("ValidSessionID(cliLogID(abc)) = true")
	}
	s := NewSession(Config{SessionStore: NewMemStore()})
	if ValidSessionID(cliLogID(s.ID)) {
		t.Errorf("ValidSessionID(cliLogID(%s)) = true", s.ID)
	}
}

func TestMirrorResumeAfterKillRestoresAppendedFrames(t *testing.T) {
	e := newMirrorEnv(t)
	store := NewMemStore()
	cfg := e.config(store)
	cfg.MaxTurnResumes = 3
	s := NewSession(cfg)
	t.Setenv("FAKE_CLAUDE_MIRROR_CRASH_AFTER", "1")
	if _, err := s.Prompt(context.Background(), "hi"); err == nil {
		t.Fatal("Prompt succeeded after the child was killed")
	}

	reloaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.ResumableTurn() {
		t.Fatal("ResumableTurn() = false after the kill")
	}
	t.Setenv("FAKE_CLAUDE_MIRROR_CRASH_AFTER", "")
	t.Setenv("FAKE_CLAUDE_MIRROR_FIXTURE", e.f.run2)
	if _, err := reloaded.ResumeTurn(context.Background()); err != nil {
		t.Fatalf("ResumeTurn: %v", err)
	}
	var want []any
	for _, v := range e.f.run1Lines {
		m := v.(map[string]any)
		if m["type"] == "transcript_mirror" {
			want = m["entries"].([]any)
			break
		}
	}
	seen := e.seenInvocations()
	var restored []any
	for _, v := range decodeLines(t, []byte(seen[1].Files[e.f.path])) {
		restored = append(restored, v)
	}
	if !reflect.DeepEqual(restored, want) {
		t.Errorf("restored file = %v\nwant the frames appended before the kill %v", restored, want)
	}
	if v, ok := argvValueAfter(readInvocations(t, e.log)[1], "--resume"); !ok || v != e.f.sid {
		t.Errorf("--resume = %q, ok=%v, want %s", v, ok, e.f.sid)
	}
}

func writeReorderedFixture(t *testing.T, e *mirrorEnv, keepInit bool, initAfterFirstFrame bool) {
	t.Helper()
	var init any
	var rest []any
	for _, v := range e.f.run1Lines {
		m := v.(map[string]any)
		if m["type"] == "system" && m["subtype"] == "init" {
			init = v
			continue
		}
		rest = append(rest, v)
	}
	var lines []any
	switch {
	case !keepInit:
		lines = rest
	case initAfterFirstFrame:
		lines = append(lines, rest[0], init)
		lines = append(lines, rest[1:]...)
	default:
		lines = append([]any{init}, rest...)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_MIRROR_FIXTURE", path)
}

func TestNoToolsRefusesEventsBeforeInit(t *testing.T) {
	e := newMirrorEnv(t)
	writeReorderedFixture(t, e, false, false)
	store := NewMemStore()
	cfg := e.config(store)
	cfg.ClaudeCode.DisableBuiltinTools = true
	s := NewSession(cfg)
	_, err := s.Prompt(context.Background(), "hi")
	if !errors.Is(err, ErrClaudeCodeBuiltinTools) {
		t.Fatalf("Prompt error = %v, want ErrClaudeCodeBuiltinTools", err)
	}
	for _, m := range s.History() {
		if m.Role == message.RoleAssistant {
			t.Errorf("assistant message journaled with no init event: %+v", m)
		}
	}
}

func TestNoToolsMirrorFrameBeforeInitStillMirrored(t *testing.T) {
	e := newMirrorEnv(t)
	writeReorderedFixture(t, e, true, true)
	store := NewMemStore()
	cfg := e.config(store)
	cfg.ClaudeCode.DisableBuiltinTools = true
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, entries := mirrorEntries(t, store, s.ID)
	if want := e.f.run1Entries(); !reflect.DeepEqual(entries, want) {
		t.Errorf("mirrored entries = %v\nwant %v", entries, want)
	}
}
