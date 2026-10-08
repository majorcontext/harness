package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

// awaitStartWork waits until serve has finished what a start owes: the line
// that follows the opens of the stored sessions.
func awaitStartWork(t *testing.T, d *runtimeDriver) {
	t.Helper()
	done := func() bool {
		return strings.Contains(d.Stderr(), "opened stored sessions")
	}
	if !testpoll.UntilNoT(waitBound, done) {
		t.Fatalf("serve did not finish its start work\n%s", d.Stderr())
	}
}

// awaitLog waits until the stderr of serve holds each of wants. Serve writes
// stderr through a pipe that a goroutine of the test copies, so a line has no
// order against the HTTP reply that followed its write.
func awaitLog(t *testing.T, d *runtimeDriver, wants ...string) {
	t.Helper()
	has := func() bool {
		log := d.Stderr()
		for _, want := range wants {
			if !strings.Contains(log, want) {
				return false
			}
		}
		return true
	}
	if !testpoll.UntilNoT(waitBound, has) {
		t.Fatalf("serve did not log %q\n%s", wants, d.Stderr())
	}
}

func TestContractServeStartOpensOnlySessionsWithWork(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.New(t, replyText("ok"))
	d := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil)), nil, t.TempDir())
	id := runTurn(t, d, "go")
	before := d.view(t, id).HeadSeq
	d.Restart(t, false)
	awaitStartWork(t, d)
	d.Restart(t, false)
	awaitStartWork(t, d)
	if after := d.view(t, id).HeadSeq; after != before {
		t.Errorf("an idle session holds %d records after two starts, want the %d from before: a start opened it", after, before)
	}
}

func TestContractServeStartSkipsALogThatDoesNotReplay(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.New(t, replyText("ok"))
	d := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil)), nil, t.TempDir())
	id := runTurn(t, d, "go")
	d.Restart(t, false)
	bad := filepath.Join(d.store, "ses_unreadable")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "log.jsonl"), []byte("not a record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.Restart(t, false)
	awaitStartWork(t, d)
	if status, _ := d.do(t, http.MethodGet, "/health", nil); status != http.StatusOK {
		t.Errorf("/health = %d after a start with a log that does not replay, want 200", status)
	}
	if got := d.view(t, id).Status; got != "idle" {
		t.Errorf("the readable session is %q, want idle", got)
	}
	awaitLog(t, d, "ses_unreadable", "does not replay")
}

func TestContractListSkipsALogThatDoesNotReplay(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.New(t, replyText("ok"))
	d := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil)), nil, t.TempDir())
	id := runTurn(t, d, "go")
	first, err := os.ReadFile(filepath.Join(d.store, id, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(first), "\n")
	bad := filepath.Join(d.store, "ses_torn")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "log.jsonl"), []byte(line+"\nnot a record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	d.expect(t, http.StatusOK, http.MethodGet, "/sessions", nil, &page)
	if len(page.Sessions) != 1 || page.Sessions[0].ID != id {
		t.Errorf("GET /sessions lists %+v, want only %s: a log that does not replay is skipped", page.Sessions, id)
	}
	awaitLog(t, d, "ses_torn")
}

func TestContractServeStartCatchesUpEveryStoredSession(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.New(t, replyText("ok"))
	first := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil)), nil, t.TempDir())
	a, b := runTurn(t, first, "one"), runTurn(t, first, "two")
	headA, headB := first.view(t, a).HeadSeq, first.view(t, b).HeadSeq
	first.proc.terminate(t)

	receiver := newSyncReceiver(t, nil)
	cfg := serveSyncConfig(t, fake, receiver.srv.URL)
	second := &runtimeDriver{store: first.store, workDir: first.workDir, serve: true, configPath: cfg, client: first.client,
		lastInput: map[string]string{}, lastTyped: map[string]string{}}
	second.start(t)
	caughtUp := func() bool {
		for id, head := range map[string]uint64{a: headA, b: headB} {
			if got, _ := receiver.store.Head(t.Context(), id); got != head {
				return false
			}
		}
		return true
	}
	if !testpoll.UntilNoT(waitBound, caughtUp) {
		t.Fatalf("the receiver did not get every record of both sessions\n%s", second.Stderr())
	}

	local := harness.NewDiskStore(first.store)
	for _, id := range []string{a, b} {
		equalLogs(t, local, receiver.store, id)
	}
	for _, c := range receiver.snapshot() {
		if c.auth != "Bearer "+syncToken || c.batch.Epoch != 7 {
			t.Errorf("a batch came with %q under epoch %d, want the bearer token of the file under epoch 7", c.auth, c.batch.Epoch)
		}
	}
	if got := second.view(t, a).HeadSeq; got != headA {
		t.Errorf("session a holds %d records after the catch-up, want the %d from before: the catch-up opened it", got, headA)
	}
	if got := second.view(t, b).HeadSeq; got != headB {
		t.Errorf("session b holds %d records after the catch-up, want the %d from before", got, headB)
	}
}

func TestContractServeLogsItsStartAndEachCreate(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.New(t, replyText("ok"))
	d := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil)), nil, t.TempDir())
	id := d.Create(t)
	awaitLog(t, d, `"msg":"serve start"`, `"msg":"config: `, `"msg":"session created"`, `"session":"`+id+`"`)
}
