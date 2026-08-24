package process

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/testpoll"
)

// TestStartConcurrentSpawnsExactlyOnce encodes the PR#71 review finding:
// Start's active-check and spawn were separated by an unlock window, so two
// concurrent Start calls for the same name (session tool racing HTTP POST)
// could both spawn, with the second overwriting the first in m.procs —
// leaking an untracked process group that Stop could never reach.
func TestStartConcurrentSpawnsExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	m := NewManager(dir, map[string]Def{
		"dev": {
			// Each real spawn appends one marker line, then blocks.
			Command: []string{"sh", "-c", "echo spawned >> " + marker + "; exec sleep 300"},
		},
	})
	t.Cleanup(func() { m.Close(context.Background()) })

	const racers = 8
	var wg sync.WaitGroup
	gate := make(chan struct{})
	pids := make([]int, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			st, err := m.Start(context.Background(), "dev")
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			pids[i] = st.PID
		}(i)
	}
	close(gate)
	wg.Wait()

	// Every racer must have observed the SAME process.
	for i := 1; i < racers; i++ {
		if pids[i] != pids[0] {
			t.Errorf("racer %d saw pid %d, racer 0 saw %d — more than one process spawned", i, pids[i], pids[0])
		}
	}
	// And exactly one real OS process ran: one marker line. The marker is
	// written by a real child shell, so this waits through testpoll, the
	// shared cross-process poll helper (see that package's doc comment).
	markerCount := func() int {
		b, _ := os.ReadFile(marker)
		return len(strings.Fields(string(b)))
	}
	testpoll.Until(t, 5*time.Second, "no process spawned at all (the marker file stayed empty)", func() bool {
		return markerCount() >= 1
	})
	// A second racer that wrongly spawned would append its own marker line
	// shortly after the winner's. Give that straggler a bounded window to
	// appear, then assert the count is still exactly one. testpoll.UntilNoT
	// reports false when the condition never held, which is the PASSING
	// outcome here — this waits for a violation, not for success.
	if testpoll.UntilNoT(time.Second, func() bool { return markerCount() > 1 }) {
		t.Fatalf("%d processes spawned, want exactly 1", markerCount())
	}
}
