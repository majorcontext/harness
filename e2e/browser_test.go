package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// browserEnv is set by the browser job of CI. Without it the test skips,
// because it builds the module and needs a browser download; with it, a
// missing node, Playwright, or Chromium fails the test.
const browserEnv = "HARNESS_E2E_BROWSER"

// browserMissingExit is the exit code of sdk/embed/test/browser.mjs when
// Chromium is not installed.
const browserMissingExit = 3

// browserSeen is one request that a fake model origin received.
type browserSeen struct {
	method string
	host   string
	header http.Header
}

// browserFront is a fake model origin: the scripted Anthropic server of
// harnesstest behind a CORS wrapper that records each request. The wrapper is
// the only part that browsers see, so a row controls the preflight answer.
type browserFront struct {
	*httptest.Server
	fake *harnesstest.Server

	mu   sync.Mutex
	seen []browserSeen
}

// newBrowserFront serves steps on a new origin. allow maps the requested
// header list of a preflight to the allowed list; nil allows what is asked.
func newBrowserFront(t *testing.T, allow func(requested string) string, steps ...harnesstest.Step) *browserFront {
	t.Helper()
	fake := harnesstest.NewAnthropic(t, steps...)
	f := newCORSFront(t, fake.URL(), allow)
	f.fake = fake
	return f
}

// newCORSFront serves the CORS wrapper in front of the server at upstream.
func newCORSFront(t *testing.T, upstream string, allow func(requested string) string) *browserFront {
	t.Helper()
	f := &browserFront{}
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.seen = append(f.seen, browserSeen{method: r.Method, host: r.Host, header: r.Header.Clone()})
		f.mu.Unlock()
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			asked := r.Header.Get("Access-Control-Request-Headers")
			if allow != nil {
				asked = allow(asked)
			}
			h.Set("Access-Control-Allow-Headers", asked)
			h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// requests returns the recorded requests with the method.
func (f *browserFront) requests(method string) []browserSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []browserSeen
	for _, s := range f.seen {
		if s.method == method {
			out = append(out, s)
		}
	}
	return out
}

// browserRow is one row of the browser job: what the page runs, and the check
// of the result and of the fake origins.
type browserRow struct {
	name  string
	spec  map[string]any
	check func(t *testing.T, res browserResult, page string)
}

// browserResult is what page.mjs returns for a row, plus the console of the page.
type browserResult struct {
	Failure    string  `json:"failure"`
	StopReason string  `json:"stopReason"`
	Text       string  `json:"text"`
	BootMs     float64 `json:"bootMs"`
	EndedMs    float64 `json:"endedMs"`
	// ServerSawClose is the answer of the fake origin, asked by the page
	// before it closes the harness.
	ServerSawClose bool `json:"serverSawClose"`
	// BrotliBytes is the size of the module under brotli, from Node's zlib.
	BrotliBytes int `json:"brotliBytes"`
	// OfflineFailures counts the model requests that failed while the page was offline.
	OfflineFailures int      `json:"offlineFailures"`
	Console         []string `json:"console"`
	// Raw is the whole result of the row, for a row with its own fields.
	Raw   json.RawMessage `json:"-"`
	Error *struct {
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	} `json:"error"`
}

// browserPage serves the page, the sdk/embed sources, the module, and wasm_exec.js
// on one origin. The page lives under /app, so the relative module and
// wasm_exec.js URLs of page.mjs must resolve against it.
func browserPage(t *testing.T, wasm, execJS string, awaits *sync.Map) *httptest.Server {
	t.Helper()
	root := filepath.Dir(mustGetwd(t))
	mux := http.NewServeMux()
	mux.HandleFunc("/app/index.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html><title>harness</title>")
	})
	file := func(path, name, typ string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", typ)
			http.ServeFile(w, r, name)
		})
	}
	file("/app/harness.wasm", wasm, "application/wasm")
	mux.HandleFunc("/app/fsprobe.wasm", func(w http.ResponseWriter, r *http.Request) {
		path, err := fsProbePath()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/wasm")
		http.ServeFile(w, r, path)
	})
	file("/app/wasm_exec.js", execJS, "text/javascript")
	for _, name := range []string{"page.mjs", "inspect-worker.mjs", "fsprobe-worker.mjs"} {
		file("/page/"+name, filepath.Join(root, "sdk", "embed", "test", "browser", name), "text/javascript")
	}
	embed := filepath.Join(root, "sdk", "embed")
	mux.HandleFunc("/await-canceled", func(w http.ResponseWriter, r *http.Request) {
		wait, ok := awaits.Load(r.URL.Query().Get("row"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, strconv.FormatBool(wait.(func() bool)()))
	})
	mux.Handle("/embed/", http.StripPrefix("/embed/", http.FileServer(http.Dir(filepath.Join(embed, "src")))))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// browserTools returns node and the sdk/embed directory. Without
// HARNESS_E2E_BROWSER=1 the test skips, because it builds the module and starts
// a browser; with it, a missing node or Playwright fails.
func browserTools(t *testing.T) (node, embed string) {
	t.Helper()
	if os.Getenv(browserEnv) != "1" {
		t.Skip(browserEnv + "=1 enables the browser job")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("the browser job needs node: %v", err)
	}
	embed = filepath.Join(filepath.Dir(mustGetwd(t)), "sdk", "embed")
	if _, err := os.Stat(filepath.Join(embed, "node_modules", "playwright")); err != nil {
		t.Fatalf("Playwright is not installed: run npm ci in sdk/embed")
	}
	return node, embed
}

// runBrowser runs sdk/embed/test/browser.mjs on the rows and returns the results by row name.
func runBrowser(t *testing.T, node, embed, origin, modulePath string, rows []map[string]any) map[string]browserResult {
	t.Helper()
	b, err := json.Marshal(map[string]any{"origin": origin, "modulePath": modulePath, "rows": rows})
	if err != nil {
		t.Fatal(err)
	}
	optsPath := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(optsPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, filepath.Join(embed, "test", "browser.mjs"), optsPath)
	cmd.Dir = embed
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	g := startGroup(t, cmd)
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	select {
	case <-g.exited:
	case <-timer.C:
		g.kill()
		t.Fatalf("browser.mjs did not end in 2 minutes\n%s", stderr.String())
	}
	if !cmd.ProcessState.Success() {
		if cmd.ProcessState.ExitCode() == browserMissingExit {
			t.Fatalf("Chromium is not installed: run npx playwright install chromium\n%s", stderr.String())
		}
		t.Fatalf("browser.mjs ended with %s\n%s", cmd.ProcessState, stderr.String())
	}
	var raw struct {
		Rows map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("browser.mjs output: %v\n%s", err, stdout.String())
	}
	out := make(map[string]browserResult, len(raw.Rows))
	for name, r := range raw.Rows {
		res := browserResult{Raw: r}
		if err := json.Unmarshal(r, &res); err != nil {
			t.Fatalf("browser.mjs result of %q: %v\n%s", name, err, r)
		}
		out[name] = res
	}
	return out
}

func headerNames(h http.Header) []string {
	var names []string
	for k := range h {
		names = append(names, strings.ToLower(k))
	}
	return names
}

// browserKit collects the rows of the browser job.
type browserKit struct {
	t    *testing.T
	wasm string
	rows []browserRow
	// awaits maps a row name to a function that the page calls, before any
	// teardown, to learn whether the model server saw the connection close.
	awaits sync.Map
}

func (k *browserKit) add(name string, spec map[string]any, check func(t *testing.T, res browserResult, page string)) {
	spec["name"] = name
	k.rows = append(k.rows, browserRow{name: name, spec: spec, check: check})
}

func (k *browserKit) noFailure(t *testing.T, res browserResult) {
	t.Helper()
	if res.Failure != "" {
		t.Fatalf("the page failed: %s\nconsole: %v", res.Failure, res.Console)
	}
}

func (k *browserKit) text(s string) harnesstest.Step {
	return harnesstest.Step{Name: "text", Reply: harnesstest.Reply{Text: s}}
}

// unused is a step for a fake origin that must get no request.
func (k *browserKit) unused(s string) harnesstest.Step {
	return harnesstest.Step{Name: "unused", Repeat: true, Reply: harnesstest.Reply{Text: s}}
}

// turnRows adds the turns that cross the origin and make the preflight.
func (k *browserKit) turnRows() {
	t := k.t
	text, add, noFailure := k.text, k.add, k.noFailure

	toolTurn := []harnesstest.Step{
		{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "js_echo", Input: map[string]any{"text": "hi"}}}}},
		text("done"),
	}
	{
		f := newBrowserFront(t, nil, toolTurn...)
		add("tool turn with a page tool and one preflight", map[string]any{"kind": "turn", "baseURL": f.URL}, func(t *testing.T, res browserResult, origin string) {
			noFailure(t, res)
			if res.StopReason != "end_turn" {
				t.Fatalf("stopReason = %q, error %+v, console %v", res.StopReason, res.Error, res.Console)
			}
			reqs := f.fake.Requests()
			if len(reqs) != 2 || !strings.Contains(requestText(reqs[1]), "echo:hi") {
				t.Errorf("the second model request lacks the tool result: %d requests", len(reqs))
			}
			opts, posts := f.requests(http.MethodOptions), f.requests(http.MethodPost)
			if len(opts) != 1 || len(posts) != 2 {
				t.Fatalf("OPTIONS = %d, POST = %d, want 1 and 2", len(opts), len(posts))
			}
			asked := strings.Split(opts[0].header.Get("Access-Control-Request-Headers"), ",")
			if !slices.Contains(asked, "content-type") {
				t.Errorf("preflight asks for %v, want content-type", asked)
			}
			sent := headerNames(posts[0].header)
			for _, name := range asked {
				if !slices.Contains(sent, name) {
					t.Errorf("preflight asks for %q, which the request does not send", name)
				}
			}
			if got := posts[0].header.Get("Origin"); got != origin {
				t.Errorf("Origin = %q, want the page origin %q", got, origin)
			}
		})
	}
	{
		f := newBrowserFront(t, nil, text("hello from the browser"))
		add("streamed text turn", map[string]any{"kind": "turn", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			if res.StopReason != "end_turn" || res.Text != "hello from the browser" {
				t.Errorf("stopReason = %q, text = %q, error %+v", res.StopReason, res.Text, res.Error)
			}
		})
	}
}

// streamRows adds a cancel and the load of the module.
func (k *browserKit) streamRows() {
	t, wasm := k.t, k.wasm
	text, add, noFailure := k.text, k.add, k.noFailure
	{
		f := newBrowserFront(t, nil, harnesstest.Step{Name: "hold", Reply: harnesstest.Reply{Text: "partial", Block: true}})
		const name = "cancel mid-stream closes the model connection"
		k.awaits.Store(name, func() bool { return f.fake.AwaitCanceled("hold", 2*time.Second) })
		add(name, map[string]any{"kind": "cancel", "baseURL": f.URL, "awaitRow": name}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			if res.StopReason != "cancelled" {
				t.Errorf("stopReason = %q, error %+v", res.StopReason, res.Error)
			}
			if !res.ServerSawClose {
				t.Error("the model server did not see the connection close before the page tore anything down")
			}
			if res.EndedMs > 2000 {
				t.Errorf("the turn ended %.0f ms after the cancel, want under 2000", res.EndedMs)
			}
		})
	}
	{
		f := newBrowserFront(t, nil, text("ok"))
		add("cold boot on a throttled link stays under the bound", map[string]any{"kind": "turn", "baseURL": f.URL, "throttle": true}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			info, err := os.Stat(wasm)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("module %d bytes raw, %d bytes brotli (information only), boot %.0f ms at 50 Mbps and 40 ms", info.Size(), res.BrotliBytes, res.BootMs)
			if res.StopReason != "end_turn" {
				t.Errorf("stopReason = %q, error %+v", res.StopReason, res.Error)
			}
			if res.BootMs > 15000 {
				t.Errorf("boot took %.0f ms, want under 15000", res.BootMs)
			}
		})
	}
}

// failureRows adds a CORS block and the loss of the network.
func (k *browserKit) failureRows() {
	t := k.t
	text, unused, add, noFailure := k.text, k.unused, k.add, k.noFailure
	{
		onlyContentType := func(string) string { return "content-type" }
		f := newBrowserFront(t, onlyContentType, unused("never"))
		add("a CORS block is one permanent attempt", map[string]any{"kind": "turn", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			if res.Error == nil {
				t.Fatalf("the turn ended %q, want an error", res.StopReason)
			}
			if res.Error.Data["error_class"] != "permanent" || !strings.Contains(res.Error.Message, "blocked") {
				t.Errorf("error = %+v, want a permanent error that names the browser block", res.Error)
			}
			if n := len(f.requests(http.MethodOptions)); n != 1 {
				t.Errorf("preflights = %d, want 1 (no retry)", n)
			}
			if n := len(f.requests(http.MethodPost)); n != 0 {
				t.Errorf("POST requests = %d, want 0", n)
			}
		})
	}
	{
		f := newBrowserFront(t, nil, text("back online"))
		add("offline keeps the retry path", map[string]any{"kind": "offline", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			if res.StopReason != "end_turn" {
				t.Errorf("stopReason = %q, error %+v, console %v", res.StopReason, res.Error, res.Console)
			}
			if res.OfflineFailures != 1 {
				t.Errorf("failed requests while offline = %d, want 1 before the retry", res.OfflineFailures)
			}
		})
	}
}

// headerRows adds the headers of the browser and of the proxy.
func (k *browserKit) headerRows() {
	t := k.t
	text, unused, add, noFailure := k.text, k.unused, k.add, k.noFailure
	{
		f := newBrowserFront(t, nil, text("ok"))
		headers := map[string]string{
			"User-Agent": "harness-test/1", "Accept-Encoding": "identity", "Host": "evil.example", "Content-Length": "5",
			"Connection": "close", "Origin": "https://evil.example", "Cookie": "a=b", "X-Custom": "kept",
		}
		add("forbidden headers are dropped and the others pass", map[string]any{"kind": "turn", "baseURL": f.URL, "extraHeaders": headers}, func(t *testing.T, res browserResult, origin string) {
			noFailure(t, res)
			if res.StopReason != "end_turn" {
				t.Fatalf("stopReason = %q, error %+v", res.StopReason, res.Error)
			}
			posts := f.requests(http.MethodPost)
			if len(posts) != 1 {
				t.Fatalf("POST requests = %d, want 1", len(posts))
			}
			p := posts[0]
			if ua := p.header.Get("User-Agent"); ua == "harness-test/1" || ua == "" {
				t.Errorf("User-Agent = %q, want the browser value", ua)
			}
			if got := p.header.Get("Accept-Encoding"); got == "identity" {
				t.Errorf("Accept-Encoding = %q, want the browser value", got)
			}
			if p.host == "evil.example" {
				t.Errorf("Host = %q, want the server host", p.host)
			}
			if p.header.Get("Origin") != origin {
				t.Errorf("Origin = %q, want %q", p.header.Get("Origin"), origin)
			}
			if p.header.Get("Cookie") != "" {
				t.Errorf("Cookie = %q, want none", p.header.Get("Cookie"))
			}
			if got := p.header.Get("Connection"); got == "close" {
				t.Errorf("Connection = %q, want the browser value", got)
			}
			if got := p.header.Get("X-Custom"); got != "kept" {
				t.Errorf("X-Custom = %q, want kept", got)
			}
		})
	}
	{
		f := newBrowserFront(t, nil, text("via proxy"))
		spec := map[string]any{"kind": "turn", "proxy": map[string]any{"baseURL": f.URL, "headers": map[string]string{"X-Proxy-Token": "proxy-secret"}}}
		add("a proxy header goes to the proxy origin", spec, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			if res.StopReason != "end_turn" {
				t.Fatalf("stopReason = %q, error %+v", res.StopReason, res.Error)
			}
			posts := f.requests(http.MethodPost)
			if len(posts) != 1 || posts[0].header.Get("X-Proxy-Token") != "proxy-secret" {
				t.Errorf("proxy origin requests = %+v, want one with X-Proxy-Token", posts)
			}
		})
	}
	{
		proxy := newBrowserFront(t, nil, unused("proxy"))
		other := newBrowserFront(t, nil, text("other"))
		spec := map[string]any{"kind": "turn", "baseURL": other.URL, "proxy": map[string]any{"baseURL": proxy.URL, "headers": map[string]string{"X-Proxy-Token": "proxy-secret"}}}
		add("a proxy header never goes to another origin", spec, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			if res.StopReason != "end_turn" {
				t.Fatalf("stopReason = %q, error %+v", res.StopReason, res.Error)
			}
			posts := other.requests(http.MethodPost)
			if len(posts) != 1 || posts[0].header.Get("X-Proxy-Token") != "" {
				t.Errorf("other origin requests = %+v, want one with no X-Proxy-Token", posts)
			}
			if n := len(proxy.requests(http.MethodPost)); n != 0 {
				t.Errorf("proxy origin got %d requests, want 0 for a provider with its own base URL", n)
			}
		})
	}
}

// TestBrowserRows pins the sentences of the WebAssembly section on the browser
// host: the module runs in a module Worker of headless Chromium, its model
// requests cross origins with a preflight, a cancel ends the stream, the
// browser decides forbidden headers, and a proxy header goes to the proxy
// origin only. It is a process-boundary test: Chromium is the host.
func TestBrowserRows(t *testing.T) {
	skipShort(t)
	node, embed := browserTools(t)
	t.Parallel()
	wasm, execJS, err := buildNodeModule()
	if err != nil {
		t.Fatal(err)
	}
	k := &browserKit{t: t, wasm: wasm}
	page := browserPage(t, wasm, execJS, &k.awaits)
	k.turnRows()
	k.streamRows()
	k.failureRows()
	k.headerRows()
	k.storeRows()
	k.fsRows()
	specs := make([]map[string]any, len(k.rows))
	for i, r := range k.rows {
		specs[i] = r.spec
	}
	results := runBrowser(t, node, embed, page.URL, wasm, specs)
	for _, r := range k.rows {
		t.Run(r.name, func(t *testing.T) {
			res, ok := results[r.name]
			if !ok {
				t.Fatalf("no result for the row")
			}
			r.check(t, res, page.URL)
		})
	}
}
