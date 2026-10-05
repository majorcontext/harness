package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
	"github.com/majorcontext/harness/protocol"
)

const stopReportFile = "serve-stop.json"

type stopReport struct {
	Stop string `json:"stop"`
	Sync string `json:"sync"`
}

func readStopReport(t *testing.T, store string) stopReport {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store, stopReportFile))
	if err != nil {
		t.Fatalf("serve left no stop report: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' || len(data) != len(trimNewline(data))+1 {
		t.Errorf("stop report %q is not one line", data)
	}
	var r stopReport
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("stop report %q: %v", data, err)
	}
	return r
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

func serveSyncConfig(t *testing.T, fake *harnesstest.Server, receiverURL string) string {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "sync-token")
	if err := os.WriteFile(tokenFile, []byte(syncToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return writeGoalConfigWith(t, fake.URL(), scenarioConfig(map[string]any{"owner_epoch": 7,
		"sync": map[string]any{"url": receiverURL + syncPath, "token_file": tokenFile}}))
}

func TestContractServeStopReportsHandoffAndSynced(t *testing.T) {
	skipShort(t)
	fake := harnesstest.New(t, replyText("ok"))
	receiver := newSyncReceiver(t, nil)
	d := newServeDriverIn(t, serveSyncConfig(t, fake, receiver.srv.URL), nil, t.TempDir())
	runTurn(t, d, "go")
	d.proc.terminate(t)
	if got := readStopReport(t, d.store); got != (stopReport{Stop: "handoff", Sync: "synced"}) {
		t.Errorf("stop report = %+v, want handoff and synced: every session released and Sync holds every record", got)
	}
}

func TestContractServeStopWithoutSyncReportsUnsynced(t *testing.T) {
	skipShort(t)
	fake := harnesstest.New(t, replyText("ok"))
	d := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil)), nil, t.TempDir())
	runTurn(t, d, "go")
	d.proc.terminate(t)
	if got := readStopReport(t, d.store); got != (stopReport{Stop: "handoff", Sync: "unsynced"}) {
		t.Errorf("stop report = %+v, want handoff and unsynced: no Sync holds the records", got)
	}
}

func TestContractServeStopReportsCrashedWhenSyncDoesNotAcknowledge(t *testing.T) {
	skipShort(t)
	fake := harnesstest.New(t, replyText("ok"), replyText("again"))
	var down atomic.Bool
	receiver := newSyncReceiver(t, func(int, protocol.SyncBatch) (int, string) {
		if down.Load() {
			return http.StatusServiceUnavailable, "unavailable"
		}
		return 0, ""
	})
	d := newServeDriverIn(t, serveSyncConfig(t, fake, receiver.srv.URL), nil, t.TempDir())
	id := runTurn(t, d, "go")
	down.Store(true)
	d.Submit(t, id, "more")
	d.WaitIdle(t, id)
	start := time.Now()
	d.proc.terminate(t)
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("serve took %s to stop, want the close deadline of 5 s", took)
	}
	if got := readStopReport(t, d.store); got != (stopReport{Stop: "crashed", Sync: "unsynced"}) {
		t.Errorf("stop report = %+v, want crashed and unsynced: Sync never acknowledged the last records", got)
	}
}

func TestContractServeStopReportsUnsyncedAfterAFinalRejectionDuringTheRun(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name   string
		status int
		code   string
	}{
		{"sync_conflict", http.StatusConflict, "sync_conflict"},
		{"unauthorized", http.StatusUnauthorized, "unauthorized"},
		{"too_large", http.StatusRequestEntityTooLarge, "too_large"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			fake := harnesstest.New(t, replyText("ok"), replyText("again"))
			var refuse atomic.Bool
			receiver := newSyncReceiver(t, func(int, protocol.SyncBatch) (int, string) {
				if refuse.Load() {
					return row.status, row.code
				}
				return 0, ""
			})
			d := newServeDriverIn(t, serveSyncConfig(t, fake, receiver.srv.URL), nil, t.TempDir())
			id := runTurn(t, d, "go")
			head := d.view(t, id).HeadSeq
			synced := func() bool {
				got, _ := receiver.store.Head(t.Context(), id)
				return got == head
			}
			if !testpoll.UntilNoT(waitBound, synced) {
				t.Fatalf("the receiver did not get the first %d records\n%s", head, d.Stderr())
			}
			refuse.Store(true)
			d.Submit(t, id, "more")
			awaitLog(t, d, "sync stopped for a session")
			d.proc.terminate(t)
			if got := readStopReport(t, d.store); got != (stopReport{Stop: "handoff", Sync: "unsynced"}) {
				t.Errorf("stop report = %+v, want handoff and unsynced: Sync rejected a batch for good, so it lacks records", got)
			}
		})
	}
}

func TestContractServeStartRemovesTheStopReportOfAnEarlierRun(t *testing.T) {
	skipShort(t)
	fake := harnesstest.New(t, replyText("ok"))
	store := t.TempDir()
	stale := filepath.Join(store, stopReportFile)
	if err := os.WriteFile(stale, []byte(`{"stop":"handoff","sync":"synced"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &runtimeDriver{store: store, workDir: t.TempDir(), serve: true, configPath: writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil)),
		client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}, lastInput: map[string]string{}, lastTyped: map[string]string{}}
	d.start(t)
	awaitStartWork(t, d)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stop report of an earlier run is still there after a start (stat err %v): a SIGKILL would leave it as the report of this run", err)
	}
}
