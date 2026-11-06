package e2e

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

type diskCheckpoint struct{ first, child string }

type diskCheckpoints interface {
	LoadCheckpoint(context.Context, string) (uint64, []byte, error)
	SaveCheckpoint(context.Context, string, uint64, []byte) error
}

func TestContractDiskCheckpoint(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "disk_checkpoint_stopped_reads_reuse_and_repair",
		model:   delegation("general-purpose", childDone),
		actions: append(append([]action{}, spawned...), restart{}, diskCheckpoint{first: "a", child: "kid"}),
	}})
}

func diskCheckpointBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "source")
	runCheckpointCommand(t, root, os.Environ(), "git", "clone", "--quiet", "--no-hardlinks", root, dir)
	files := exec.CommandContext(t.Context(), "git", "ls-files", "-co", "--exclude-standard", "-z")
	files.Dir = root
	out, err := files.Output()
	if err != nil {
		t.Fatal(err)
	}
	untracked := exec.CommandContext(t.Context(), "git", "ls-files", "--others", "--exclude-standard", "-z")
	untracked.Dir = root
	extra, err := untracked.Output()
	if err != nil {
		t.Fatal(err)
	}
	untrackedFiles := make(map[string]bool)
	for _, name := range strings.Split(string(extra), "\x00") {
		untrackedFiles[name] = true
	}
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" || strings.HasPrefix(name, ".harness/") || untrackedFiles[name] && !strings.HasSuffix(name, ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if os.IsNotExist(err) {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		writeCheckpointFile(t, filepath.Join(dir, name), data)
	}
	path := filepath.Join(dir, "cmd", "harness", "storelog.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(source), "\"io\"", "\"io\"\n\"os\"", 1)
	needle := "func (s *observedStore) Read(ctx context.Context, session string, afterSeq uint64, limit int) ([]harness.Record, error) {"
	instrument := needle + `
 if afterSeq == 0 && limit > 1 {
  checkpointTraceMu.Lock()
  if f, err := os.OpenFile(os.Getenv("HARNESS_CHECKPOINT_TRACE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil { _, _ = f.WriteString(session + "\n"); _ = f.Close() }
  checkpointTraceMu.Unlock()
 }`
	if !strings.Contains(text, needle) {
		t.Fatal("observedStore.Read not found for checkpoint trace")
	}
	text = strings.Replace(text, needle, instrument, 1) + "\nvar checkpointTraceMu sync.Mutex\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	runCheckpointCommand(t, dir, os.Environ(), "git", "add", "-A")
	runCheckpointCommand(t, dir, os.Environ(), "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "checkpoint fixture")
	bin := filepath.Join(t.TempDir(), "harness")
	runCheckpointCommand(t, dir, os.Environ(), "go", "build", "-buildvcs=true", "-o", bin, "./cmd/harness")
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var revision, modified string
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			revision = s.Value
		}
		if s.Key == "vcs.modified" {
			modified = s.Value
		}
	}
	if revision == "" || modified != "false" {
		t.Fatalf("serve build is not versioned: revision=%q modified=%q", revision, modified)
	}
	return bin
}

func startCheckpointServe(t *testing.T, bin, dir, trace string) *serveProc {
	t.Helper()
	addr, token := freeAddr(t), newRunToken(t)
	cmd := exec.CommandContext(t.Context(), bin, "serve", "-addr", addr)
	cmd.Dir = t.TempDir()
	cmd.Env = cleanEnv(withToken(map[string]string{"HARNESS_SESSION_DIR": dir, "HARNESS_CONFIG": writeGoalConfig(t, "http://127.0.0.1:1"), "ANTHROPIC_API_KEY": "e2e-dummy-key", "HARNESS_CHECKPOINT_TRACE": trace}, token))
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	p := &serveProc{procGroup: startGroup(t, cmd), t: t, addr: addr, token: token, stderr: stderr}
	if !p.waitOwned() {
		t.Fatal("checkpoint serve did not bind its port")
	}
	return p
}

func checkpointRead(t *testing.T, p *serveProc, id string) []byte {
	t.Helper()
	resp := serveRequest(t, p, http.MethodGet, "/sessions/"+id+"/messages", "Bearer "+p.token, "")
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("read stopped session: status=%d err=%v body=%s stderr=%s", resp.StatusCode, err, body, p.stderr.String())
	}
	return body
}

func (a diskCheckpoint) run(t *testing.T, r *run) {
	t.Helper()
	bin := diskCheckpointBinary(t)
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(r.drv.storeDir())); err != nil {
		t.Fatal(err)
	}
	child := r.id(t, a.child)
	log, err := os.ReadFile(filepath.Join(dir, child, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range checkpointChildIDs() {
		writeCheckpointFile(t, filepath.Join(dir, id, "log.jsonl"), bytes.ReplaceAll(log, []byte(child), []byte(id)))
	}
	trace := filepath.Join(t.TempDir(), "trace")
	p := startCheckpointServe(t, bin, dir, trace)
	first := r.id(t, a.first)
	for _, session := range append([]string{first, child}, checkpointChildIDs()...) {
		if err := os.Remove(filepath.Join(dir, session, "checkpoint")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	list := checkpointList(t, p)
	if len(list) != 201 {
		t.Fatalf("list returned %d sessions, want 201", len(list))
	}
	st, ok := any(store.NewDiskStore(dir)).(diskCheckpoints)
	if !ok {
		t.Fatal("versioned serve disk store did not persist checkpoints: disk store does not support checkpoints")
	}
	wantFirst := checkpointRead(t, p, first)
	checkDiskCheckpoint(t, p, st, dir, first, wantFirst, list)
	if err := os.WriteFile(trace, nil, 0600); err != nil {
		t.Fatal(err)
	}
	again := checkpointList(t, p)
	if !reflect.DeepEqual(list, again) {
		t.Fatal("checkpoint list changed session results")
	}
	replayed, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 0 {
		t.Fatalf("versioned serve list replayed checkpoints from seq 1: %s", replayed)
	}
}

func checkDiskCheckpoint(t *testing.T, p *serveProc, st diskCheckpoints, dir, first string, want []byte, list []protocol.Session) {
	t.Helper()
	ctx := t.Context()
	seq, data, err := st.LoadCheckpoint(ctx, first)
	if err != nil || seq == 0 || len(data) == 0 {
		t.Fatalf("versioned serve did not save checkpoint: seq=%d bytes=%d err=%v", seq, len(data), err)
	}
	if got := checkpointRead(t, p, first); !bytes.Equal(got, want) {
		t.Fatal("checkpoint read changed session messages")
	}
	for _, bad := range [][]byte{[]byte("corrupt checkpoint")} {
		if err := st.SaveCheckpoint(ctx, first, seq, bad); err != nil {
			t.Fatal(err)
		}
		if got := checkpointList(t, p); !reflect.DeepEqual(got, list) {
			t.Fatal("corrupt checkpoint changed the session list")
		}
		fresh := any(store.NewDiskStore(dir)).(diskCheckpoints)
		repairedSeq, repaired, err := fresh.LoadCheckpoint(ctx, first)
		if err != nil || repairedSeq != seq || len(repaired) == 0 || bytes.Equal(repaired, bad) {
			t.Fatalf("checkpoint not repaired across store instances: seq=%d err=%v", repairedSeq, err)
		}
	}
	checkpointAheadOfLog(t, dir, first, seq, data)
	if got := checkpointList(t, p); !reflect.DeepEqual(got, list) {
		t.Fatal("high-sequence checkpoint changed the session list")
	}
	fresh := any(store.NewDiskStore(dir)).(diskCheckpoints)
	if got, data, err := fresh.LoadCheckpoint(ctx, first); err != nil || got != seq || len(data) == 0 {
		t.Fatalf("high-sequence checkpoint prevented repair: seq=%d err=%v", got, err)
	}
}

func checkpointChildIDs() []string {
	ids := make([]string, 199)
	for i := range ids {
		ids[i] = fmt.Sprintf("ses_%026d", i)
	}
	return ids
}

func checkpointAheadOfLog(t *testing.T, dir, session string, seq uint64, data []byte) {
	t.Helper()
	copyDir := t.TempDir()
	if err := os.CopyFS(copyDir, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(copyDir, session, "log.jsonl")
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line, _, found := bytes.Cut(log, []byte("\n"))
	if !found {
		t.Fatal("session log has no complete record")
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	copyStore := any(store.NewDiskStore(copyDir)).(diskCheckpoints)
	if err := copyStore.SaveCheckpoint(t.Context(), session, seq+1, data); err != nil {
		t.Fatalf("create ahead checkpoint in copied store: %v", err)
	}
	path := filepath.Join(copyDir, session, "checkpoint")
	checkpoint, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, session, "checkpoint"), checkpoint, 0600); err != nil {
		t.Fatal(err)
	}
}

func checkpointList(t *testing.T, p *serveProc) []protocol.Session {
	t.Helper()
	var all []protocol.Session
	after := ""
	for {
		path := "/sessions?limit=100&after=" + url.QueryEscape(after)
		resp := serveRequest(t, p, http.MethodGet, path, "Bearer "+p.token, "")
		var page protocol.SessionPage
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("list stopped sessions: status=%d err=%v", resp.StatusCode, err)
		}
		all = append(all, page.Sessions...)
		if page.Next == "" {
			return all
		}
		if page.Next == after {
			t.Fatal("list cursor did not advance")
		}
		after = page.Next
	}
}
