package hub

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// collectSpawn runs runSpawn to completion and returns every event emitted,
// in order.
func collectSpawn(t *testing.T, ctx context.Context, command string) []spawnEvent {
	t.Helper()
	var mu sync.Mutex
	var got []spawnEvent
	runSpawn(ctx, command, func(ev spawnEvent) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	})
	return got
}

func TestRunSpawnNoCommandConfigured(t *testing.T) {
	events := collectSpawn(t, context.Background(), "")
	if len(events) != 1 {
		t.Fatalf("events = %#v, want exactly one", events)
	}
	if events[0].Type != "done" || events[0].Error == "" {
		t.Fatalf("event = %#v, want a done event carrying an error", events[0])
	}
}

func TestRunSpawnParsesContractLines(t *testing.T) {
	// Feed synthetic spawn output through a real `sh -c` invocation — the
	// subprocess machinery (exec.CommandContext, combined-stream scanning)
	// is what's under test, so a real process is the right fixture here
	// (see AGENTS.md's e2e exception).
	script := `echo "booting sandbox..."
echo "TUNNEL_URL=https://box-42.example.dev"
echo "some other progress line"
echo "RUN_TOKEN=sekrit-token-value"
echo "ready"
`
	events := collectSpawn(t, context.Background(), script)
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	last := events[len(events)-1]
	if last.Type != "done" {
		t.Fatalf("last event type = %q, want done", last.Type)
	}
	if last.Error != "" {
		t.Fatalf("done event carried error: %q", last.Error)
	}
	if last.TunnelURL != "https://box-42.example.dev" {
		t.Errorf("TunnelURL = %q, want https://box-42.example.dev", last.TunnelURL)
	}
	if last.RunToken != "sekrit-token-value" {
		t.Errorf("RunToken = %q, want sekrit-token-value", last.RunToken)
	}
	if last.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", last.ExitCode)
	}

	var lines []string
	for _, ev := range events {
		if ev.Type == "stdout" {
			lines = append(lines, ev.Line)
		}
	}
	want := []string{"booting sandbox...", "TUNNEL_URL=https://box-42.example.dev", "some other progress line", "RUN_TOKEN=sekrit-token-value", "ready"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("stdout lines = %#v, want %#v", lines, want)
	}
}

// TestRunSpawnContractLinesToleratesWhitespace verifies the TUNNEL_URL=/
// RUN_TOKEN= lines are matched after trimming surrounding whitespace, and
// that a value is trimmed too (a spawn script commonly pads with a
// trailing carriage return or spaces from an underlying tool's echo).
func TestRunSpawnContractLinesToleratesWhitespace(t *testing.T) {
	script := `printf '  TUNNEL_URL=https://x.example  \n'
printf 'RUN_TOKEN=abc123\n'
`
	events := collectSpawn(t, context.Background(), script)
	last := events[len(events)-1]
	if last.TunnelURL != "https://x.example" {
		t.Errorf("TunnelURL = %q, want https://x.example", last.TunnelURL)
	}
	if last.RunToken != "abc123" {
		t.Errorf("RunToken = %q, want abc123", last.RunToken)
	}
}

func TestRunSpawnNonZeroExit(t *testing.T) {
	events := collectSpawn(t, context.Background(), "echo one; exit 7")
	last := events[len(events)-1]
	if last.Type != "done" {
		t.Fatalf("last event type = %q, want done", last.Type)
	}
	if last.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", last.ExitCode)
	}
	if last.Error == "" {
		t.Error("Error is empty, want a non-zero-exit error message")
	}
}

func TestRunSpawnMissingContractLinesLeaveFieldsEmpty(t *testing.T) {
	events := collectSpawn(t, context.Background(), "echo hello")
	last := events[len(events)-1]
	if last.TunnelURL != "" || last.RunToken != "" {
		t.Errorf("done = %#v, want empty TunnelURL/RunToken", last)
	}
}

// TestRunSpawnCancelKillsProcess proves context cancellation actually kills
// a hung spawn process rather than runSpawn blocking forever, with no raw
// time.Sleep anywhere: the fixture prints one line and then blocks reading
// stdin (which the test never provides), and the test waits on a channel
// for that exact "stdout" event — the emitted event itself is the
// synchronization signal that the process has started and is now blocked —
// before canceling. runSpawn finishing (the done channel closing) is what
// proves the kill worked; there is nothing to sleep for.
func TestRunSpawnCancelKillsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ready := make(chan struct{})
	done := make(chan []spawnEvent, 1)
	go func() {
		var mu sync.Mutex
		var events []spawnEvent
		var readyOnce sync.Once
		runSpawn(ctx, "echo ready; cat", func(ev spawnEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			if ev.Type == "stdout" && ev.Line == "ready" {
				readyOnce.Do(func() { close(ready) })
			}
		})
		mu.Lock()
		done <- events
		mu.Unlock()
	}()

	<-ready
	cancel()
	events := <-done
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	last := events[len(events)-1]
	if last.Type != "done" {
		t.Fatalf("last event type = %q, want done", last.Type)
	}
}
