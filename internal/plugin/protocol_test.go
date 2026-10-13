package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"

	pub "github.com/majorcontext/harness/plugin"

	"github.com/majorcontext/harness/internal/plugin/wire"
)

// fakeClientAPI implements ClientAPI for TestHostNotificationsDroppedNeverBlocksResponses:
// only MCPCall does anything (its behavior is test-controlled); the other
// two methods are never exercised by that test.
type fakeClientAPI struct {
	mcpCall func(ctx context.Context, req *pub.MCPCallRequest) (*pub.MCPCallResult, error)
}

func (f *fakeClientAPI) SessionMessages(context.Context, *pub.SessionMessagesRequest) (*pub.SessionMessagesResponse, error) {
	return nil, errors.New("fakeClientAPI: SessionMessages not implemented")
}

func (f *fakeClientAPI) MCPCall(ctx context.Context, req *pub.MCPCallRequest) (*pub.MCPCallResult, error) {
	return f.mcpCall(ctx, req)
}

func (f *fakeClientAPI) Generate(context.Context, *pub.GenerateRequest) (*pub.GenerateResponse, error) {
	return nil, errors.New("fakeClientAPI: Generate not implemented")
}

// TestHostNotificationsDroppedNeverBlocksResponses is the Host-level
// counterpart to TestNotifyBacklogNeverBlocksReadLoop: it exercises the same
// fix through the public API (Host.NotificationsDropped) rather than by
// reaching into conn's unexported field, and does so via the same conn
// machinery Host actually uses to talk to a plugin process.
//
// dispatchNotification's drop rule protects both sides of a connection, not
// just a plugin's inbound hook/event stream: a plugin could just as well
// send the harness a notification-shaped message for a method that would
// normally be a request (here, "client/mcp.call" with no id). This proves
// that an adversarial or malfunctioning plugin flooding the harness with
// those cannot wedge Host's read loop either — the flood is dropped past
// notifyCh's capacity (observable via Host.NotificationsDropped) instead of
// blocking, so a legitimate hook round trip (SystemTransform) started before
// the flood still completes once the fake plugin gets around to answering
// it.
func TestHostNotificationsDroppedNeverBlocksResponses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hostSide, fakePluginSide := net.Pipe()

		release := make(chan struct{})
		wedged := make(chan struct{})
		var wedgeOnce sync.Once
		client := &fakeClientAPI{
			mcpCall: func(context.Context, *pub.MCPCallRequest) (*pub.MCPCallResult, error) {
				wedgeOnce.Do(func() { close(wedged) })
				<-release
				return &pub.MCPCallResult{}, nil
			},
		}

		const pluginName = "adversary"
		manifest := pub.Manifest{Name: pluginName, ProtocolVersion: pub.ProtocolVersion, Hooks: []pub.Hook{pub.HookSystemTransform}}
		h := newTestHost(t, Options{Client: client}, Spec{
			Manifest: manifest,
			dial:     func() (io.ReadWriteCloser, error) { return hostSide, nil },
		})
		t.Cleanup(func() {
			close(release)
			_ = hostSide.Close()
			_ = fakePluginSide.Close()
		})

		fakeDone := make(chan struct{})
		go func() {
			defer close(fakeDone)
			r := bufio.NewReader(fakePluginSide)
			w := bufio.NewWriter(fakePluginSide)
			readMsg := func() wire.Message {
				line, err := r.ReadBytes('\n')
				if err != nil {
					return wire.Message{}
				}
				var m wire.Message
				_ = json.Unmarshal(line, &m)
				return m
			}
			writeMsg := func(m wire.Message) {
				raw, err := json.Marshal(m)
				if err != nil {
					t.Error(err)
					return
				}
				raw = append(raw, '\n')
				if _, err := w.Write(raw); err != nil {
					return
				}
				_ = w.Flush()
			}

			// Answer the initialize handshake so the instance starts.
			init := readMsg()
			manifestRaw, err := json.Marshal(manifest)
			if err != nil {
				t.Error(err)
				return
			}
			writeMsg(wire.Message{JSONRPC: "2.0", ID: init.ID, Result: manifestRaw})

			// Flood the harness with notification-shaped "client/mcp.call"
			// messages: the first one wedges Host's single notification
			// consumer inside fakeClientAPI.MCPCall (via wedgeOnce/release
			// above), then every send after notifyCh fills must be
			// dropped rather than block this write (net.Pipe is
			// synchronous, so each send here paces against Host's read
			// loop actually consuming it).
			const overflow = 200
			for i := 0; i < wire.NotifyQueueSize+overflow; i++ {
				writeMsg(wire.Message{JSONRPC: "2.0", Method: "client/mcp.call", Params: json.RawMessage(`{}`)})
			}

			// Only now read and answer the hook/system.transform request
			// Host sent independently (and blocked writing/awaiting,
			// possibly since before the flood even started) — proving
			// Host's read loop was never stuck on the flood above.
			req := readMsg()
			respRaw, err := json.Marshal(pub.SystemTransformResponse{Segments: []string{"adversary survived"}})
			if err != nil {
				t.Error(err)
				return
			}
			writeMsg(wire.Message{JSONRPC: "2.0", ID: req.ID, Result: respRaw})
		}()

		got := h.SystemTransform(context.Background(), &pub.SystemTransformRequest{SessionID: "s1"})
		<-fakeDone

		if len(got) != 1 || got[0] != "adversary survived" {
			t.Fatalf("segments = %v, want [adversary survived] (Host's read loop must not have been wedged by the flood)", got)
		}
		if dropped := h.NotificationsDropped(pluginName); dropped == 0 {
			t.Fatal("Host.NotificationsDropped(\"adversary\") = 0, want > 0 after flooding past notifyCh's capacity")
		}
	})
}
