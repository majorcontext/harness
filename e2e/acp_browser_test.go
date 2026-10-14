package e2e

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// browserHostName names the browser host in the subtests of the ACP table.
const browserHostName = "browser"

// browserWorkDir is the work directory of a browser session: the in-memory
// directory of the Worker file system.
const browserWorkDir = "/work"

// browserDefaultTools is the sorted tool list of a browser session: the default
// set of sdk/embed and the page tool js_echo.
var browserDefaultTools = []string{"edit_file", "glob", "grep", "js_echo", "ls", "read_file", "read_tool_result", "session_info", "write_file"}

// startSharedChromium runs one headless Chromium for all rows of t, through
// sdk/embed/test/browser-server.mjs, and returns its WebSocket endpoint. Each row
// works in a browser context of its own, so no row shares storage, OPFS, or Web
// Locks with another. The process ends when t and its subtests end.
func startSharedChromium(t *testing.T) string {
	t.Helper()
	node, embed := browserTools(t)
	cmd := exec.Command(node, filepath.Join(embed, "test", "browser-server.mjs"))
	cmd.Dir = embed
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	g := startGroup(t, cmd)
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = g.stopGracefully(cleanupGrace)
	})
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		<-g.exited
		if cmd.ProcessState.ExitCode() == browserMissingExit {
			t.Fatalf("Chromium is not installed: run npx playwright install chromium\n%s", stderr.String())
		}
		t.Fatalf("browser-server.mjs ended with %s\n%s", cmd.ProcessState, stderr.String())
	}
	var ready struct {
		WSEndpoint string `json:"wsEndpoint"`
	}
	if err := json.Unmarshal(line, &ready); err != nil || ready.WSEndpoint == "" {
		t.Fatalf("browser-server.mjs output %q: %v", line, err)
	}
	return ready.WSEndpoint
}

// startACPBrowser runs the harness module in a Worker of a context of the shared
// Chromium at wsEndpoint and speaks ACP on stdio through
// sdk/embed/test/browser-host.mjs. The model origin is the scripted server of
// the row behind a CORS wrapper.
func startACPBrowser(t *testing.T, wsEndpoint string, env acpEnv) (io.WriteCloser, io.Reader, func()) {
	t.Helper()
	node, embed := browserTools(t)
	wasm, execJS, err := buildNodeModule()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(env.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	provider := cfg["providers"].(map[string]any)["anthropic"].(map[string]any)
	front := newCORSFront(t, provider["base_url"].(string), nil)
	provider["base_url"] = front.URL
	delete(provider, "api_key_env")
	page := browserPage(t, wasm, execJS, &sync.Map{})
	b, err := json.Marshal(map[string]any{"origin": page.URL, "config": cfg, "wsEndpoint": wsEndpoint})
	if err != nil {
		t.Fatal(err)
	}
	optsPath := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(optsPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, filepath.Join(embed, "test", "browser-host.mjs"), optsPath)
	cmd.Dir = embed
	in, out, g, stderr := startACPCmd(t, cmd)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("browser host stderr:\n%s", stderr.String())
		}
	})
	return in, out, func() { t.Helper(); waitACPExit(t, g, stderr) }
}

// alignBrowserGolden makes the golden of the browser host comparable with the
// golden of the native hosts. The browser host offers the default tool set of
// sdk/embed and the page tool, so its tool list differs from a native host by
// design: the function checks that list against browserDefaultTools and takes the tools of the golden. The
// browser host has no session directory that the test reads, so the golden
// sessions are the ones of the golden file.
func alignBrowserGolden(t *testing.T, got acpGolden, path string) acpGolden {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want acpGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for i := range got.Requests {
		tools := got.Requests[i].Tools
		if !slices.Equal(tools, browserDefaultTools) {
			t.Errorf("tools of request %d = %v, want %v", i+1, tools, browserDefaultTools)
		}
		if i < len(want.Requests) {
			got.Requests[i].Tools = want.Requests[i].Tools
		}
	}
	got.Sessions = want.Sessions
	return got
}

// TestACPBrowserScenarios runs the rows of the ACP table that the browser host
// takes (a streamed text turn, a tool turn, and a page tool round trip through
// the Worker) in headless Chromium. Its frames and model requests must equal
// the golden of the native hosts, except the tool list of the requests.
func TestACPBrowserScenarios(t *testing.T) {
	skipShort(t)
	browserTools(t)
	t.Parallel()
	wsEndpoint := startSharedChromium(t)
	start := func(t *testing.T, env acpEnv) (io.WriteCloser, io.Reader, func()) {
		return startACPBrowser(t, wsEndpoint, env)
	}
	runACPScenarios(t, []acpHost{{name: browserHostName, start: start}})
}
