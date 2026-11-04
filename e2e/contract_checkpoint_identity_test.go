package e2e

import (
	"bytes"
	"os"
	"runtime/debug"
	"testing"

	"github.com/majorcontext/harness"
)

func testCleanVCS() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	return revision != "" && modified == "false"
}

func checkCheckpointMode(t *testing.T) {
	t.Helper()
	mode := os.Getenv("HARNESS_E2E_CHECKPOINT")
	switch mode {
	case "":
	case "restore":
		if !testCleanVCS() {
			t.Fatal("checkpoint restore expected a clean VCS build")
		}
	case "bypass":
		if testCleanVCS() {
			t.Fatal("checkpoint bypass expected a dirty or unstamped build")
		}
	default:
		t.Fatalf("invalid HARNESS_E2E_CHECKPOINT mode %q", mode)
	}
}

func checkCheckpointBypass(t *testing.T, base harness.Store, id string) {
	t.Helper()
	if testCleanVCS() {
		return
	}
	tracking := &replayTrackingStore{Store: base}
	st := &checkpointStore{Store: tracking, seq: 1, data: []byte("invalid")}
	if _, err := harness.OpenView(t.Context(), st, id); err != nil {
		t.Fatal(err)
	}
	if st.loads != 0 || st.saves != 0 || !tracking.fromStart {
		t.Fatalf("uncacheable build: loads=%d saves=%d replay from start=%v", st.loads, st.saves, tracking.fromStart)
	}
}

func checkCheckpointRepair(t *testing.T, st *checkpointStore, id string, seq uint64, invalid []byte) {
	t.Helper()
	saved, data, err := st.LoadCheckpoint(t.Context(), id)
	if err != nil || saved != seq || len(data) == 0 || bytes.Equal(data, invalid) {
		t.Fatalf("checkpoint was not repaired: seq=%d bytes=%d err=%v", saved, len(data), err)
	}
	tracking := &replayTrackingStore{Store: st.Store}
	repaired := &checkpointStore{Store: tracking, seq: saved, data: data}
	if _, err := harness.OpenView(t.Context(), repaired, id); err != nil {
		t.Fatal(err)
	}
	if tracking.fromStart {
		t.Fatal("repaired checkpoint replayed from start")
	}
}

func checkCheckpointBuildIdentity(t *testing.T, base harness.Store, id string, seq uint64, data []byte) {
	t.Helper()
	hasVCS := testCleanVCS()
	tracking := &replayTrackingStore{Store: base}
	st := &checkpointStore{Store: tracking, seq: seq, data: data}
	if _, err := harness.OpenView(t.Context(), st, id); err != nil {
		t.Fatal(err)
	}
	if tracking.fromStart == hasVCS {
		t.Fatalf("checkpoint replay from start = %v, VCS identity = %v", tracking.fromStart, hasVCS)
	}
	tracking = &replayTrackingStore{Store: base}
	st = &checkpointStore{Store: tracking, seq: seq + 1, data: data}
	if _, err := harness.OpenView(t.Context(), st, id); err != nil {
		t.Fatal(err)
	}
	if !tracking.fromStart {
		t.Fatal("out-of-range checkpoint did not replay")
	}
}
