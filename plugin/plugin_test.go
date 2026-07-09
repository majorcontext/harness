package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
)

// testPlugin runs a plugin in-process over a net.Pipe and returns a Spec for
// the host side. It is a thin, *testing.T-taking wrapper around the
// exported NewTestSpec (which other packages use directly, since they can't
// reach Spec's unexported dial field).
func testPlugin(t *testing.T, name string, hooks *Hooks) Spec {
	t.Helper()
	return NewTestSpec(name, hooks)
}

func newTestHost(t *testing.T, opts Options, specs ...Spec) *Host {
	t.Helper()
	if opts.HookTimeout == 0 {
		opts.HookTimeout = 2 * time.Second
	}
	h, err := NewHost(opts, specs...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func TestChatParamsChain(t *testing.T) {
	temp := func(v float64) *float64 { return &v }
	p1 := testPlugin(t, "p1", &Hooks{
		ChatParams: func(_ context.Context, _ *Client, req *ChatParamsRequest) (*ChatParamsResponse, error) {
			params := req.Params
			params.Temperature = temp(0.2)
			return &ChatParamsResponse{Params: params}, nil
		},
	})
	p2 := testPlugin(t, "p2", &Hooks{
		ChatParams: func(_ context.Context, _ *Client, req *ChatParamsRequest) (*ChatParamsResponse, error) {
			// Chaining: must see p1's mutation.
			if req.Params.Temperature == nil || *req.Params.Temperature != 0.2 {
				t.Errorf("p2 did not see p1's temperature, got %v", req.Params.Temperature)
			}
			params := req.Params
			params.Model = message.ModelRef{Provider: "openai", Model: "gpt-5.5"}
			return &ChatParamsResponse{Params: params}, nil
		},
	})

	h := newTestHost(t, Options{}, p1, p2)
	got := h.ChatParams(context.Background(), &ChatParamsRequest{SessionID: "s1"})
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("temperature = %v, want 0.2", got.Temperature)
	}
	if got.Model.String() != "openai/gpt-5.5" {
		t.Errorf("model = %q, want openai/gpt-5.5", got.Model)
	}
}

func TestSystemTransformOrder(t *testing.T) {
	seg := func(name, s string) Spec {
		return testPlugin(t, name, &Hooks{
			SystemTransform: func(_ context.Context, _ *Client, _ *SystemTransformRequest) (*SystemTransformResponse, error) {
				return &SystemTransformResponse{Segments: []string{s}}, nil
			},
		})
	}
	h := newTestHost(t, Options{}, seg("a", "first"), seg("b", "second"))
	got := h.SystemTransform(context.Background(), &SystemTransformRequest{SessionID: "s1"})
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("segments = %v, want [first second]", got)
	}
}

func TestToolExecuteBeforeDeny(t *testing.T) {
	rewriter := testPlugin(t, "rewriter", &Hooks{
		ToolExecuteBefore: func(_ context.Context, _ *Client, req *ToolExecuteBeforeRequest) (*ToolExecuteBeforeResponse, error) {
			return &ToolExecuteBeforeResponse{Args: json.RawMessage(`{"command":"rewritten"}`)}, nil
		},
	})
	denier := testPlugin(t, "denier", &Hooks{
		ToolExecuteBefore: func(_ context.Context, _ *Client, req *ToolExecuteBeforeRequest) (*ToolExecuteBeforeResponse, error) {
			// Must see the rewritten args.
			if string(req.Args) != `{"command":"rewritten"}` {
				t.Errorf("denier saw args %s", req.Args)
			}
			return &ToolExecuteBeforeResponse{Deny: "merge conflicts detected"}, nil
		},
	})
	neverCalled := testPlugin(t, "never", &Hooks{
		ToolExecuteBefore: func(_ context.Context, _ *Client, _ *ToolExecuteBeforeRequest) (*ToolExecuteBeforeResponse, error) {
			t.Error("chain should have stopped at deny")
			return nil, nil
		},
	})

	h := newTestHost(t, Options{}, rewriter, denier, neverCalled)
	args, deny := h.ToolExecuteBefore(context.Background(), &ToolExecuteBeforeRequest{
		SessionID: "s1", CallID: "tc1", Tool: "bash", Args: json.RawMessage(`{"command":"gh pr create"}`),
	})
	if deny != "merge conflicts detected" {
		t.Errorf("deny = %q", deny)
	}
	if string(args) != `{"command":"rewritten"}` {
		t.Errorf("args = %s", args)
	}
}

func TestToolExecuteAfterMutation(t *testing.T) {
	guard := testPlugin(t, "guard", &Hooks{
		ToolExecuteAfter: func(_ context.Context, _ *Client, req *ToolExecuteAfterRequest) (*ToolExecuteAfterResponse, error) {
			return &ToolExecuteAfterResponse{
				Output: message.Parts{&message.Text{Text: "[REJECTED] " + req.Output.Text()}},
			}, nil
		},
	})
	h := newTestHost(t, Options{}, guard)
	out := h.ToolExecuteAfter(context.Background(), &ToolExecuteAfterRequest{
		SessionID: "s1", CallID: "tc1", Tool: "screenshot",
		Output: message.Parts{&message.Text{Text: "too big"}},
	})
	if out.Text() != "[REJECTED] too big" {
		t.Errorf("output = %q", out.Text())
	}
}

func TestShellEnvMerge(t *testing.T) {
	envPlugin := func(name string, env map[string]string) Spec {
		return testPlugin(t, name, &Hooks{
			ShellEnv: func(_ context.Context, _ *Client, _ *ShellEnvRequest) (*ShellEnvResponse, error) {
				return &ShellEnvResponse{Env: env}, nil
			},
		})
	}
	h := newTestHost(t, Options{},
		envPlugin("gh", map[string]string{"GH_TOKEN": "old", "A": "1"}),
		envPlugin("gh2", map[string]string{"GH_TOKEN": "new"}),
	)
	env := h.ShellEnv(context.Background(), &ShellEnvRequest{SessionID: "s1", Tool: "bash", Command: "gh pr view"})
	if env["GH_TOKEN"] != "new" || env["A"] != "1" {
		t.Errorf("env = %v", env)
	}
}

func TestCustomToolWithClientAPI(t *testing.T) {
	api := &stubClientAPI{
		mcp: func(req *MCPCallRequest) (*MCPCallResult, error) {
			if req.Server != "gateway" || req.Tool != "slack_post_message" {
				t.Errorf("unexpected MCP call: %+v", req)
			}
			return &MCPCallResult{Content: message.Parts{&message.Text{Text: "ok"}}}, nil
		},
	}
	uploader := testPlugin(t, "uploader", &Hooks{
		Tools: []Tool{{
			Def: ToolDef{
				Name:        "upload_file",
				Description: "Upload a file",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
			Execute: func(ctx context.Context, c *Client, args json.RawMessage) (message.Parts, error) {
				// Exercise the plugin → harness client API mid-execution.
				res, err := c.MCPCall(ctx, "gateway", "slack_post_message", map[string]string{"text": "hi"})
				if err != nil {
					return nil, err
				}
				return message.Parts{&message.Text{Text: "uploaded, slack said " + res.Content.Text()}}, nil
			},
		}},
	})

	h := newTestHost(t, Options{Client: api}, uploader)

	defs := h.Tools()
	if len(defs) != 1 || defs[0].Name != "upload_file" {
		t.Fatalf("Tools() = %+v", defs)
	}

	resp, err := h.ExecuteTool(context.Background(), &ToolExecuteRequest{
		SessionID: "s1", CallID: "tc1", Tool: "upload_file", Args: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Output.Text() != "uploaded, slack said ok" {
		t.Errorf("output = %q", resp.Output.Text())
	}
}

func TestEventDelivery(t *testing.T) {
	got := make(chan Event, 1)
	listener := testPlugin(t, "listener", &Hooks{
		Event: func(_ context.Context, _ *Client, events []Event) {
			for _, ev := range events {
				got <- ev
			}
		},
	})
	h := newTestHost(t, Options{}, listener)
	h.Emit([]Event{{Type: EventSessionStatus, SessionID: "s1", Properties: json.RawMessage(`{"status":"busy"}`)}})

	// Block directly; a delivery bug fails via the test binary timeout
	// rather than a guessed deadline.
	ev := <-got
	if ev.Type != EventSessionStatus || ev.SessionID != "s1" {
		t.Errorf("event = %+v", ev)
	}
}

func TestHookTimeoutFailsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var errs []Hook
		// The slow hook hangs until cleanup. Inside the bubble, fake time
		// advances to the host's timeout the moment every goroutine is
		// durably blocked — the timeout fires deterministically and the
		// test costs no wall clock. (A time.Sleep here would leak past the
		// bubble's end, which synctest reports as a deadlock.)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		slow := testPlugin(t, "slow", &Hooks{
			SystemTransform: func(ctx context.Context, _ *Client, _ *SystemTransformRequest) (*SystemTransformResponse, error) {
				<-release
				return &SystemTransformResponse{Segments: []string{"late"}}, nil
			},
		})
		fast := testPlugin(t, "fast", &Hooks{
			SystemTransform: func(_ context.Context, _ *Client, _ *SystemTransformRequest) (*SystemTransformResponse, error) {
				return &SystemTransformResponse{Segments: []string{"on time"}}, nil
			},
		})
		h := newTestHost(t, Options{
			HookTimeout: 50 * time.Millisecond,
			OnError: func(_ string, hook Hook, _ error) {
				errs = append(errs, hook)
			},
		}, slow, fast)

		got := h.SystemTransform(context.Background(), &SystemTransformRequest{SessionID: "s1"})
		if len(got) != 1 || got[0] != "on time" {
			t.Errorf("segments = %v, want [on time]", got)
		}
		if len(errs) != 1 || errs[0] != HookSystemTransform {
			t.Errorf("OnError calls = %v", errs)
		}
	})
}

func TestUnsubscribedPluginNeverSpawns(t *testing.T) {
	spawned := false
	spec := Spec{
		Manifest: Manifest{Name: "lazy", ProtocolVersion: ProtocolVersion, Hooks: []Hook{HookShellEnv}},
		dial: func() (io.ReadWriteCloser, error) {
			spawned = true
			hostSide, pluginSide := net.Pipe()
			go serve(pluginSide, Manifest{Name: "lazy"}, &Hooks{ //nolint:errcheck
				ShellEnv: func(_ context.Context, _ *Client, _ *ShellEnvRequest) (*ShellEnvResponse, error) {
					return nil, nil
				},
			})
			return hostSide, nil
		},
	}
	h := newTestHost(t, Options{}, spec)

	// Dispatching a hook the plugin doesn't subscribe to must not spawn it.
	h.SystemTransform(context.Background(), &SystemTransformRequest{SessionID: "s1"})
	if spawned {
		t.Fatal("plugin spawned for unsubscribed hook")
	}

	// Its own hook spawns it.
	h.ShellEnv(context.Background(), &ShellEnvRequest{SessionID: "s1", Tool: "bash", Command: "ls"})
	if !spawned {
		t.Fatal("plugin not spawned for subscribed hook")
	}
}

// TestProbeSpecPassesConfig proves finding (2): probing must send the
// spec's Config in the initialize handshake, exactly as a real spawn does
// (instance.startLocked), rather than the empty InitializeParams Probe(ctx,
// command) sends. A fake in-process plugin captures the InitializeParams it
// actually receives.
func TestProbeSpecPassesConfig(t *testing.T) {
	gotConfig := make(chan json.RawMessage, 1)
	hostSide, pluginSide := net.Pipe()
	go func() {
		c := newConn(pluginSide, func(_ context.Context, method string, params json.RawMessage) (any, error) {
			switch method {
			case methodInitialize:
				var init InitializeParams
				if err := json.Unmarshal(params, &init); err != nil {
					return nil, err
				}
				gotConfig <- init.Config
				return Manifest{Name: "cfgplug", ProtocolVersion: ProtocolVersion}, nil
			case methodShutdown:
				return nil, nil
			default:
				return nil, &rpcError{Code: codeMethodNotFound, Message: method}
			}
		})
		c.run() //nolint:errcheck
	}()

	spec := Spec{
		Command: []string{"unused"},
		Config:  json.RawMessage(`{"token":"abc123"}`),
		dial:    func() (io.ReadWriteCloser, error) { return hostSide, nil },
	}
	m, err := ProbeSpec(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "cfgplug" {
		t.Fatalf("manifest name = %q, want cfgplug", m.Name)
	}
	if cfg := <-gotConfig; string(cfg) != `{"token":"abc123"}` {
		t.Errorf("plugin received Config = %s, want {\"token\":\"abc123\"}", cfg)
	}
}

// TestProbeBackwardCompat proves Probe(ctx, command) — kept for backward
// compatibility — is exactly ProbeSpec with a bare command and no
// env/dir/config: both fail identically on an empty command.
func TestProbeBackwardCompat(t *testing.T) {
	_, err1 := Probe(context.Background(), nil)
	_, err2 := ProbeSpec(context.Background(), Spec{})
	if err1 == nil || err2 == nil {
		t.Fatal("expected both Probe and ProbeSpec to error on an empty command")
	}
	if err1.Error() != err2.Error() {
		t.Errorf("Probe error = %q, ProbeSpec error = %q, want identical", err1, err2)
	}
}

type stubClientAPI struct {
	mcp func(*MCPCallRequest) (*MCPCallResult, error)
}

func (s *stubClientAPI) SessionMessages(_ context.Context, _ *SessionMessagesRequest) (*SessionMessagesResponse, error) {
	return &SessionMessagesResponse{}, nil
}

func (s *stubClientAPI) MCPCall(_ context.Context, req *MCPCallRequest) (*MCPCallResult, error) {
	return s.mcp(req)
}

func (s *stubClientAPI) Generate(_ context.Context, _ *GenerateRequest) (*GenerateResponse, error) {
	return &GenerateResponse{}, nil
}
