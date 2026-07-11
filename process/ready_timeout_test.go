package process

import (
	"strings"
	"testing"
	"time"
)

// TestNoteReadyTimeoutPreservesReady encodes the PR#71 review finding:
// select picks randomly among simultaneously-ready cases, so the timer
// branch can fire even though markReady just ran. A process that already
// reached ready (or exited, or was stopped) must be left alone; only
// starting -> running is a legal timeout transition.
func TestNoteReadyTimeoutPreservesReady(t *testing.T) {
	cases := []struct {
		state     State
		wantState State
		wantNote  bool
	}{
		{StateStarting, StateRunning, true},
		{StateReady, StateReady, false},
		{StateExited, StateExited, false},
		{StateStopped, StateStopped, false},
	}
	for _, c := range cases {
		p := &managedProcess{state: c.state, ready: c.state == StateReady}
		p.noteReadyTimeout(30 * time.Second)
		if p.state != c.wantState {
			t.Errorf("from %s: state = %s, want %s", c.state, p.state, c.wantState)
		}
		if got := strings.Contains(p.note, "timed out"); got != c.wantNote {
			t.Errorf("from %s: note %q, wantNote=%v", c.state, p.note, c.wantNote)
		}
	}
}
