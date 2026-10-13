package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	pub "github.com/majorcontext/harness/plugin"

	"github.com/majorcontext/harness/internal/plugin/wire"
)

// testPlugin serves p in-process over a net.Pipe, with no subprocess, and
// returns a Spec for the host side.
func testPlugin(t *testing.T, name string, p *pub.Plugin) Spec {
	t.Helper()
	p.Name = name
	return Spec{
		Manifest: p.Manifest(),
		dial: func() (io.ReadWriteCloser, error) {
			hostSide, pluginSide := net.Pipe()
			go pub.ServeConn(pluginSide, p) //nolint:errcheck
			return hostSide, nil
		},
	}
}

// severable returns a func that closes the host side of the pipe that spec dials.
func severable(spec *Spec) func() {
	sides := make(chan io.Closer, 1)
	dial := spec.dial
	spec.dial = func() (io.ReadWriteCloser, error) {
		rwc, err := dial()
		sides <- rwc
		return rwc, err
	}
	return func() { _ = (<-sides).Close() }
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
	p1 := testPlugin(t, "p1", &pub.Plugin{
		ChatParams: func(_ context.Context, _ *pub.Client, req *pub.ChatParamsRequest) (*pub.ChatParamsResponse, error) {
			params := req.Params
			params.Temperature = temp(0.2)
			return &pub.ChatParamsResponse{Params: params}, nil
		},
	})
	p2 := testPlugin(t, "p2", &pub.Plugin{
		ChatParams: func(_ context.Context, _ *pub.Client, req *pub.ChatParamsRequest) (*pub.ChatParamsResponse, error) {
			// Chaining: must see p1's mutation.
			if req.Params.Temperature == nil || *req.Params.Temperature != 0.2 {
				t.Errorf("p2 did not see p1's temperature, got %v", req.Params.Temperature)
			}
			params := req.Params
			params.Model = "openai/gpt-5.5"
			return &pub.ChatParamsResponse{Params: params}, nil
		},
	})

	h := newTestHost(t, Options{}, p1, p2)
	got := h.ChatParams(context.Background(), &pub.ChatParamsRequest{SessionID: "s1"})
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("temperature = %v, want 0.2", got.Temperature)
	}
	if got.Model != "openai/gpt-5.5" {
		t.Errorf("model = %q, want openai/gpt-5.5", got.Model)
	}
}

func TestSystemTransformOrder(t *testing.T) {
	seg := func(name, s string) Spec {
		return testPlugin(t, name, &pub.Plugin{
			SystemTransform: func(_ context.Context, _ *pub.Client, _ *pub.SystemTransformRequest) (*pub.SystemTransformResponse, error) {
				return &pub.SystemTransformResponse{Segments: []string{s}}, nil
			},
		})
	}
	h := newTestHost(t, Options{}, seg("a", "first"), seg("b", "second"))
	got := h.SystemTransform(context.Background(), &pub.SystemTransformRequest{SessionID: "s1"})
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("segments = %v, want [first second]", got)
	}
}

func TestToolExecuteBeforeDeny(t *testing.T) {
	rewriter := testPlugin(t, "rewriter", &pub.Plugin{
		ToolExecuteBefore: &pub.ToolBefore{Run: func(_ context.Context, _ *pub.Client, req *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
			return &pub.ToolExecuteBeforeResponse{Args: json.RawMessage(`{"command":"rewritten"}`)}, nil
		}},
	})
	denier := testPlugin(t, "denier", &pub.Plugin{
		ToolExecuteBefore: &pub.ToolBefore{Run: func(_ context.Context, _ *pub.Client, req *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
			// Must see the rewritten args.
			if string(req.Args) != `{"command":"rewritten"}` {
				t.Errorf("denier saw args %s", req.Args)
			}
			return &pub.ToolExecuteBeforeResponse{Deny: "merge conflicts detected"}, nil
		}},
	})
	neverCalled := testPlugin(t, "never", &pub.Plugin{
		ToolExecuteBefore: &pub.ToolBefore{Run: func(_ context.Context, _ *pub.Client, _ *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
			t.Error("chain should have stopped at deny")
			return nil, nil
		}},
	})

	h := newTestHost(t, Options{}, rewriter, denier, neverCalled)
	args, deny := h.ToolExecuteBefore(context.Background(), &pub.ToolExecuteBeforeRequest{
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
	guard := testPlugin(t, "guard", &pub.Plugin{
		ToolExecuteAfter: func(_ context.Context, _ *pub.Client, req *pub.ToolExecuteAfterRequest) (*pub.ToolExecuteAfterResponse, error) {
			return &pub.ToolExecuteAfterResponse{
				Output: []pub.Part{pub.TextPart("[REJECTED] " + req.Output[0].Text)},
			}, nil
		},
	})
	h := newTestHost(t, Options{}, guard)
	out := h.ToolExecuteAfter(context.Background(), &pub.ToolExecuteAfterRequest{
		SessionID: "s1", CallID: "tc1", Tool: "screenshot",
		Output: []pub.Part{pub.TextPart("too big")},
	})
	if out[0].Text != "[REJECTED] too big" {
		t.Errorf("output = %q", out[0].Text)
	}
}

func TestShellEnvMerge(t *testing.T) {
	envPlugin := func(name string, env map[string]string) Spec {
		return testPlugin(t, name, &pub.Plugin{
			ShellEnv: func(_ context.Context, _ *pub.Client, _ *pub.ShellEnvRequest) (*pub.ShellEnvResponse, error) {
				return &pub.ShellEnvResponse{Env: env}, nil
			},
		})
	}
	h := newTestHost(t, Options{},
		envPlugin("gh", map[string]string{"GH_TOKEN": "old", "A": "1"}),
		envPlugin("gh2", map[string]string{"GH_TOKEN": "new"}),
	)
	env := h.ShellEnv(context.Background(), &pub.ShellEnvRequest{SessionID: "s1", Tool: "bash", Command: "gh pr view"})
	if env["GH_TOKEN"] != "new" || env["A"] != "1" {
		t.Errorf("env = %v", env)
	}
}

func TestCustomToolWithClientAPI(t *testing.T) {
	api := &stubClientAPI{
		messages: func(req *pub.SessionMessagesRequest) (*pub.SessionMessagesResponse, error) {
			if req.SessionID != "s1" {
				t.Errorf("unexpected session: %+v", req)
			}
			return &pub.SessionMessagesResponse{Messages: []pub.Message{{ID: "m1", Role: "user", Parts: []pub.Part{pub.TextPart("ok")}}}}, nil
		},
	}
	uploader := testPlugin(t, "uploader", &pub.Plugin{
		Tools: []pub.Tool{{
			Def: pub.ToolDef{
				Name:        "upload_file",
				Description: "Upload a file",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
			Execute: func(ctx context.Context, c *pub.Client, args json.RawMessage) ([]pub.Part, error) {
				// Exercise the plugin → harness client API mid-execution.
				res, err := c.SessionMessages(ctx, "s1")
				if err != nil {
					return nil, err
				}
				_, mcpErr := c.MCPCall(ctx, "gateway", "slack_post_message", json.RawMessage(`{"text":"hi"}`))
				return []pub.Part{pub.TextPart("uploaded, history said " + res[0].Parts[0].Text + "; mcp: " + fmt.Sprint(mcpErr))}, nil
			},
		}},
	})

	h := newTestHost(t, Options{Client: api}, uploader)

	defs := h.Tools()
	if len(defs) != 1 || defs[0].Name != "upload_file" {
		t.Fatalf("Tools() = %+v", defs)
	}

	resp, err := h.ExecuteTool(context.Background(), &pub.ToolExecuteRequest{
		SessionID: "s1", CallID: "tc1", Tool: "upload_file", Args: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Output[0].Text != "uploaded, history said ok; mcp: plugin: rpc error -32603: stub mcp failure" {
		t.Errorf("output = %q", resp.Output[0].Text)
	}
}

func TestHookTimeoutFailsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var errs []pub.Hook
		// The slow hook hangs until cleanup. Inside the bubble, fake time
		// advances to the host's timeout the moment every goroutine is
		// durably blocked — the timeout fires deterministically and the
		// test costs no wall clock. (A time.Sleep here would leak past the
		// bubble's end, which synctest reports as a deadlock.)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		slow := testPlugin(t, "slow", &pub.Plugin{
			SystemTransform: func(ctx context.Context, _ *pub.Client, _ *pub.SystemTransformRequest) (*pub.SystemTransformResponse, error) {
				<-release
				return &pub.SystemTransformResponse{Segments: []string{"late"}}, nil
			},
		})
		fast := testPlugin(t, "fast", &pub.Plugin{
			SystemTransform: func(_ context.Context, _ *pub.Client, _ *pub.SystemTransformRequest) (*pub.SystemTransformResponse, error) {
				return &pub.SystemTransformResponse{Segments: []string{"on time"}}, nil
			},
		})
		h := newTestHost(t, Options{
			HookTimeout: 50 * time.Millisecond,
			OnError: func(_ string, hook pub.Hook, _ error) {
				errs = append(errs, hook)
			},
		}, slow, fast)

		got := h.SystemTransform(context.Background(), &pub.SystemTransformRequest{SessionID: "s1"})
		if len(got) != 1 || got[0] != "on time" {
			t.Errorf("segments = %v, want [on time]", got)
		}
		if len(errs) != 1 || errs[0] != pub.HookSystemTransform {
			t.Errorf("OnError calls = %v", errs)
		}
	})
}

func TestCanceledContextDuringFlaggedHookDeniesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		arrived := make(chan struct{})
		t.Cleanup(func() { close(release) })
		spec := testPlugin(t, "guard", &pub.Plugin{
			ToolExecuteBefore: &pub.ToolBefore{DenyOnFailure: true, Run: func(_ context.Context, _ *pub.Client, _ *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
				close(arrived)
				<-release
				return &pub.ToolExecuteBeforeResponse{}, nil
			}},
		})
		h := newTestHost(t, Options{HookTimeout: time.Hour}, spec)

		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			args json.RawMessage
			deny string
		}
		done := make(chan result)
		go func() {
			args, deny := h.ToolExecuteBefore(ctx, &pub.ToolExecuteBeforeRequest{SessionID: "s1", CallID: "c1", Tool: "bash", Args: json.RawMessage(`{}`)})
			done <- result{args, deny}
		}()
		<-arrived
		cancel()
		got := <-done
		if got.deny != "" {
			t.Errorf("deny = %q, want none: a canceled context is an abort, and the caller checks ctx", got.deny)
		}
		if string(got.args) != `{}` {
			t.Errorf("args = %s, want the unchanged {}", got.args)
		}
	})
}

func TestLiveFlagWithStaleCacheDeniesAtSpawn(t *testing.T) {
	spec := testPlugin(t, "guard", &pub.Plugin{
		ToolExecuteBefore: &pub.ToolBefore{DenyOnFailure: true, Run: func(_ context.Context, _ *pub.Client, _ *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
			return &pub.ToolExecuteBeforeResponse{}, nil
		}},
	})
	spec.Manifest.DenyOnFailure = nil
	h := newTestHost(t, Options{}, spec)
	_, deny := h.ToolExecuteBefore(context.Background(), &pub.ToolExecuteBeforeRequest{SessionID: "s1", CallID: "c1", Tool: "bash", Args: json.RawMessage(`{}`)})
	if !strings.HasPrefix(deny, "denied: plugin guard failed: ") {
		t.Errorf("deny = %q, want a deny for the start failure of a plugin that the live manifest flags", deny)
	}
}

func TestUnsubscribedPluginNeverSpawns(t *testing.T) {
	spawned := false
	spec := Spec{
		Manifest: pub.Manifest{Name: "lazy", ProtocolVersion: pub.ProtocolVersion, Hooks: []pub.Hook{pub.HookShellEnv}},
		dial: func() (io.ReadWriteCloser, error) {
			spawned = true
			hostSide, pluginSide := net.Pipe()
			go pub.ServeConn(pluginSide, &pub.Plugin{Name: "lazy", //nolint:errcheck
				ShellEnv: func(_ context.Context, _ *pub.Client, _ *pub.ShellEnvRequest) (*pub.ShellEnvResponse, error) {
					return nil, nil
				},
			})
			return hostSide, nil
		},
	}
	h := newTestHost(t, Options{}, spec)

	// Dispatching a hook the plugin doesn't subscribe to must not spawn it.
	h.SystemTransform(context.Background(), &pub.SystemTransformRequest{SessionID: "s1"})
	if spawned {
		t.Fatal("plugin spawned for unsubscribed hook")
	}

	// Its own hook spawns it.
	h.ShellEnv(context.Background(), &pub.ShellEnvRequest{SessionID: "s1", Tool: "bash", Command: "ls"})
	if !spawned {
		t.Fatal("plugin not spawned for subscribed hook")
	}
}

// TestPluginsReportsManifestAndState verifies Host.Plugins reports every
// CONFIGURED plugin — from the cached manifest — with its tools and hooks,
// and that its state tracks the live spawn: "not-spawned" before any dispatch,
// "running" after the plugin's own hook spawns it.
func TestPluginsReportsManifestAndState(t *testing.T) {
	spec := testPlugin(t, "guard", &pub.Plugin{
		SystemTransform: func(_ context.Context, _ *pub.Client, _ *pub.SystemTransformRequest) (*pub.SystemTransformResponse, error) {
			return &pub.SystemTransformResponse{Segments: []string{"seg"}}, nil
		},
		Tools: []pub.Tool{{
			Def:     pub.ToolDef{Name: "scan_file", Description: "d", InputSchema: json.RawMessage(`{}`)},
			Execute: func(context.Context, *pub.Client, json.RawMessage) ([]pub.Part, error) { return nil, nil },
		}},
	})
	h := newTestHost(t, Options{}, spec)

	infos := h.Plugins()
	if len(infos) != 1 {
		t.Fatalf("Plugins() = %+v, want exactly one", infos)
	}
	p := infos[0]
	if p.Name != "guard" {
		t.Errorf("name = %q, want guard", p.Name)
	}
	if p.State != PluginNotSpawned {
		t.Errorf("state = %q, want %q before any dispatch", p.State, PluginNotSpawned)
	}
	if len(p.Tools) != 1 || p.Tools[0] != "scan_file" {
		t.Errorf("tools = %v, want [scan_file]", p.Tools)
	}
	if len(p.Hooks) != 1 || p.Hooks[0] != string(pub.HookSystemTransform) {
		t.Errorf("hooks = %v, want [system.transform]", p.Hooks)
	}

	// Its own hook spawns it; the reported state must follow.
	h.SystemTransform(context.Background(), &pub.SystemTransformRequest{SessionID: "s1"})
	if got := h.Plugins()[0].State; got != PluginRunning {
		t.Errorf("state = %q after dispatch, want %q", got, PluginRunning)
	}
}

// TestProbeSpecPassesConfig verifies that probing sends the
// spec's Config in the initialize handshake, exactly as a real spawn does
// (instance.startLocked), rather than the empty InitializeParams Probe(ctx,
// command) sends. A fake in-process plugin captures the InitializeParams it
// actually receives.
func TestProbeSpecPassesConfig(t *testing.T) {
	gotConfig := make(chan json.RawMessage, 1)
	hostSide, pluginSide := net.Pipe()
	go func() {
		c := wire.NewConn(pluginSide, func(_ context.Context, method string, params json.RawMessage) (any, error) {
			switch method {
			case wire.MethodInitialize:
				var init pub.InitializeParams
				if err := json.Unmarshal(params, &init); err != nil {
					return nil, err
				}
				gotConfig <- init.Config
				return pub.Manifest{Name: "cfgplug", ProtocolVersion: pub.ProtocolVersion}, nil
			case wire.MethodShutdown:
				return nil, nil
			default:
				return nil, &wire.Error{Code: wire.CodeMethodNotFound, Message: method}
			}
		})
		c.Run() //nolint:errcheck
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
	messages func(*pub.SessionMessagesRequest) (*pub.SessionMessagesResponse, error)
}

func (s *stubClientAPI) SessionMessages(_ context.Context, req *pub.SessionMessagesRequest) (*pub.SessionMessagesResponse, error) {
	return s.messages(req)
}

func (s *stubClientAPI) MCPCall(_ context.Context, req *pub.MCPCallRequest) (*pub.MCPCallResult, error) {
	if req.Server != "gateway" || req.Tool != "slack_post_message" || string(req.Args) != `{"text":"hi"}` {
		return nil, fmt.Errorf("unexpected call %+v", req)
	}
	return nil, errors.New("stub mcp failure")
}

func (s *stubClientAPI) Generate(_ context.Context, _ *pub.GenerateRequest) (*pub.GenerateResponse, error) {
	return &pub.GenerateResponse{}, nil
}

func TestMalformedResponsesFailTheHook(t *testing.T) {
	bad := testPlugin(t, "bad", &pub.Plugin{
		ChatParams: func(context.Context, *pub.Client, *pub.ChatParamsRequest) (*pub.ChatParamsResponse, error) {
			return &pub.ChatParamsResponse{Params: pub.ChatParams{Model: "no-slash"}}, nil
		},
		ToolExecuteAfter: func(context.Context, *pub.Client, *pub.ToolExecuteAfterRequest) (*pub.ToolExecuteAfterResponse, error) {
			return &pub.ToolExecuteAfterResponse{Output: []pub.Part{{Type: "bogus", Raw: json.RawMessage(`{"type":"bogus"}`)}}}, nil
		},
		Tools: []pub.Tool{{Def: pub.ToolDef{Name: "bad_tool", InputSchema: json.RawMessage(`{}`)}, Execute: func(context.Context, *pub.Client, json.RawMessage) ([]pub.Part, error) {
			return []pub.Part{{Type: "bogus", Raw: json.RawMessage(`{"type":"bogus"}`)}}, nil
		}}},
	})
	var hooks []pub.Hook
	var causes []string
	h := newTestHost(t, Options{OnError: func(_ string, hook pub.Hook, err error) {
		hooks = append(hooks, hook)
		causes = append(causes, err.Error())
	}}, bad)
	params := h.ChatParams(context.Background(), &pub.ChatParamsRequest{SessionID: "s1", Params: pub.ChatParams{Model: "a/b"}})
	out := h.ToolExecuteAfter(context.Background(), &pub.ToolExecuteAfterRequest{SessionID: "s1", Output: []pub.Part{pub.TextPart("kept")}})
	_, err := h.ExecuteTool(context.Background(), &pub.ToolExecuteRequest{SessionID: "s1", Tool: "bad_tool", Args: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), `unknown part type "bogus"`) {
		t.Errorf("ExecuteTool error = %v, want the unknown part type", err)
	}
	if len(causes) != 2 || !strings.Contains(causes[0], `is not of the form "provider/model"`) || !strings.Contains(causes[1], `unknown part type "bogus"`) {
		t.Errorf("OnError causes = %q, want the model ref error and the unknown part type", causes)
	}
	if params.Model != "a/b" || out[0].Text != "kept" {
		t.Errorf("params = %+v, output = %+v, want both unchanged", params, out)
	}
	if !slices.Equal(hooks, []pub.Hook{pub.HookChatParams, pub.HookToolExecuteAfter}) {
		t.Errorf("OnError hooks = %v", hooks)
	}
}

func TestHookTimeoutsSetTheDeadlineOfEachHook(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slowBefore := func(wait time.Duration) *pub.ToolBefore {
			return &pub.ToolBefore{Run: func(ctx context.Context, _ *pub.Client, _ *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
				timer := time.NewTimer(wait)
				defer timer.Stop()
				select {
				case <-timer.C:
					return &pub.ToolExecuteBeforeResponse{Deny: "slow hook finished"}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}
		}
		long := testPlugin(t, "long", &pub.Plugin{
			ToolExecuteBefore: slowBefore(7 * time.Second),
			HookTimeouts:      map[pub.Hook]time.Duration{pub.HookToolExecuteBefore: 10 * time.Second},
		})
		short := testPlugin(t, "short", &pub.Plugin{
			ToolExecuteBefore: slowBefore(time.Second),
			HookTimeouts:      map[pub.Hook]time.Duration{pub.HookToolExecuteBefore: 500 * time.Millisecond},
		})
		unset := testPlugin(t, "unset", &pub.Plugin{ToolExecuteBefore: slowBefore(7 * time.Second)})
		call := func(spec Spec) string {
			h := newTestHost(t, Options{HookTimeout: 5 * time.Second}, spec)
			_, deny := h.ToolExecuteBefore(context.Background(), &pub.ToolExecuteBeforeRequest{SessionID: "s1", CallID: "c1", Tool: "bash", Args: json.RawMessage(`{}`)})
			return deny
		}
		if got := call(long); got != "slow hook finished" {
			t.Errorf("7 s hook with a 10 s timeout: deny = %q, want the hook result", got)
		}
		if got := call(short); got != "" {
			t.Errorf("1 s hook with a 500 ms timeout: deny = %q, want the hook cut off", got)
		}
		if got := call(unset); got != "" {
			t.Errorf("7 s hook with the 5 s default: deny = %q, want the hook cut off", got)
		}
	})
}

func TestSubMillisecondHookTimeoutFailsNewHost(t *testing.T) {
	p := &pub.Plugin{
		ToolExecuteBefore: &pub.ToolBefore{Run: func(context.Context, *pub.Client, *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
			return nil, nil
		}},
		HookTimeouts: map[pub.Hook]time.Duration{pub.HookToolExecuteBefore: time.Microsecond},
	}
	want := "plugin t: hook_timeouts for tool.execute.before must be from 1 to 600000 ms"
	if _, err := NewHost(Options{}, testPlugin(t, "t", p)); err == nil || err.Error() != want {
		t.Errorf("NewHost error = %v, want %q", err, want)
	}
}

func TestShortHookTimeoutDoesNotShortenTheStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const startTime = time.Second
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		var hookRan atomic.Bool
		spec := testPlugin(t, "slowstart", &pub.Plugin{
			ChatParams: func(context.Context, *pub.Client, *pub.ChatParamsRequest) (*pub.ChatParamsResponse, error) {
				hookRan.Store(true)
				<-release
				return &pub.ChatParamsResponse{}, nil
			},
			HookTimeouts: map[pub.Hook]time.Duration{pub.HookChatParams: 50 * time.Millisecond},
		})
		dial := spec.dial
		spec.dial = func() (io.ReadWriteCloser, error) {
			timer := time.NewTimer(startTime)
			defer timer.Stop()
			<-timer.C
			return dial()
		}
		var errs []error
		h := newTestHost(t, Options{OnError: func(_ string, _ pub.Hook, err error) { errs = append(errs, err) }}, spec)
		begin := time.Now()
		h.ChatParams(context.Background(), &pub.ChatParamsRequest{SessionID: "s1"})
		if !hookRan.Load() {
			t.Errorf("hook never ran; errors = %v", errs)
		}
		if got, want := time.Since(begin), startTime+50*time.Millisecond; got != want {
			t.Errorf("dispatch took %v, want %v: the start has the host default and the hook has its own timeout", got, want)
		}
		if len(errs) != 1 || !errors.Is(errs[0], context.DeadlineExceeded) {
			t.Errorf("errors = %v, want one deadline error from the hook", errs)
		}
	})
}

func TestStartHookTimeoutsMismatchFailsToStart(t *testing.T) {
	spec := testPlugin(t, "timed", &pub.Plugin{
		ChatParams: func(context.Context, *pub.Client, *pub.ChatParamsRequest) (*pub.ChatParamsResponse, error) {
			return nil, nil
		},
		HookTimeouts: map[pub.Hook]time.Duration{pub.HookChatParams: 9 * time.Second},
	})
	spec.Manifest.HookTimeouts = map[pub.Hook]int{pub.HookChatParams: 1000}
	var errs []error
	h := newTestHost(t, Options{OnError: func(_ string, _ pub.Hook, err error) { errs = append(errs, err) }}, spec)
	h.ChatParams(context.Background(), &pub.ChatParamsRequest{SessionID: "s1"})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "disagrees with cache in hook_timeouts") {
		t.Errorf("errors = %v, want a mismatch in hook_timeouts", errs)
	}
}

func TestCanceledFirstStartIsNotLatched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spec := testPlugin(t, "guard", &pub.Plugin{
			ToolExecuteBefore: &pub.ToolBefore{DenyOnFailure: true, Run: func(context.Context, *pub.Client, *pub.ToolExecuteBeforeRequest) (*pub.ToolExecuteBeforeResponse, error) {
				return &pub.ToolExecuteBeforeResponse{Deny: "hook ran"}, nil
			}},
		})
		serve := spec.dial
		dials := 0
		dialing := make(chan struct{})
		spec.dial = func() (io.ReadWriteCloser, error) {
			dials++
			if dials == 1 {
				hostSide, pluginSide := net.Pipe()
				go io.Copy(io.Discard, pluginSide) //nolint:errcheck
				close(dialing)
				return hostSide, nil
			}
			return serve()
		}
		h := newTestHost(t, Options{HookTimeout: time.Hour}, spec)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan string)
		go func() {
			_, deny := h.ToolExecuteBefore(ctx, &pub.ToolExecuteBeforeRequest{SessionID: "s1", CallID: "c1", Tool: "bash", Args: json.RawMessage(`{}`)})
			done <- deny
		}()
		<-dialing
		cancel()
		if deny := <-done; deny != "" {
			t.Fatalf("deny = %q after a canceled call, want none", deny)
		}
		_, deny := h.ToolExecuteBefore(context.Background(), &pub.ToolExecuteBeforeRequest{SessionID: "s1", CallID: "c2", Tool: "bash", Args: json.RawMessage(`{}`)})
		if deny != "hook ran" {
			t.Errorf("deny = %q, want the hook to run after the canceled first start", deny)
		}
	})
}

func TestNilChatParamsAndMessageResponsesChangeNothing(t *testing.T) {
	temp := 0.2
	first := testPlugin(t, "first", &pub.Plugin{
		ChatParams: func(_ context.Context, _ *pub.Client, req *pub.ChatParamsRequest) (*pub.ChatParamsResponse, error) {
			params := req.Params
			params.Temperature = &temp
			return &pub.ChatParamsResponse{Params: params}, nil
		},
		ChatMessage: func(_ context.Context, _ *pub.Client, req *pub.ChatMessageRequest) (*pub.ChatMessageResponse, error) {
			msg := req.Message
			msg.Parts = []pub.Part{pub.TextPart("changed")}
			return &pub.ChatMessageResponse{Message: msg}, nil
		},
	})
	second := testPlugin(t, "second", &pub.Plugin{
		ChatParams: func(context.Context, *pub.Client, *pub.ChatParamsRequest) (*pub.ChatParamsResponse, error) {
			return nil, nil
		},
		ChatMessage: func(context.Context, *pub.Client, *pub.ChatMessageRequest) (*pub.ChatMessageResponse, error) {
			return nil, nil
		},
	})
	h := newTestHost(t, Options{}, first, second)
	got := h.ChatParams(context.Background(), &pub.ChatParamsRequest{SessionID: "s1"})
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("temperature = %v, want 0.2 kept after a nil response", got.Temperature)
	}
	msg := h.ChatMessage(context.Background(), &pub.ChatMessageRequest{SessionID: "s1", Message: pub.Message{Role: "user"}})
	if len(msg.Parts) != 1 || msg.Parts[0].Text != "changed" {
		t.Errorf("message parts = %+v, want the first plugin's change kept after a nil response", msg.Parts)
	}
}
