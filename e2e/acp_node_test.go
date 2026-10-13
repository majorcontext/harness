package e2e

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
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
}

// startACPNode runs the wasm module under Node, with the config, the store
// directory, and the default tool set of env. Without HARNESS_E2E_NODE=1 a
// missing Node skips the test; with it, the test fails.
func startACPNode(t *testing.T, env acpEnv) (io.WriteCloser, io.Reader, func()) {
	t.Helper()
	return startACPNodeOpts(t, nodeHostOptions{ConfigPath: env.configPath, StoreDir: env.sessDir,
		Env: map[string]string{"ANTHROPIC_API_KEY": acpDummyKey}, BuiltinTools: env.allowed})
}

// nodePath returns the node binary. A missing node skips the test, unless
// HARNESS_E2E_NODE=1 says that the job needs it.
func nodePath(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv(nodeEnv) == "1" {
			t.Fatalf("the Node wasm host needs node: %v", err)
		}
		t.Skip("node is not installed")
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
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("node stderr:\n%s", stderr.String())
		}
	})
	return in, out, func() { t.Helper(); waitACPExit(t, g, stderr) }
}

const (
	nodeMCPOff = `"mcpCapabilities":{"http":false`
	nodeMCPOn  = `"mcpCapabilities":{"http":true`
)

// alignNodeMCP rewrites the one capability in which the wasm host differs from
// the native hosts, so the rest of its frames compare with the shared golden:
// the wasm host starts no MCP server and advertises none. It fails when an
// initialize answer advertises MCP.
func alignNodeMCP(t *testing.T, g acpGolden) acpGolden {
	t.Helper()
	for i, f := range g.Wire {
		text := string(f.Frame)
		if f.Dir != "agent" || !strings.Contains(text, `"mcpCapabilities"`) {
			continue
		}
		if !strings.Contains(text, nodeMCPOff) {
			t.Errorf("the wasm host advertises MCP: %s", text)
			continue
		}
		g.Wire[i].Frame = json.RawMessage(strings.Replace(text, nodeMCPOff, nodeMCPOn, 1))
	}
	return g
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
