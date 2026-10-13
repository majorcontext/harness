package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

var bg = context.Background()

func closeRuntime(t *testing.T, r *harness.Runtime) {
	t.Helper()
	if err := r.Close(bg); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func text(id, s string) protocol.Input {
	return protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: s}}}
}

func converse(t *testing.T, sess *harness.Session, txt string) {
	t.Helper()
	turn, err := sess.Prompt(bg, text("a", txt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(bg); err != nil {
		t.Fatal(err)
	}
}

func pluginFixture(t *testing.T, cfg string) []config.PluginSpec {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pluginfixture")
	if out, err := exec.Command("go", "build", "-o", bin, "../harnesstest/pluginfixture").CombinedOutput(); err != nil {
		t.Fatalf("go build pluginfixture: %v\n%s", err, out)
	}
	return []config.PluginSpec{{Name: "fixture", Command: []string{bin}, Config: []byte(cfg)}}
}

const forcePush = "git push --force origin main"

func bashStep(command string) harnesstest.Step {
	return harnesstest.Step{Name: "bash", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{
		ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": command}}}}}
}

func doneStep() harnesstest.Step {
	return harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "done"}}
}

func forceGuard(deny bool, hook func()) *plugin.Plugin {
	return &plugin.Plugin{Name: "guard", ToolExecuteBefore: &plugin.ToolBefore{DenyOnFailure: deny,
		Run: func(_ context.Context, _ *plugin.Client, req *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
			if hook != nil {
				hook()
			}
			if strings.Contains(string(req.Args), "--force") {
				return &plugin.ToolExecuteBeforeResponse{Deny: "force push is blocked"}, nil
			}
			return nil, nil
		}}}
}

func inprocRuntime(t *testing.T, dir string, s *harnesstest.Server, cfgPlugins []config.PluginSpec, plugins ...*plugin.Plugin) *harness.Runtime {
	t.Helper()
	return inprocRuntimeLog(t, dir, s, nil, cfgPlugins, plugins...)
}

func inprocRuntimeLog(t *testing.T, dir string, s *harnesstest.Server, logger *slog.Logger, cfgPlugins []config.PluginSpec, plugins ...*plugin.Plugin) *harness.Runtime {
	t.Helper()
	t.Setenv("HARNESS_TEST_KEY", "k")
	zero := 0
	r, err := harness.New(harness.Options{Getenv: os.Getenv, Store: store.NewMemStore(), WorkDir: dir, Plugins: plugins, Logger: logger,
		ModelTransport: func(string) http.RoundTripper { return &http.Transport{DisableKeepAlives: true} },
		Config: config.Config{RequestMaxRetries: &zero, StreamMaxRetries: &zero, Plugins: cfgPlugins, Providers: map[string]config.Provider{
			"anthropic": {APIKeyEnv: "HARNESS_TEST_KEY", BaseURL: s.URL()}}}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func lastResult(t *testing.T, s *harnesstest.Server) string {
	t.Helper()
	reqs := s.Requests()
	for _, m := range slices.Backward(reqs[len(reqs)-1].Messages) {
		for _, p := range m.Parts {
			if p.Kind == "tool_result" {
				return strings.TrimPrefix(p.Text, "[tool error] ")
			}
		}
	}
	t.Fatal("the last request has no tool result")
	return ""
}

func runTurn(t *testing.T, r *harness.Runtime) {
	t.Helper()
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "anthropic/claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	converse(t, sess, "run")
}

func TestInProcessGuardDeniesAForcePush(t *testing.T) {
	s := harnesstest.NewAnthropic(t, bashStep(forcePush), doneStep())
	r := inprocRuntime(t, t.TempDir(), s, nil, forceGuard(false, nil))
	defer closeRuntime(t, r)
	runTurn(t, r)
	if got := lastResult(t, s); got != "force push is blocked" {
		t.Errorf("tool result = %q, want the deny text", got)
	}
}

func TestInProcessFlaggedHookDeniesOnPanic(t *testing.T) {
	s := harnesstest.NewAnthropic(t, bashStep("echo hi"), doneStep())
	r := inprocRuntime(t, t.TempDir(), s, nil, forceGuard(true, func() { panic("boom") }))
	defer closeRuntime(t, r)
	runTurn(t, r)
	if got := lastResult(t, s); !strings.HasPrefix(got, "denied: plugin guard failed: ") || !strings.Contains(got, "panic: boom") {
		t.Errorf("tool result = %q, want a denial that names the panic", got)
	}
}

func TestFailedUnflaggedHookIsLogged(t *testing.T) {
	s := harnesstest.NewAnthropic(t, bashStep("echo hi"), doneStep())
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	r := inprocRuntimeLog(t, t.TempDir(), s, logger, nil, forceGuard(false, func() { panic("boom api_key=abcdefghijklmnop") }))
	defer closeRuntime(t, r)
	runTurn(t, r)
	got := buf.String()
	for _, want := range []string{"level=WARN", "plugin: hook failed", "plugin=guard", "hook=tool.execute.before", "panic: boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("log = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "abcdefghijklmnop") {
		t.Errorf("log = %q, want the secret masked", got)
	}
}

func TestInterruptedHookIsNotLogged(t *testing.T) {
	s := harnesstest.NewAnthropic(t, bashStep("echo hi"))
	var buf syncBuffer
	arrived, release := make(chan struct{}), make(chan struct{})
	r := inprocRuntimeLog(t, t.TempDir(), s, slog.New(slog.NewTextHandler(&buf, nil)), nil, forceGuard(false, func() {
		close(arrived)
		<-release
	}))
	defer closeRuntime(t, r)
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "anthropic/claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := sess.Prompt(bg, text("a", "run"))
	if err != nil {
		t.Fatal(err)
	}
	<-arrived
	if err := sess.Interrupt(bg, protocol.Interrupt{}); err != nil {
		t.Fatal(err)
	}
	close(release)
	_, _ = turn.Wait(bg)
	if strings.Contains(buf.String(), "plugin: hook failed") {
		t.Errorf("log = %q, want no hook failure line for an interrupt", buf.String())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestInProcessFlaggedHookDeniesAtTheDeadline(t *testing.T) {
	s := harnesstest.NewAnthropic(t, bashStep("echo hi"), doneStep())
	synctest.Test(t, func(t *testing.T) {
		never := make(chan struct{})
		defer close(never)
		r := inprocRuntime(t, t.TempDir(), s, nil, forceGuard(true, func() { <-never }))
		sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "anthropic/claude-opus-5"})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		converse(t, sess, "run")
		if d := time.Since(start); d < 5*time.Second || d > 6*time.Second {
			t.Errorf("the call ended after %v, want the 5 s deadline", d)
		}
		if got := lastResult(t, s); !strings.HasPrefix(got, "denied: plugin guard failed: ") {
			t.Errorf("tool result = %q, want a denial", got)
		}
		ctx, cancel := context.WithTimeout(bg, time.Second)
		defer cancel()
		start = time.Now()
		if err := r.Close(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Close = %v", err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("Close took %v, want a return by its context", time.Since(start))
		}
	})
}

func TestInProcessPluginRunsAfterConfigPlugins(t *testing.T) {
	cfg := pluginFixture(t, `{"rewrite":{"from":"echo push","to":"git push --force"}}`)
	s := harnesstest.NewAnthropic(t, bashStep("echo push"), doneStep())
	r := inprocRuntime(t, t.TempDir(), s, cfg, forceGuard(false, nil))
	defer closeRuntime(t, r)
	runTurn(t, r)
	if got := lastResult(t, s); got != "force push is blocked" {
		t.Errorf("tool result = %q, want the deny text of the guard", got)
	}
}

func TestOnePluginValueServesTwoRuntimes(t *testing.T) {
	var mu sync.Mutex
	dirs := map[string]string{}
	p := &plugin.Plugin{Name: "count", ToolExecuteBefore: &plugin.ToolBefore{
		Run: func(_ context.Context, c *plugin.Client, req *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			dirs[req.SessionID] += c.WorkspaceDir() + ";"
			return nil, nil
		}}}
	want := map[string]string{"a": t.TempDir(), "b": t.TempDir()}
	var wg sync.WaitGroup
	errs := make(chan error, len(want))
	for id, dir := range want {
		s := harnesstest.NewAnthropic(t, bashStep("echo hi"), doneStep())
		r := inprocRuntime(t, dir, s, nil, p)
		defer closeRuntime(t, r)
		sess, err := r.Create(bg, protocol.CreateSession{ID: id, Model: "anthropic/claude-opus-5"})
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			turn, err := sess.Prompt(bg, text("a", "run"))
			if err == nil {
				_, err = turn.Wait(bg)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for id, dir := range want {
		if dirs[id] != dir+";" {
			t.Errorf("session %s hook saw workspaces %q, want its own %q once", id, dirs[id], dir)
		}
	}
}

func TestRuntimeCloseWaitsForAnInProcessHook(t *testing.T) {
	s := harnesstest.NewAnthropic(t, bashStep("echo hi"), doneStep())
	synctest.Test(t, func(t *testing.T) {
		var started, exited atomic.Bool
		release := make(chan struct{})
		p := &plugin.Plugin{Name: "slow", ToolExecuteBefore: &plugin.ToolBefore{
			Run: func(context.Context, *plugin.Client, *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
				started.Store(true)
				<-release
				exited.Store(true)
				return nil, nil
			}}}
		r := inprocRuntime(t, t.TempDir(), s, nil, p)
		runTurn(t, r)
		if !started.Load() || exited.Load() {
			t.Fatalf("started = %v, exited = %v, want the hook running after the turn", started.Load(), exited.Load())
		}
		closed := make(chan struct{})
		go func() {
			closeRuntime(t, r)
			close(closed)
		}()
		synctest.Wait()
		select {
		case <-closed:
			t.Error("Close returned while the hook was still running")
		default:
		}
		close(release)
		<-closed
		if !exited.Load() {
			t.Error("Close returned before the hook exited")
		}
	})
}

type bigTool struct{}

func (bigTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "big", Description: "big", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (bigTool) Run(context.Context, harness.Call) (protocol.ToolResult, error) {
	return protocol.ToolResult{Text: strings.Repeat("x", 2<<20)}, nil
}

func TestInProcessAfterHookGetsLargeOutput(t *testing.T) {
	got := make(chan int, 1)
	p := &plugin.Plugin{Name: "tap", ToolExecuteAfter: func(_ context.Context, _ *plugin.Client, req *plugin.ToolExecuteAfterRequest) (*plugin.ToolExecuteAfterResponse, error) {
		got <- len(req.Output[0].Text)
		return nil, nil
	}}
	s := harnesstest.NewAnthropic(t, harnesstest.Step{Name: "big", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{
		ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "big", Input: map[string]any{}}}}},
		harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("big"), Reply: harnesstest.Reply{Text: "done"}})
	t.Setenv("HARNESS_TEST_KEY", "k")
	r, err := harness.New(harness.Options{Getenv: os.Getenv, Store: store.NewMemStore(), Plugins: []*plugin.Plugin{p}, Tools: []harness.Tool{bigTool{}},
		Config: config.Config{Providers: map[string]config.Provider{"anthropic": {APIKeyEnv: "HARNESS_TEST_KEY", BaseURL: s.URL()}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime(t, r)
	runTurn(t, r)
	if n := <-got; n != 2<<20 {
		t.Errorf("the after hook got %d bytes, want %d", n, 2<<20)
	}
}

func TestNewRejectsAPluginToolCollision(t *testing.T) {
	tool := func(name string) plugin.Tool {
		return plugin.Tool{Def: plugin.ToolDef{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			Execute: func(context.Context, *plugin.Client, json.RawMessage) ([]plugin.Part, error) { return nil, nil }}
	}
	for _, tc := range []struct {
		name    string
		workDir string
		tools   []harness.Tool
		plugins []*plugin.Plugin
		want    string
	}{
		{"a built-in tool", t.TempDir(), nil, []*plugin.Plugin{{Name: "p", Tools: []plugin.Tool{tool("bash")}}}, `harness: plugin p: tool name "bash" is empty or taken`},
		{"an Options.Tools tool", "", []harness.Tool{bigTool{}}, []*plugin.Plugin{{Name: "p", Tools: []plugin.Tool{tool("big")}}}, `harness: plugin p: tool name "big" is empty or taken`},
		{"another in-process tool", "", nil, []*plugin.Plugin{{Name: "p", Tools: []plugin.Tool{tool("x")}}, {Name: "q", Tools: []plugin.Tool{tool("x")}}}, `harness: plugin q: tool name "x" is empty or taken`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := harness.New(harness.Options{Store: store.NewMemStore(), WorkDir: tc.workDir, Tools: tc.tools, Plugins: tc.plugins})
			if err == nil || err.Error() != tc.want {
				t.Errorf("New = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNewRejectsAnInvalidPlugin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     []config.PluginSpec
		plugins []*plugin.Plugin
		want    string
	}{
		{"a nil plugin", nil, []*plugin.Plugin{nil}, "harness: plugin 0 is nil"},
		{"a plugin with no name", nil, []*plugin.Plugin{{Name: "a"}, {}}, "harness: plugin 1 has no name"},
		{"two plugins of the same name", nil, []*plugin.Plugin{{Name: "a"}, {Name: "a"}}, `harness: plugin name "a" is repeated`},
		{"a config plugin of the same name", []config.PluginSpec{{Name: "a", Command: []string{"a"}}}, []*plugin.Plugin{{Name: "a"}}, `harness: plugin name "a" is repeated`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := harness.New(harness.Options{Store: store.NewMemStore(), Config: config.Config{Plugins: tc.cfg}, Plugins: tc.plugins})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("New = %v, want an error that contains %q", err, tc.want)
			}
		})
	}
}

func TestCreateRejectsAnInProcessToolThatTakesAConfigPluginToolName(t *testing.T) {
	p := &plugin.Plugin{Name: "p", Tools: []plugin.Tool{{Def: plugin.ToolDef{Name: "fixture_echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, *plugin.Client, json.RawMessage) ([]plugin.Part, error) { return nil, nil }}}}
	s := harnesstest.NewAnthropic(t)
	r := inprocRuntime(t, t.TempDir(), s, pluginFixture(t, `{}`), p)
	defer closeRuntime(t, r)
	_, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "anthropic/claude-opus-5"})
	if err == nil || !strings.Contains(err.Error(), `plugin p: tool name "fixture_echo" is empty or taken`) {
		t.Errorf("Create = %v, want a tool name clash", err)
	}
}

func TestRuntimeCloseWaitsForAnInProcessEventHook(t *testing.T) {
	s := harnesstest.NewAnthropic(t, bashStep("echo hi"), doneStep())
	synctest.Test(t, func(t *testing.T) {
		var started, exited atomic.Bool
		p := &plugin.Plugin{Name: "events", Event: func(ctx context.Context, _ *plugin.Client, _ []plugin.Event) {
			started.Store(true)
			<-ctx.Done()
			<-time.NewTimer(time.Second).C
			exited.Store(true)
		}}
		r := inprocRuntime(t, t.TempDir(), s, nil, p)
		runTurn(t, r)
		if !started.Load() || exited.Load() {
			t.Fatalf("started = %v, exited = %v, want the event hook running after the turn", started.Load(), exited.Load())
		}
		closeRuntime(t, r)
		if !exited.Load() {
			t.Error("Close returned while the event hook was still running")
		}
	})
}

func TestNewRejectsAPluginThatCannotRun(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	for _, tc := range []struct {
		name string
		p    *plugin.Plugin
		want string
	}{
		{"a flagged guard with no Run", &plugin.Plugin{Name: "g", ToolExecuteBefore: &plugin.ToolBefore{DenyOnFailure: true}}, "harness: plugin g: ToolExecuteBefore has no Run"},
		{"a tool with no Execute", &plugin.Plugin{Name: "g", Tools: []plugin.Tool{{Def: plugin.ToolDef{Name: "x", InputSchema: schema}}}}, `harness: plugin g: tool "x" has no Execute`},
		{"a hook timeout over the limit", &plugin.Plugin{Name: "g", ToolExecuteBefore: &plugin.ToolBefore{Run: func(context.Context, *plugin.Client, *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
			return nil, nil
		}}, HookTimeouts: map[plugin.Hook]time.Duration{plugin.HookToolExecuteBefore: 601 * time.Second}}, "harness: plugin g: hook_timeouts for tool.execute.before must be from 1 to 600000 ms (HookTimeouts[tool.execute.before] is 10m1s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := harness.New(harness.Options{Store: store.NewMemStore(), Plugins: []*plugin.Plugin{tc.p}})
			if err == nil || err.Error() != tc.want {
				t.Errorf("New = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCloseCancelsAGenerateCallInFlight(t *testing.T) {
	gen := harnesstest.Step{Name: "generate", Match: func(r harnesstest.Request) bool { return r.System == "GEN" },
		Reply: harnesstest.Reply{Text: "never read", Block: true}}
	call := harnesstest.Step{Name: "call", Match: func(r harnesstest.Request) bool { return r.System != "GEN" },
		Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "gen", Input: map[string]any{}}}}}
	s := harnesstest.NewAnthropic(t, gen, call)
	p := &plugin.Plugin{Name: "gen", Tools: []plugin.Tool{{
		Def: plugin.ToolDef{Name: "gen", Description: "generate", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Execute: func(ctx context.Context, c *plugin.Client, _ json.RawMessage) ([]plugin.Part, error) {
			_, err := c.Generate(ctx, &plugin.GenerateRequest{Model: "anthropic/claude-opus-5", System: "GEN",
				Messages: []plugin.Message{{Role: "user", Parts: []plugin.Part{plugin.TextPart("hi")}}}})
			return []plugin.Part{plugin.TextPart("generate: " + fmt.Sprint(err))}, nil
		}}}}
	r := inprocRuntime(t, t.TempDir(), s, nil, p)
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "anthropic/claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Prompt(bg, text("a", "go")); err != nil {
		t.Fatal(err)
	}
	if !s.AwaitBlocked("generate", time.Minute) {
		t.Fatal("the generate request never reached the model")
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.AwaitCanceled("generate", time.Minute) {
		t.Error("Close returned and the model request was not canceled")
	}
}
