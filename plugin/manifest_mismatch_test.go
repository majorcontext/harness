package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
)

// TestStartManifestNameMismatchFailsToStart covers startLocked's
// live-manifest-disagreement branch: a plugin whose install-time cache
// (spec.Manifest) says one name, but whose live initialize response
// reports a different one — e.g. the on-disk binary was replaced without
// re-running `harness plugin install` — must fail to start with the
// reinstall message, and the hook must never run.
//
// NewTestSpec's fake plugin always reports Manifest{Name: name} (the name
// given to NewTestSpec) at initialize time, independent of whatever the
// returned Spec's Manifest field is changed to afterward. Mutating
// spec.Manifest.Name post-construction reproduces exactly the stale-cache
// scenario: the cache (spec.Manifest) diverges from what the live process
// actually advertises.
func TestStartManifestNameMismatchFailsToStart(t *testing.T) {
	ran := false
	spec := testPlugin(t, "trusted-plugin", &Hooks{
		ChatParams: func(_ context.Context, _ *Client, _ *ChatParamsRequest) (*ChatParamsResponse, error) {
			ran = true
			return &ChatParamsResponse{}, nil
		},
	})
	spec.Manifest.Name = "trusted-plugin-v2" // stale cache: reinstalled binary renamed itself

	var errs []error
	h := newTestHost(t, Options{
		OnError: func(_ string, hook Hook, err error) {
			if hook != HookChatParams {
				t.Errorf("OnError hook = %v, want HookChatParams", hook)
			}
			errs = append(errs, err)
		},
	}, spec)

	got := h.ChatParams(context.Background(), &ChatParamsRequest{SessionID: "s1"})

	if ran {
		t.Error("hook must not run when live manifest disagrees with the cache")
	}
	if got.Model.String() != "" {
		t.Errorf("chain must not apply mutations, params = %+v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("OnError calls = %d, want 1", len(errs))
	}
	msg := errs[0].Error()
	if !strings.Contains(msg, "reinstall the plugin") {
		t.Errorf("error = %q, want it to mention reinstalling the plugin", msg)
	}
	if !strings.Contains(msg, "trusted-plugin-v2") || !strings.Contains(msg, "trusted-plugin") {
		t.Errorf("error = %q, want both the cached and live names", msg)
	}
}

// TestStartManifestProtocolVersionMismatchFailsToStart covers the other
// half of the same disjunct: a live initialize response whose
// ProtocolVersion disagrees with the cached manifest's. NewHost itself
// eagerly rejects a Spec whose cached ProtocolVersion doesn't match the
// version this harness build speaks, so the only way to exercise
// startLocked's own (runtime) comparison is a plugin that passes NewHost's
// cache check but lies about its protocol version at initialize time —
// simulating a binary that was rebuilt against a different SDK version
// without the cache being refreshed.
func TestStartManifestProtocolVersionMismatchFailsToStart(t *testing.T) {
	ran := false
	spec := Spec{
		Manifest: Manifest{Name: "versioned-plugin", ProtocolVersion: ProtocolVersion, Hooks: []Hook{HookChatParams}},
		dial: func() (io.ReadWriteCloser, error) {
			hostSide, pluginSide := net.Pipe()
			go func() {
				c := newConn(pluginSide, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
					if method == methodInitialize {
						// Reports the same name but a different protocol
						// version than the cache claims.
						return Manifest{Name: "versioned-plugin", ProtocolVersion: ProtocolVersion + 1}, nil
					}
					ran = true
					return &ChatParamsResponse{}, nil
				})
				_ = c.run()
			}()
			return hostSide, nil
		},
	}

	var errs []error
	h := newTestHost(t, Options{
		OnError: func(_ string, _ Hook, err error) { errs = append(errs, err) },
	}, spec)

	h.ChatParams(context.Background(), &ChatParamsRequest{SessionID: "s1"})

	if ran {
		t.Error("hook must not run when live protocol version disagrees with the cache")
	}
	if len(errs) != 1 {
		t.Fatalf("OnError calls = %d, want 1", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "reinstall the plugin") {
		t.Errorf("error = %q, want it to mention reinstalling the plugin", errs[0])
	}
}
