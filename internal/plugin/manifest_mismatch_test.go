package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"slices"
	"strings"
	"testing"

	pub "github.com/majorcontext/harness/plugin"

	"github.com/majorcontext/harness/internal/plugin/wire"
)

// A plugin whose live initialize response disagrees with the cached manifest
// in its name or its protocol version fails to start with the reinstall
// message, and its hook never runs. NewHost rejects a cached protocol version
// that this build does not speak, so the protocol case needs a plugin that
// lies at initialize time.
func TestStartManifestMismatchFailsToStart(t *testing.T) {
	var ran bool
	cached := pub.Manifest{Name: "p", ProtocolVersion: pub.ProtocolVersion, Hooks: []pub.Hook{pub.HookChatParams}}
	for name, tc := range map[string]struct {
		live pub.Manifest
		want string
	}{
		"name":             {pub.Manifest{Name: "p-v2", ProtocolVersion: pub.ProtocolVersion}, "p-v2"},
		"protocol version": {pub.Manifest{Name: "p", ProtocolVersion: pub.ProtocolVersion + 1}, "protocol version"},
	} {
		t.Run(name, func(t *testing.T) {
			ran = false
			spec := Spec{Manifest: cached, dial: func() (io.ReadWriteCloser, error) {
				hostSide, pluginSide := net.Pipe()
				go func() {
					c := wire.NewConn(pluginSide, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
						if method == wire.MethodInitialize {
							return tc.live, nil
						}
						ran = true
						return &pub.ChatParamsResponse{}, nil
					})
					_ = c.Run()
				}()
				return hostSide, nil
			}}
			var errs []error
			var hooks []pub.Hook
			h := newTestHost(t, Options{OnError: func(_ string, hook pub.Hook, err error) {
				hooks, errs = append(hooks, hook), append(errs, err)
			}}, spec)
			got := h.ChatParams(context.Background(), &pub.ChatParamsRequest{SessionID: "s1"})
			if ran || got.Model != "" || !slices.Equal(hooks, []pub.Hook{pub.HookChatParams}) {
				t.Fatalf("hook ran = %v, params = %+v, OnError hooks = %v, want no run, unchanged params, and one chat.params error", ran, got, hooks)
			}
			for _, want := range []string{"reinstall the plugin", tc.want, "p"} {
				if !strings.Contains(errs[0].Error(), want) {
					t.Errorf("error = %q, want it to contain %q", errs[0], want)
				}
			}
		})
	}
}
