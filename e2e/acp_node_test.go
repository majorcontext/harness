package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

// nodeHostName names the Node wasm host in the subtests of the ACP table.
const nodeHostName = "node-wasm"

// nodeEnv is set by the Node job of CI. With it set, a missing Node fails a
// test; without it the Node host is not part of the table, because it builds
// the module.
const nodeEnv = "HARNESS_E2E_NODE"

var nodeModule struct {
	once            sync.Once
	dir, wasm, exec string
	err             error
}

// buildNodeModule builds cmd/harness-wasm once and finds the wasm_exec.js of
// the same Go version.
func buildNodeModule() (wasm, execJS string, err error) {
	nodeModule.once.Do(func() {
		wd, err := os.Getwd()
		if err != nil {
			nodeModule.err = err
			return
		}
		root := filepath.Dir(wd)
		if nodeModule.dir, err = os.MkdirTemp("", "harness-e2e-wasm"); err != nil {
			nodeModule.err = err
			return
		}
		nodeModule.wasm = filepath.Join(nodeModule.dir, "harness.wasm")
		build := exec.Command("go", "build", "-o", nodeModule.wasm, "./cmd/harness-wasm")
		build.Dir = root
		build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0")
		if msg, err := build.CombinedOutput(); err != nil {
			nodeModule.err = &buildError{err: err, out: string(msg)}
			return
		}
		goroot, err := exec.Command("go", "env", "GOROOT").Output()
		if err != nil {
			nodeModule.err = err
			return
		}
		nodeModule.exec = filepath.Join(strings.TrimSpace(string(goroot)), "lib", "wasm", "wasm_exec.js")
	})
	return nodeModule.wasm, nodeModule.exec, nodeModule.err
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string {
	return "building cmd/harness-wasm: " + e.err.Error() + "\n" + e.out
}

// cleanupNodeModule removes the module that buildNodeModule built.
func cleanupNodeModule() {
	if nodeModule.dir != "" {
		_ = os.RemoveAll(nodeModule.dir)
	}
}

// nodeHostOptions is the options file of cmd/harness-wasm/node/host.mjs.
type nodeHostOptions struct {
	ConfigPath   string            `json:"configPath"`
	StoreDir     string            `json:"storeDir"`
	Env          map[string]string `json:"env"`
	BuiltinTools []string          `json:"builtinTools,omitempty"`
	Online       *bool             `json:"online,omitempty"`
	EchoTool     bool              `json:"echoTool,omitempty"`
	JSTools      []string          `json:"jsTools,omitempty"`
	Connect      map[string]any    `json:"connect,omitempty"`
	CyclicConfig bool              `json:"cyclicConfig,omitempty"`
}

// startACPNode runs the wasm module under Node, with the config, the store
// directory, and the default tool set of env.
func startACPNode(t *testing.T, env acpEnv) (io.WriteCloser, io.Reader, func()) {
	t.Helper()
	return startACPNodeOpts(t, nodeHostOptions{ConfigPath: env.configPath, StoreDir: env.sessDir,
		Env: map[string]string{"ANTHROPIC_API_KEY": acpDummyKey}, BuiltinTools: env.allowed, EchoTool: env.jsTool})
}

// nodePath returns the node binary. Without HARNESS_E2E_NODE=1 the test skips,
// because the Node host builds the module; with it, a missing node fails.
func nodePath(t *testing.T) string {
	t.Helper()
	if os.Getenv(nodeEnv) != "1" {
		t.Skip(nodeEnv + "=1 enables the Node wasm host")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("the Node wasm host needs node: %v", err)
	}
	return node
}

func startACPNodeOpts(t *testing.T, opts nodeHostOptions) (io.WriteCloser, io.Reader, func()) {
	t.Helper()
	node := nodePath(t)
	wasm, execJS, err := buildNodeModule()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	optsPath := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(optsPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := filepath.Abs(filepath.Join("..", "cmd", "harness-wasm", "node", "host.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, host, wasm, execJS, optsPath)
	cmd.Dir = t.TempDir()
	in, out, g, stderr := startACPCmd(t, cmd)
	dump := func() string {
		_ = syscall.Kill(g.cmd.Process.Pid, syscall.SIGUSR2)
		state := "the host answered the state request"
		if !testpoll.UntilNoT(3*time.Second, func() bool { return strings.Contains(stderr.String(), "active resources:") }, 50*time.Millisecond) {
			state = "the host did not answer the state request; its event loop is blocked"
		}
		report := state + "\nnode stderr:\n" + stderr.String()
		g.kill()
		return report
	}
	stallDumps.Store(t, dump)
	callback := make(chan struct{})
	watchdog := time.AfterFunc(rowBound, func() {
		defer close(callback)
		t.Errorf("the Node host ran longer than %s\n%s", rowBound, dump())
	})
	// endWatch ends the limit of this host before another host of t starts.
	// Stop does not wait for a callback that runs, so the wait does.
	var endOnce sync.Once
	endWatch := func() {
		endOnce.Do(func() {
			if !watchdog.Stop() {
				<-callback
			}
			stallDumps.Delete(t)
		})
	}
	t.Cleanup(func() {
		endWatch()
		if t.Failed() {
			t.Logf("node stderr:\n%s", stderr.String())
		}
	})
	return in, out, func() { t.Helper(); endWatch(); waitACPExit(t, g, stderr) }
}

const (
	nodeMCPOff = `"mcpCapabilities":{"http":false`
	nodeMCPOn  = `"mcpCapabilities":{"http":true`
	// nodeAuthMethods is the authMethods of the native hosts, for the wasm host, which offers no login.
	nodeAuthMethods = `"authMethods":[{"description":"Open your browser and sign in with your ChatGPT subscription.","id":"chatgpt","name":"Log in with ChatGPT"}]`
)

// alignNodeGolden rewrites the two texts in which the wasm host differs from
// the native hosts, so the rest of its golden compares with the shared one. The
// wasm host starts no MCP server and advertises none; the check fails when an
// initialize answer advertises MCP. The js file system writes the ENOENT text
// with a capital letter.
func alignNodeGolden(t *testing.T, g acpGolden) acpGolden {
	t.Helper()
	for i, f := range g.Wire {
		text := string(f.Frame)
		if f.Dir == "agent" && strings.Contains(text, `"authMethods":[]`) {
			text = strings.Replace(strings.Replace(text, `"authMethods":[]`, nodeAuthMethods, 1), `"agentCapabilities":{`, `"agentCapabilities":{"auth":{"logout":{}},`, 1)
			g.Wire[i].Frame = json.RawMessage(text)
		}
		if f.Dir != "agent" || !strings.Contains(text, `"mcpCapabilities"`) {
			continue
		}
		if !strings.Contains(text, nodeMCPOff) {
			t.Errorf("the wasm host advertises MCP: %s", text)
			continue
		}
		g.Wire[i].Frame = json.RawMessage(strings.Replace(text, nodeMCPOff, nodeMCPOn, 1))
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var out acpGolden
	if err := json.Unmarshal([]byte(strings.ReplaceAll(strings.ReplaceAll(string(b), "No such file or directory", "no such file or directory"), "the config of harnessConnect", "\u003cuser-config\u003e")), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNodeWasmFetchFailureIsPermanentWhileOnline pins the sentence of the
// WebAssembly section on a fetch that gets no response: while the host reports
// online, the turn ends permanent after one attempt, and the error names the
// JavaScript error, not an object.
func TestNodeWasmFetchFailureIsPermanentWhileOnline(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	online := true
	closed := freeAddr(t)
	env := acpEnv{configPath: writeGoalConfigWith(t, "http://"+closed, nil), sessDir: t.TempDir()}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{ConfigPath: env.configPath, StoreDir: env.sessDir,
		Env: map[string]string{"ANTHROPIC_API_KEY": acpDummyKey}, Online: &online})
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.env = env
	c.dirs["main"] = t.TempDir()
	acpNew{as: "a"}.run(t, c)
	acpPromptStart{as: "a", text: "go"}.run(t, c)
	f := c.response(c.pending["a"])
	c.mustBeError(f, acpInternal)
	e, _ := f["error"].(map[string]any)
	data, _ := e["data"].(map[string]any)
	if data["error_class"] != "permanent" {
		t.Errorf("error data = %v, want error_class permanent", data)
	}
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "TypeError") || strings.Contains(msg, "<object>") || strings.Contains(msg, "[object") {
		t.Errorf("error message = %q, want the name and message of the JavaScript error and no object text", msg)
	}
	c.finish(stop)
}

// TestNodeWasmRunsAJavaScriptTool pins the sentence of the WebAssembly section
// on tools: a tool that the host passes to harnessConnect reaches the model and
// its result returns to the model.
func TestNodeWasmRunsAJavaScriptTool(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	fake := harnesstest.NewAnthropic(t,
		harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "js_echo", Input: map[string]any{"text": "hi"}}}}},
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
	env := acpEnv{configPath: writeGoalConfigWith(t, fake.URL(), nil), sessDir: t.TempDir()}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{ConfigPath: env.configPath, StoreDir: env.sessDir,
		Env: map[string]string{"ANTHROPIC_API_KEY": acpDummyKey}, EchoTool: true})
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.fake, c.env = fake, env
	c.dirs["main"] = t.TempDir()
	acpNew{as: "a"}.run(t, c)
	acpPrompt{as: "a", text: "go", stop: "end_turn"}.run(t, c)
	reqs := acpRequests(t, c, 2)
	if !requestHas(reqs[0], "js_echo") {
		t.Errorf("tools of the first request = %v, want js_echo", reqs[0].Tools)
	}
	if got := requestText(reqs[1]); !strings.Contains(got, "echo:hi") {
		t.Errorf("the second request does not hold the tool result: %s", got)
	}
	c.finish(stop)
}

// TestNodeWasmPageToolReplacesABuiltinTool pins the sentence of the tools
// section on a host tool with the name of a built-in tool: with an allowed list,
// the host tool replaces the built-in, and the model sees one tool of that name.
func TestNodeWasmPageToolReplacesABuiltinTool(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	fake := harnesstest.NewAnthropic(t,
		harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "bash", Input: map[string]any{"command": "hi"}}}}},
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
	env := acpEnv{configPath: writeGoalConfigWith(t, fake.URL(), nil), sessDir: t.TempDir()}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{BuiltinTools: []string{"read_file", "bash"}, JSTools: []string{"bash"}}.withBase(env))
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.fake, c.env = fake, env
	c.dirs["main"] = t.TempDir()
	acpNew{as: "a"}.run(t, c)
	acpPrompt{as: "a", text: "go", stop: "end_turn"}.run(t, c)
	reqs := acpRequests(t, c, 2)
	if n := strings.Count(strings.Join(reqs[0].Tools, " ")+" ", "bash "); n != 1 {
		t.Errorf("tools of the first request = %v, want one bash", reqs[0].Tools)
	}
	if got := requestText(reqs[1]); !strings.Contains(got, "host:hi") {
		t.Errorf("the second request does not hold the result of the page tool: %s", got)
	}
	c.finish(stop)
}

// TestNodeWasmPageToolWithABuiltinNameFailsWhenEveryToolIsAllowed pins the
// same sentence: with no allowed list, the built-in tool is allowed, and the
// name is taken.
func TestNodeWasmPageToolWithABuiltinNameFailsWhenEveryToolIsAllowed(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	fake := harnesstest.NewAnthropic(t)
	env := acpEnv{configPath: writeGoalConfigWith(t, fake.URL(), nil), sessDir: t.TempDir()}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{JSTools: []string{"bash"}}.withBase(env))
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.fake, c.env = fake, env
	c.dirs["main"] = t.TempDir()
	acpNew{as: "a", code: acpInvalidParams}.run(t, c)
	c.finish(stop)
}

func (o nodeHostOptions) withBase(env acpEnv) nodeHostOptions {
	o.ConfigPath, o.StoreDir = env.configPath, env.sessDir
	o.Env = map[string]string{"ANTHROPIC_API_KEY": acpDummyKey}
	return o
}

// TestNodeWasmBadOptionFailsTheFirstSession pins the sentence of the
// WebAssembly section on options: an option that is present and has the wrong
// type fails the first session/new as an invalid request.
func TestNodeWasmBadOptionFailsTheFirstSession(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	rows := map[string]nodeHostOptions{
		"primitive config":                 {Connect: map[string]any{"config": 5}},
		"cyclic config":                    {CyclicConfig: true},
		"array config":                     {Connect: map[string]any{"config": []any{}}},
		"env with an array":                {Connect: map[string]any{"env": []any{1}}},
		"env with a number":                {Connect: map[string]any{"env": map[string]any{"K": 1}}},
		"proxy that is a number":           {Connect: map[string]any{"proxy": 5}},
		"proxy header that is a number":    {Connect: map[string]any{"proxy": map[string]any{"headers": map[string]any{"h": 1}}}},
		"builtinTools with a number":       {Connect: map[string]any{"builtinTools": []any{1}}},
		"builtinTools that is a string":    {Connect: map[string]any{"builtinTools": "read_file"}},
		"array-like tools":                 {Connect: map[string]any{"tools": map[string]any{"length": 1, "0": map[string]any{"name": "x"}}}},
		"store directory that is a number": {Connect: map[string]any{"storeDir": 3}},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := harnesstest.NewAnthropic(t)
			env := acpEnv{configPath: writeGoalConfigWith(t, fake.URL(), nil), sessDir: t.TempDir()}
			in, out, stop := startACPNodeOpts(t, row.withBase(env))
			c := newACPConn(t, loadACPSchema(t), in, out)
			c.fake, c.env = fake, env
			c.dirs["main"] = t.TempDir()
			acpNew{as: "a", code: acpInvalidParams}.run(t, c)
			c.finish(stop)
		})
	}
}

// TestNodeWasmJSToolFailuresAreReadable pins the sentence of the WebAssembly
// section on tools: a JavaScript value that cannot become JSON, and a rejection
// that is not an Error, give an error result with readable text and no object marker.
func TestNodeWasmJSToolFailuresAreReadable(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	fake := harnesstest.NewAnthropic(t,
		harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			{ID: "c1", Name: "js_cyclic", Input: map[string]any{}},
			{ID: "c2", Name: "js_reject", Input: map[string]any{}}}}},
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
	env := acpEnv{configPath: writeGoalConfigWith(t, fake.URL(), nil), sessDir: t.TempDir()}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{JSTools: []string{"js_cyclic", "js_reject"}}.withBase(env))
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.fake, c.env = fake, env
	c.dirs["main"] = t.TempDir()
	acpNew{as: "a"}.run(t, c)
	acpPrompt{as: "a", text: "go", stop: "end_turn"}.run(t, c)
	got := requestText(acpRequests(t, c, 2)[1])
	for _, want := range []string{"circular", "non-Error rejection of type object"} {
		if !strings.Contains(got, want) {
			t.Errorf("the second request lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "[object") || strings.Contains(got, "<object>") {
		t.Errorf("the second request holds an object marker: %s", got)
	}
	c.finish(stop)
}

// TestNodeStoreLockTakeoverHasOneWinner starts several processes that find the
// same stale lock at the same moment. Exactly one gets the lock, the others see
// it in use, and its release leaves no lock file.
func TestNodeStoreLockTakeoverHasOneWinner(t *testing.T) {
	skipShort(t)
	node := nodePath(t)
	t.Parallel()
	lock, err := filepath.Abs(filepath.Join("..", "sdk", "embed", "src", "storelock.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".embed.lock"), []byte("999999999\nstale"), 0o600); err != nil {
		t.Fatal(err)
	}
	prog := fmt.Sprintf(`
import { acquireStoreLock } from %q;
const start = Number(process.argv[2]);
while (Date.now() < start) {}
try {
  const release = acquireStoreLock(%q);
  console.log("won");
  await new Promise((r) => setTimeout(r, 1500));
  release();
} catch (err) {
  console.log("busy");
}
`, "file://"+lock, dir)
	script2 := filepath.Join(t.TempDir(), "takeover.mjs")
	if err := os.WriteFile(script2, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	start := fmt.Sprint(time.Now().Add(1500 * time.Millisecond).UnixMilli())
	const procs = 8
	results := make([]string, procs)
	var wg sync.WaitGroup
	for i := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			cmd := exec.Command(node, script2, start)
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			if err := cmd.Run(); err != nil {
				buf.WriteString(err.Error())
			}
			results[i] = strings.TrimSpace(buf.String())
		}()
	}
	wg.Wait()
	won := 0
	for _, r := range results {
		switch r {
		case "won":
			won++
		case "busy":
		default:
			t.Errorf("a process printed %q", r)
		}
	}
	if won != 1 {
		t.Errorf("%d processes won the lock, want 1 (%v)", won, results)
	}
	if _, err := os.Stat(filepath.Join(dir, ".embed.lock")); !os.IsNotExist(err) {
		t.Errorf("the lock file stays after the release: %v", err)
	}
}

// TestNodeWasmProxyBaseURLRoutesModelTraffic pins the sentence of the
// WebAssembly section on proxy.baseURL: with no provider in the config, the
// model request goes to the proxy base.
func TestNodeWasmProxyBaseURLRoutesModelTraffic(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	fake := harnesstest.NewAnthropic(t, harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"model":"anthropic/claude-fable-5"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := acpEnv{configPath: cfgPath, sessDir: t.TempDir()}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{ConfigPath: cfgPath, StoreDir: env.sessDir,
		Env:     map[string]string{"ANTHROPIC_API_KEY": acpDummyKey},
		Connect: map[string]any{"proxy": map[string]any{"baseURL": fake.URL()}}})
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.fake, c.env = fake, env
	c.dirs["main"] = t.TempDir()
	acpNew{as: "a"}.run(t, c)
	acpPrompt{as: "a", text: "go", stop: "end_turn"}.run(t, c)
	acpRequests(t, c, 1)
	c.finish(stop)
}

// TestNodeWasmProxyBaseURLFillsAnOpenAIProviderWithNoBaseURL pins the sentence
// of the WebAssembly section on proxy.baseURL: a providers entry that sets no
// base_url gets the proxy base before the config is validated.
func TestNodeWasmProxyBaseURLFillsAnOpenAIProviderWithNoBaseURL(t *testing.T) {
	skipShort(t)
	nodePath(t)
	t.Parallel()
	fake := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{APIKey: acpDummyKey}, harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{"model":"codex/gpt-5.6-sol","providers":{"codex":{"type":"openai","api_key_env":"ANTHROPIC_API_KEY","responses_path":"/responses"}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	env := acpEnv{configPath: cfgPath, sessDir: t.TempDir()}
	in, out, stop := startACPNodeOpts(t, nodeHostOptions{ConfigPath: cfgPath, StoreDir: env.sessDir,
		Env:     map[string]string{"ANTHROPIC_API_KEY": acpDummyKey},
		Connect: map[string]any{"proxy": map[string]any{"baseURL": fake.URL() + "/backend-api/codex"}}})
	c := newACPConn(t, loadACPSchema(t), in, out)
	c.fake, c.env = fake.Server, env
	c.dirs["main"] = t.TempDir()
	acpNew{as: "a"}.run(t, c)
	acpPrompt{as: "a", text: "go", stop: "end_turn"}.run(t, c)
	acpRequests(t, c, 1)
	c.finish(stop)
}
