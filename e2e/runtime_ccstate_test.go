package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// stateChunkBytes is the bound of one chunk of Claude Code state (spec
// Backend state).
const stateChunkBytes = 4 << 20

const (
	ccSession = "claude-code"
	ccWorking = "Working on it."
)

func TestContractRuntimeClaudeCodeStateChunks(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"claude_code_state_is_saved_as_chunks_that_fit_every_batch", rowCCStateChunksFitEveryBatch},
		{"claude_code_state_resumes_the_cli_on_a_new_runtime", rowCCStateResumesOnANewRuntime},
		{"claude_code_state_continues_its_chain_after_a_handoff", rowCCStateContinuesAfterAHandoff},
		{"claude_code_state_of_a_new_cli_session_starts_a_new_chain", rowCCStateNewSessionStartsANewChain},
		{"claude_code_state_of_one_blob_resumes_and_the_next_save_starts_a_chain", rowCCStateOneBlobStartsAChain},
		{"claude_code_state_keeps_mirror_frames_that_arrive_before_the_init_frame", rowCCStateKeepsEarlyMirrors},
	}
	for _, row := range rows {
		t.Run(row.name, row.run)
	}
}

func ccEntry(tag string, size int) string {
	pad := max(size-len(tag)-24, 0)
	return fmt.Sprintf(`{"tag":%q,"pad":%q}`, tag, strings.Repeat(tag[len(tag)-1:], pad))
}

func ccEntries(prefix string, n, size int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = ccEntry(fmt.Sprintf("%s%02d", prefix, i), size)
	}
	return out
}

func ccTranscript(entries ...[]string) string {
	var all []string
	for _, e := range entries {
		all = append(all, e...)
	}
	return strings.Join(all, "\n") + "\n"
}

// ccFixture writes the stdout of one fakeclaude spawn: the CLI session id,
// one mirror frame for each entry, and a result.
func ccFixture(t *testing.T, session string, entries []string) string {
	t.Helper()
	return ccFixtureOrdered(t, session, entries, false)
}

// ccFixtureOrdered is ccFixture with the mirror frames before the init frame
// when early is set, the order of a recorded CLI run.
func ccFixtureOrdered(t *testing.T, session string, entries []string, early bool) string {
	t.Helper()
	file := "/home/u/cfg/projects/p/" + session + ".jsonl"
	var b strings.Builder
	line := func(v any) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	init := func() {
		line(map[string]any{"type": "system", "subtype": "init", "session_id": session, "model": "claude-haiku-4-5-20251001", "tools": []string{}})
	}
	if !early {
		init()
	}
	for _, e := range entries {
		b.WriteString(`{"type":"transcript_mirror","filePath":` + fmt.Sprintf("%q", file) + `,"entries":[` + e + "]}\n")
	}
	if early {
		init()
	}
	line(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "ok"}}}})
	line(map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "result": "ok", "session_id": session, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
	path := filepath.Join(t.TempDir(), session+".stdout.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ccLane runs Claude Code sessions on fakeclaude over one Store, with a fake
// control plane as the Sync receiver.
type ccLane struct {
	t     *testing.T
	st    harness.Store
	rcv   *syncReceiver
	seen  string
	argv  string
	epoch int
}

// newCCLane starts a lane whose spawns replay fixtures in order, the last
// one for each later spawn. hangAfter is the FAKE_CLAUDE_MIRROR_HANG_AFTER list.
func newCCLane(t *testing.T, hangAfter string, fixtures ...string) *ccLane {
	t.Helper()
	t.Setenv("HARNESS_E2E_KEY", "k")
	dir := t.TempDir()
	l := &ccLane{t: t, st: harness.NewMemStore(), rcv: newSyncReceiver(t, nil), seen: filepath.Join(dir, "seen.jsonl"), argv: filepath.Join(dir, "argv.jsonl")}
	for k, v := range map[string]string{
		"FAKE_CLAUDE_MODE": "mirror", "FAKE_CLAUDE_STATE": filepath.Join(dir, "state"), "FAKE_CLAUDE_MIRROR_FIXTURE": strings.Join(fixtures, ","),
		"FAKE_CLAUDE_MIRROR_SEEN": l.seen, "FAKE_CLAUDE_LOG": l.argv, "FAKE_CLAUDE_MIRROR_HANG_AFTER": hangAfter,
	} {
		t.Setenv(k, v)
	}
	return l
}

func (l *ccLane) runtime() *harness.Runtime {
	l.t.Helper()
	l.epoch++
	cfg := syncConfig(l.t, l.epoch, l.rcv, "http://127.0.0.1:1", map[string]any{
		"model": "claude-code/sonnet",
		"providers": map[string]any{"claude-code": map[string]any{
			"type": "claude-code-cli", "binary_path": fakeClaudePath(), "session_mirror": true}},
	})
	r, err := harness.New(harness.Options{Store: l.st, Config: cfg})
	if err != nil {
		l.t.Fatal(err)
	}
	return r
}

func (l *ccLane) close(r *harness.Runtime) {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(l.t.Context(), waitBound)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		l.t.Fatalf("Close = %v, want nil", err)
	}
}

func (l *ccLane) create(r *harness.Runtime) *harness.Session {
	l.t.Helper()
	s, err := r.Create(l.t.Context(), protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet"})
	if err != nil {
		l.t.Fatal(err)
	}
	return s
}

func (l *ccLane) open(r *harness.Runtime) *harness.Session {
	l.t.Helper()
	s, err := r.Open(l.t.Context(), "s1")
	if err != nil {
		l.t.Fatal(err)
	}
	return s
}

func (l *ccLane) send(s *harness.Session, id string) {
	l.t.Helper()
	if _, err := s.Submit(l.t.Context(), protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: "go " + id}}}); err != nil {
		l.t.Fatal(err)
	}
}

// awaitText returns once the session shows an item with text.
func (l *ccLane) awaitText(s *harness.Session, text string) {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(l.t.Context(), waitBound)
	defer cancel()
	for e, err := range s.Events(ctx, 0) {
		if err != nil {
			l.t.Fatalf("waiting for %q: %v", text, err)
		}
		if (e.Kind == protocol.KindItemDelta || e.Kind == "item.completed") && strings.Contains(string(e.Data), text) {
			return
		}
	}
}

// seenFiles returns, for each spawn in order, the transcripts that the CLI
// found in its config directory at start.
func (l *ccLane) seenFiles() []map[string]string {
	l.t.Helper()
	data, err := os.ReadFile(l.seen)
	if err != nil {
		l.t.Fatal(err)
	}
	var out []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var s struct {
			Files map[string]string `json:"files"`
		}
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			l.t.Fatal(err)
		}
		out = append(out, s.Files)
	}
	return out
}

// resumed returns the --resume value of each spawn in order, "" for none.
func (l *ccLane) resumed() []string {
	l.t.Helper()
	data, err := os.ReadFile(l.argv)
	if err != nil {
		l.t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			l.t.Fatal(err)
		}
		resume, _ := argvFacts(argv)["resume"].(string)
		out = append(out, resume)
	}
	return out
}

type ccRecord struct {
	Seq  uint64          `json:"seq"`
	Kind string          `json:"k"`
	Data json.RawMessage `json:"d"`
}

type ccState struct {
	Backend string `json:"backend"`
	Chunk   string `json:"chunk"`
	Restart bool   `json:"restart"`
	BlobKey string `json:"blob_key"`
}

// log returns the records of s1.
func (l *ccLane) log() []ccRecord {
	l.t.Helper()
	var out []ccRecord
	for _, data := range readAll(l.t, l.st, "s1") {
		var r ccRecord
		if err := json.Unmarshal(data, &r); err != nil {
			l.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// states returns each backend.state record of s1 with the seq of the
// owner.acquired record that fenced the owner that wrote it.
func (l *ccLane) states() (recs []ccState, fences []uint64) {
	l.t.Helper()
	var fence uint64
	for _, r := range l.log() {
		switch r.Kind {
		case "owner.acquired":
			fence = r.Seq
		case "backend.state":
			var s ccState
			if err := json.Unmarshal(r.Data, &s); err != nil {
				l.t.Fatal(err)
			}
			recs, fences = append(recs, s), append(fences, fence)
		}
	}
	return recs, fences
}

func (l *ccLane) blob(key string) []byte {
	l.t.Helper()
	rc, err := l.st.GetBlob(l.t.Context(), "s1", key)
	if err != nil {
		l.t.Fatalf("blob %s: %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		l.t.Fatal(err)
	}
	return data
}

func rowCCStateChunksFitEveryBatch(t *testing.T) {
	const entry = 3 << 19
	first, second := ccEntries("a", 16, entry), append(ccEntries("b", 2, entry), ccEntry("big", 5<<20))
	l := newCCLane(t, "", ccFixture(t, "S", first), ccFixture(t, "S", second))
	r := l.runtime()
	s := l.create(r)
	l.send(s, "1")
	awaitTurns(t, s, 1)
	l.send(s, "2")
	awaitTurns(t, s, 2)
	l.close(r)
	equalLogs(t, l.st, l.rcv.store, "s1")
	carried, chunks, single := map[string]int{}, 0, false
	for i, c := range l.rcv.snapshot() {
		if c.size > syncBodyCap {
			t.Errorf("batch %d is %d bytes, want at most %d", i, c.size, syncBodyCap)
		}
		for key, blob := range c.batch.Blobs {
			if !strings.HasPrefix(key, ccSession+"-") {
				continue
			}
			chunks++
			if n, ok := carried[key]; ok {
				t.Errorf("chunk %s is in batches %d and %d, want one", key, n, i)
			}
			carried[key] = i
			entries := bytes.Count(blob, []byte("\n"))
			if len(blob) > stateChunkBytes && entries != 1 {
				t.Errorf("chunk %s has %d bytes in %d entries, want at most %d bytes or one entry", key, len(blob), entries, stateChunkBytes)
			}
			single = single || entries == 1 && len(blob) > stateChunkBytes
		}
	}
	if chunks < 9 || !single {
		t.Errorf("chunks = %d, one-entry chunk over the bound = %v, want at least 9 chunks and an entry of its own", chunks, single)
	}
}

func rowCCStateResumesOnANewRuntime(t *testing.T) {
	a, b, c := ccEntries("a", 5, 3<<19), ccEntries("b", 1, 1<<10), ccEntries("c", 1, 1<<10)
	l := newCCLane(t, "", ccFixture(t, "S", a), ccFixture(t, "S", b), ccFixture(t, "S", c))
	r := l.runtime()
	s := l.create(r)
	l.send(s, "1")
	awaitTurns(t, s, 1)
	l.close(r)
	r = l.runtime()
	s = l.open(r)
	l.send(s, "2")
	awaitTurns(t, s, 2)
	l.close(r)
	r = l.runtime()
	s = l.open(r)
	l.send(s, "3")
	awaitTurns(t, s, 3)
	l.close(r)
	seen, file := l.seenFiles(), "projects/p/S.jsonl"
	if len(seen) != 3 || len(seen[0]) != 0 || seen[1][file] != ccTranscript(a) || seen[2][file] != ccTranscript(a, b) {
		t.Errorf("transcripts at start: %d spawns, want none, then the entries of the first turn, then those of two turns", len(seen))
	}
	if got := l.resumed(); len(got) != 3 || got[0] != "" || got[1] != "S" || got[2] != "S" {
		t.Errorf("--resume of the spawns = %q, want none, S, S", got)
	}
}

func rowCCStateContinuesAfterAHandoff(t *testing.T) {
	a, b, c := ccEntries("a", 5, 3<<19), ccEntries("b", 2, 3<<19), ccEntries("c", 1, 1<<10)
	l := newCCLane(t, "3", ccFixture(t, "S", a), ccFixture(t, "S", b), ccFixture(t, "S", c))
	r := l.runtime()
	s := l.create(r)
	l.send(s, "1")
	l.awaitText(s, ccWorking)
	l.close(r)
	r = l.runtime()
	s = l.open(r)
	awaitTurns(t, s, 1)
	l.send(s, "2")
	awaitTurns(t, s, 2)
	l.close(r)
	seen, file := l.seenFiles(), "projects/p/S.jsonl"
	if len(seen) != 3 || seen[1][file] != ccTranscript(a[:3]) || seen[2][file] != ccTranscript(a[:3], b) {
		t.Errorf("transcripts at start: %d spawns, want none, the three entries of the cut turn, then those of both owners", len(seen))
	}
	states, fences := l.states()
	owners := map[uint64]int{}
	for i, s := range states {
		if s.Chunk != "" {
			owners[fences[i]]++
		}
		if want := fmt.Sprintf("%s-%d-", ccSession, fences[i]); s.Chunk != "" && !strings.HasPrefix(s.Chunk, want) {
			t.Errorf("chunk %q of owner %d, want the prefix %q", s.Chunk, fences[i], want)
		}
		if s.Restart && fences[i] != fences[0] {
			t.Errorf("record %d of owner %d starts a new chain, want the chain of the first owner to continue", i, fences[i])
		}
	}
	if len(owners) != 2 {
		t.Errorf("owners that saved a chunk = %v, want 2 owners", owners)
	}
}

func rowCCStateNewSessionStartsANewChain(t *testing.T) {
	x, y, z := ccEntries("x", 2, 3<<19), ccEntries("y", 2, 1<<10), ccEntries("z", 1, 1<<10)
	l := newCCLane(t, "", ccFixture(t, "X", x), ccFixture(t, "Y", y), ccFixture(t, "Y", z))
	r := l.runtime()
	s := l.create(r)
	for i := 1; i <= 3; i++ {
		l.send(s, fmt.Sprint(i))
		awaitTurns(t, s, i)
	}
	l.close(r)
	seen := l.seenFiles()
	if len(seen) != 3 || seen[1]["projects/p/X.jsonl"] != ccTranscript(x) || len(seen[2]) != 1 || seen[2]["projects/p/Y.jsonl"] != ccTranscript(y) {
		t.Errorf("transcripts at start = %v, want none, the file of X, then the file of Y alone", seen)
	}
	if got := l.resumed(); len(got) != 3 || got[2] != "Y" {
		t.Errorf("--resume of the spawns = %q, want the third to resume Y", got)
	}
	restarts := 0
	states, _ := l.states()
	for _, s := range states {
		if s.Restart {
			restarts++
		}
	}
	if restarts != 2 {
		t.Errorf("records that start a chain = %d, want 2: the first session and the new one", restarts)
	}
}

func rowCCStateOneBlobStartsAChain(t *testing.T) {
	old, add := ccEntries("o", 3, 1<<10), ccEntries("n", 1, 1<<10)
	l := newCCLane(t, "", ccFixture(t, "S", add), ccFixture(t, "S", ccEntries("m", 1, 1<<10)))
	r := l.runtime()
	l.create(r)
	l.close(r)
	head, err := l.st.Head(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"v":1,"seq":%d,"t":"2026-10-06T00:00:00Z","k":"backend.state","d":{"backend":%q,"blob_key":"claude-code-1"}}`, head+1, ccSession)
	blob := `{"session_id":"S","path":"projects/p/S.jsonl"}` + "\n" + ccTranscript(old)
	if err := l.st.PutBlob(t.Context(), "s1", "claude-code-1", strings.NewReader(blob)); err != nil {
		t.Fatal(err)
	}
	if err := l.st.Append(t.Context(), "s1", head, []byte(record)); err != nil {
		t.Fatal(err)
	}
	r = l.runtime()
	s := l.open(r)
	l.send(s, "1")
	awaitTurns(t, s, 1)
	l.close(r)
	r = l.runtime()
	s = l.open(r)
	l.send(s, "2")
	awaitTurns(t, s, 2)
	l.close(r)
	seen, file := l.seenFiles(), "projects/p/S.jsonl"
	if len(seen) != 2 || seen[0][file] != ccTranscript(old) || seen[1][file] != ccTranscript(old, add) {
		t.Errorf("transcripts at start = %d spawns, want the old blob, then the old blob and the new entry", len(seen))
	}
	states, _ := l.states()
	if len(states) < 3 || states[0].BlobKey == "" {
		t.Fatalf("records = %+v, want the one-blob record and then records of chunks", states)
	}
	if first := states[1]; !first.Restart || first.Chunk == "" || string(l.blob(first.Chunk)) != ccTranscript(old, add) {
		t.Errorf("first save after the one-blob record = %+v, want a chain that starts with a chunk of every entry", first)
	}
	if next := states[2]; next.Restart || next.Chunk == "" {
		t.Errorf("second save = %+v, want it to continue the chain with a chunk", next)
	}
}

func rowCCStateKeepsEarlyMirrors(t *testing.T) {
	a, b := ccEntries("a", 3, 1<<10), ccEntries("b", 1, 1<<10)
	l := newCCLane(t, "", ccFixtureOrdered(t, "S", a, true), ccFixture(t, "S", b))
	r := l.runtime()
	s := l.create(r)
	l.send(s, "1")
	awaitTurns(t, s, 1)
	l.close(r)
	r = l.runtime()
	s = l.open(r)
	l.send(s, "2")
	awaitTurns(t, s, 2)
	l.close(r)
	seen := l.seenFiles()
	if len(seen) != 2 || seen[1]["projects/p/S.jsonl"] != ccTranscript(a) {
		t.Errorf("transcripts at start: %d spawns, want none, then the entries that came before the init frame", len(seen))
	}
}
