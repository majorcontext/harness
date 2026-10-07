package plugin

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"testing/synctest"
)

// testInstance finds the named instance inside a Host, for tests that need
// to reach into unexported state (e.g. instance.liveConn) that no public
// API exposes.
func testInstance(h *Host, name string) *instance {
	for _, inst := range h.instances {
		if inst.spec.Manifest.Name == name {
			return inst
		}
	}
	return nil
}

// TestPluginsDoesNotBlockOnWedgedSpawn verifies that a Host.Plugins read does
// not block behind another plugin's
// in-progress spawn. start holds inst.mu for the whole, possibly
// uncancellable dial-plus-handshake (see instance.start's doc comment), and
// Host is a box-scoped singleton shared by every session on the box, so a
// read that depended on inst.mu would let one wedged plugin stall
// GET /session and the session_info tool for every OTHER session too —
// exactly the coupling AGENTS.md's "a hung plugin can't wedge a session"
// rule forbids.
//
// Red-verify: before the fix, instance.info took inst.mu to read state.
// Wedge dial() on a channel closed only in t.Cleanup, so the spawning
// goroutine holds inst.mu for the rest of the test. The main goroutine then
// calls h.Plugins(), which — pre-fix — blocks on that same mu. Both
// goroutines end up durably blocked with no timer pending (release fires
// only in Cleanup, which runs after the test function returns), which
// synctest.Test reports as a bubble deadlock: a deterministic, zero-
// wall-clock red signal, the same pattern TestNotifyBacklogNeverBlocksReadLoop
// (protocol_test.go) and TestHookTimeoutFailsOpen (plugin_test.go) use.
func TestPluginsDoesNotBlockOnWedgedSpawn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spawning := make(chan struct{})
		release := make(chan struct{})
		spec := Spec{
			Manifest: Manifest{Name: "wedged", ProtocolVersion: ProtocolVersion, Hooks: []Hook{HookShellEnv}},
			dial: func() (io.ReadWriteCloser, error) {
				close(spawning)
				<-release // held for the life of the test: simulates a wedged spawn
				return nil, fmt.Errorf("dial never actually completes in this test")
			},
		}
		h := newTestHost(t, Options{}, spec)
		// release must fire (unblocking the spawning goroutine, and so
		// releasing inst.mu) before h.Close's own Cleanup runs, or Close
		// would itself block behind the same wedged start. t.Cleanup runs
		// LIFO, so registering this after newTestHost is what guarantees
		// the ordering.
		t.Cleanup(func() { close(release) })

		// Dispatch on its own goroutine: it spawns "wedged" and blocks for
		// the life of the test inside dial(), holding inst.mu throughout
		// (instance.start's documented behavior).
		go h.ShellEnv(context.Background(), &ShellEnvRequest{SessionID: "s1", Tool: "bash", Command: "ls"}) //nolint:errcheck
		<-spawning                                                                                          // wait until the spawn goroutine is inside dial(), holding inst.mu

		// The read must return promptly and report a non-running state,
		// even though inst.mu stays held for the rest of the test.
		infos := h.Plugins()
		if len(infos) != 1 {
			t.Fatalf("Plugins() = %+v, want exactly one", infos)
		}
		if got := infos[0].State; got != PluginNotSpawned {
			t.Errorf("state while spawn is wedged = %q, want %q", got, PluginNotSpawned)
		}
	})
}

// TestPluginsReportsErroredAfterPostSpawnDeath verifies that a plugin
// that dies after a successful spawn must not report "running" forever.
// liveState folds conn.closed — already closed both by an explicit stop and
// by the read loop's own error path (conn.fail, called from conn.run when
// the stream ends, e.g. the peer process died) — into the reported state,
// so no new death-detection goroutine is needed.
//
// Red-verify: before the fix, inst.err (the only input to the reported
// state besides started/stopped) is written once, in start, and never
// touched again. Killing the plugin after a successful spawn leaves
// started=true, stopped=false, err=nil untouched, so the state stayed
// "running" forever.
func TestPluginsReportsErroredAfterPostSpawnDeath(t *testing.T) {
	hostSide, pluginSide := net.Pipe()
	spec := Spec{
		Manifest: Manifest{Name: "flaky", ProtocolVersion: ProtocolVersion, Hooks: []Hook{HookShellEnv}},
		dial: func() (io.ReadWriteCloser, error) {
			go serve(pluginSide, Manifest{Name: "flaky"}, &Hooks{ //nolint:errcheck
				ShellEnv: func(_ context.Context, _ *Client, _ *ShellEnvRequest) (*ShellEnvResponse, error) {
					return &ShellEnvResponse{}, nil
				},
			})
			return hostSide, nil
		},
	}
	h := newTestHost(t, Options{}, spec)

	h.ShellEnv(context.Background(), &ShellEnvRequest{SessionID: "s1", Tool: "bash", Command: "ls"})
	if got := h.Plugins()[0].State; got != PluginRunning {
		t.Fatalf("state after spawn = %q, want %q", got, PluginRunning)
	}

	inst := testInstance(h, "flaky")
	c := inst.liveConn.Load()
	if c == nil {
		t.Fatal("liveConn not published after a successful spawn")
	}

	// Simulate the plugin process dying: close its side of the pipe. The
	// host's read loop (conn.run) then gets a read error and calls
	// conn.fail, closing conn.closed.
	if err := pluginSide.Close(); err != nil {
		t.Fatal(err)
	}
	<-c.closed // block on the actual death signal, not a sleep or a poll loop

	if got := h.Plugins()[0].State; got != PluginErrored {
		t.Errorf("state after the plugin process died = %q, want %q", got, PluginErrored)
	}
}

// TestNeverSpawnedStaysNotSpawnedAfterClose verifies that a configured
// plugin that no turn ever dispatched to must still report "not-spawned"
// after Host.Close, not "stopped". Close sets stopped=true on every
// instance unconditionally (it has to, to prevent any later respawn), but a
// plugin's own spawn state must outrank that box-wide flag: a plugin that
// never ran must never be reported as having run and then stopped.
//
// Red-verify: before the fix, stateLocked checked inst.stopped before
// !inst.started, so Close alone flipped every never-spawned plugin's
// reported state to "stopped".
func TestNeverSpawnedStaysNotSpawnedAfterClose(t *testing.T) {
	spec := Spec{
		Manifest: Manifest{Name: "idle", ProtocolVersion: ProtocolVersion, Hooks: []Hook{HookShellEnv}},
		dial: func() (io.ReadWriteCloser, error) {
			t.Fatal("idle plugin must never spawn in this test")
			return nil, nil
		},
	}
	h, err := NewHost(Options{}, spec)
	if err != nil {
		t.Fatal(err)
	}

	h.Close()

	if got := h.Plugins()[0].State; got != PluginNotSpawned {
		t.Errorf("state after Close with no dispatch = %q, want %q", got, PluginNotSpawned)
	}
}

// TestErroredStaysErroredAfterClose verifies that a
// plugin whose spawn itself failed must keep reporting "errored" after
// Host.Close, not be relabeled "stopped". stop's stopped=true guard has to
// apply unconditionally (so a later start attempt is refused — see
// errInstanceStopped), but the REPORTED state is a separate concern: a
// plugin Close only ever shut down cleanly is "stopped"; a plugin that
// never came up in the first place stays "errored" — Close didn't stop
// anything, there was nothing running to stop. Overwriting errored with
// stopped would erase the failure state.
//
// Red-verify: before the fix, stop's guard only special-cased
// stateNotSpawned (`!= stateNotSpawned` stores stateStopped for every other
// value, including stateErrored), so Close relabeled an errored plugin
// stopped.
func TestErroredStaysErroredAfterClose(t *testing.T) {
	spec := Spec{
		Manifest: Manifest{Name: "broken", ProtocolVersion: ProtocolVersion, Hooks: []Hook{HookShellEnv}},
		dial: func() (io.ReadWriteCloser, error) {
			return nil, fmt.Errorf("simulated spawn failure")
		},
	}
	h := newTestHost(t, Options{}, spec)

	h.ShellEnv(context.Background(), &ShellEnvRequest{SessionID: "s1", Tool: "bash", Command: "ls"})
	if got := h.Plugins()[0].State; got != PluginErrored {
		t.Fatalf("state after failed spawn = %q, want %q", got, PluginErrored)
	}

	h.Close()

	if got := h.Plugins()[0].State; got != PluginErrored {
		t.Errorf("state after Close following a failed spawn = %q, want %q", got, PluginErrored)
	}
}

// TestErroredAfterPostSpawnDeathStaysErroredAfterClose verifies that a plugin
// that spawned successfully and then fails must
// keep reporting "errored" after a later Host.Close, not "stopped".
//
// liveState's running-case computes a post-spawn death LAZILY, by folding
// conn.closed at read time (see TestPluginsReportsErroredAfterPostSpawnDeath)
// — inst.state itself stays stateRunning the whole time, since nothing ever
// writes the death back to it. stop's guard only compares inst.state, so it
// sees stateRunning and stores stateStopped, masking the crash as a clean
// shutdown; stop then nils liveConn, so the conn.closed fold in liveState
// can never fire again after that point either. This is the same
// clean-shutdown-vs-already-dead distinction
// TestErroredStaysErroredAfterClose proves for a spawn FAILURE; this proves
// it for a post-spawn death instead — TestErroredStaysErroredAfterClose
// never touches the stateRunning branch at all, so it cannot catch this.
//
// Red-verify: before the fix, stop unconditionally stored stateStopped for
// a stateRunning instance, with no check of conn.closed, so this read
// stopped instead of errored.
func TestErroredAfterPostSpawnDeathStaysErroredAfterClose(t *testing.T) {
	hostSide, pluginSide := net.Pipe()
	spec := Spec{
		Manifest: Manifest{Name: "flaky3", ProtocolVersion: ProtocolVersion, Hooks: []Hook{HookShellEnv}},
		dial: func() (io.ReadWriteCloser, error) {
			go serve(pluginSide, Manifest{Name: "flaky3"}, &Hooks{ //nolint:errcheck
				ShellEnv: func(_ context.Context, _ *Client, _ *ShellEnvRequest) (*ShellEnvResponse, error) {
					return &ShellEnvResponse{}, nil
				},
			})
			return hostSide, nil
		},
	}
	h := newTestHost(t, Options{}, spec)

	h.ShellEnv(context.Background(), &ShellEnvRequest{SessionID: "s1", Tool: "bash", Command: "ls"})
	inst := testInstance(h, "flaky3")
	c := inst.liveConn.Load()
	if c == nil {
		t.Fatal("liveConn not published after a successful spawn")
	}

	// Simulate the plugin process dying, exactly like
	// TestPluginsReportsErroredAfterPostSpawnDeath.
	if err := pluginSide.Close(); err != nil {
		t.Fatal(err)
	}
	<-c.closed // block on the actual death signal, not a sleep or a poll loop

	if got := h.Plugins()[0].State; got != PluginErrored {
		t.Fatalf("state after the plugin process died = %q, want %q", got, PluginErrored)
	}

	h.Close()

	if got := h.Plugins()[0].State; got != PluginErrored {
		t.Errorf("state after Close following a post-spawn death = %q, want %q", got, PluginErrored)
	}
}
