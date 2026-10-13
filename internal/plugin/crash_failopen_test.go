package plugin

import (
	"context"
	"testing"

	pub "github.com/majorcontext/harness/plugin"
)

// TestCrashMidHookFailsOpen covers a plugin that dies while a sync hook
// call is already in flight — as opposed to the existing
// TestHookTimeoutFailsOpen, which only exercises a plugin that never
// responds at all. A real crashed process severs its end of the stdio
// pipe; here the fake plugin's own hook handler closes its connection
// (client.c, the plugin-side conn — reachable because this test lives in
// package plugin) to reproduce exactly that signal without spawning a real
// subprocess.
//
// The host must fail open for the crashed plugin (report it via OnError,
// apply none of its response) and — the actual gap this closes — every
// OTHER plugin later in the chain must still run and have its mutation
// applied, matching the design rule that one wedged/dead plugin cannot
// take a session down.
func TestCrashMidHookFailsOpen(t *testing.T) {
	var sever func()
	crasher := testPlugin(t, "crasher", &pub.Plugin{
		SystemTransform: func(_ context.Context, _ *pub.Client, _ *pub.SystemTransformRequest) (*pub.SystemTransformResponse, error) {
			sever()
			return &pub.SystemTransformResponse{Segments: []string{"should never be applied"}}, nil
		},
	})
	sever = severable(&crasher)
	survivor := testPlugin(t, "survivor", &pub.Plugin{
		SystemTransform: func(_ context.Context, _ *pub.Client, _ *pub.SystemTransformRequest) (*pub.SystemTransformResponse, error) {
			return &pub.SystemTransformResponse{Segments: []string{"survivor ran"}}, nil
		},
	})

	var errs []string
	h := newTestHost(t, Options{
		OnError: func(name string, hook pub.Hook, _ error) {
			if hook != pub.HookSystemTransform {
				t.Errorf("OnError hook = %v, want pub.HookSystemTransform", hook)
			}
			errs = append(errs, name)
		},
	}, crasher, survivor)

	got := h.SystemTransform(context.Background(), &pub.SystemTransformRequest{SessionID: "s1"})

	if len(got) != 1 || got[0] != "survivor ran" {
		t.Errorf("segments = %v, want [survivor ran]", got)
	}
	if len(errs) != 1 || errs[0] != "crasher" {
		t.Errorf("OnError calls = %v, want [crasher]", errs)
	}
}

// TestCrashMidHookPanicFailsOpen is the "panics mid-call" variant of the
// same gap. The panic is recovered inside the hook handler itself — a
// handler panicking in package plugin's own in-process fake-plugin
// goroutine would otherwise crash the whole test binary, since nothing in
// the real dispatch path recovers panics (a real plugin process panicking
// just takes down that OS process, which the host observes as a severed
// connection; recovering here and severing the connection ourselves
// reproduces that same observable failure without an actual process to
// crash). Chain-continuation behavior must be identical to the
// closed-pipe case above.
func TestCrashMidHookPanicFailsOpen(t *testing.T) {
	crasher := testPlugin(t, "crasher", &pub.Plugin{
		SystemTransform: func(context.Context, *pub.Client, *pub.SystemTransformRequest) (*pub.SystemTransformResponse, error) {
			panic("simulated plugin crash mid-hook")
		},
	})
	survivor := testPlugin(t, "survivor", &pub.Plugin{
		SystemTransform: func(_ context.Context, _ *pub.Client, _ *pub.SystemTransformRequest) (*pub.SystemTransformResponse, error) {
			return &pub.SystemTransformResponse{Segments: []string{"survivor ran"}}, nil
		},
	})

	var errs []string
	h := newTestHost(t, Options{
		OnError: func(name string, _ pub.Hook, _ error) { errs = append(errs, name) },
	}, crasher, survivor)

	got := h.SystemTransform(context.Background(), &pub.SystemTransformRequest{SessionID: "s1"})

	if len(got) != 1 || got[0] != "survivor ran" {
		t.Errorf("segments = %v, want [survivor ran]", got)
	}
	if len(errs) != 1 || errs[0] != "crasher" {
		t.Errorf("OnError calls = %v, want [crasher]", errs)
	}
}
