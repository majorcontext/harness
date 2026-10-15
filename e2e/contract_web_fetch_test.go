package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// webLoopbackEnv makes the runtime host let web_fetch reach loopback. The
// serve binary has no such seam, so a row that sets it is a runtime-only row.
const webLoopbackEnv = "HARNESS_E2E_WEB_LOOPBACK"

var webOn = map[string]any{"web": map[string]any{"fetch": true}}

const webPage = `<!doctype html>
<html><head><title>Fixture Page</title><style>body{color:red}</style><script>var secret = "script-text";</script></head>
<body>
<nav><a href="/menu">Menu item</a></nav>
<h1>Fixture heading</h1>
<p>Read the <a href="/docs/guide">guide</a> and the <a href="https://example.org/abs">absolute link</a>, with <strong>bold</strong> and <em>italic</em> and <code>inline()</code>.</p>
<ul><li>first</li><li>second<ul><li>nested</li></ul></li></ul>
<pre><code class="language-go">fmt.Println("hi")
</code></pre>
<table><tr><th>Name</th><th>Size</th></tr><tr><td>a</td><td>1</td></tr></table>
<img src="/pic.png" alt="A picture">
<footer>Footer noise</footer>
</body></html>`

// webFixture is the local server that the web_fetch rows fetch. It is shared
// by the rows of the process and lives until the process ends.
var webFixture = sync.OnceValue(func() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(webPage))
	})
	mux.HandleFunc("/hop1", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/hop2", http.StatusFound) })
	mux.HandleFunc("/hop2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/page", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/to-private", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/secret", http.StatusFound)
	})
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		chunk := []byte(strings.Repeat("0123456789abcdef", 4096))
		for range 200 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(make([]byte, 2048))
	})
	mux.HandleFunc("/data.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/pic.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(pngBody()))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<h1>Not here</h1>"))
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "method=%s ua-has-harness=%t cookie=%q authorization=%q referer=%q body=%d",
			r.Method, strings.HasPrefix(r.Header.Get("User-Agent"), "harness/"), r.Header.Get("Cookie"), r.Header.Get("Authorization"), r.Header.Get("Referer"), r.ContentLength)
	})
	mux.HandleFunc("/hostile", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(strings.Repeat("<blockquote>", 500) + strings.Repeat("line of text<br>", 300_000) + strings.Repeat("</blockquote>", 500)))
	})
	mux.HandleFunc("/long", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(strings.Repeat("<p>paragraph number one two three</p>", 100_000)))
	})
	mux.HandleFunc("/hop-echo", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/echo", http.StatusFound) })
	return httptest.NewServer(mux)
})

func webURL(path string) string { return webFixture().URL + path }

func fetchCall(url string, kv ...any) harnesstest.ToolCall {
	return ftTool("web_fetch", ftArgs(append([]any{"url", url}, kv...)...))
}

func webLoopbackDriver(config map[string]any) func(*testing.T, host, string) driver {
	return inTreeEnv(nil, "", config, map[string]string{webLoopbackEnv: "1"})
}

func TestContractWebFetchOffer(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "web_fetch_is_not_offered_unless_web_fetch_is_true",
			model:   toolChain(),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_is_offered_when_web_fetch_is_true",
			config:  webOn,
			model:   toolChain(),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_call_without_web_fetch_true_is_an_unknown_tool",
			model:   toolChain(fetchCall("https://example.com/")),
			actions: oneTurn,
		},
	})
}

func TestContractWebFetchRefusals(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "web_fetch_refuses_private_loopback_and_link_local_addresses",
			model: toolChain(
				fetchCall("http://10.0.0.1/"),
				fetchCall("http://192.168.1.10:8080/admin"),
				fetchCall("http://172.16.0.1/"),
				fetchCall("http://100.64.0.1/"),
				fetchCall("http://169.254.169.254/latest/meta-data/"),
				fetchCall("http://0.0.0.0/"),
				fetchCall("http://224.0.0.1/"),
				fetchCall("http://127.0.0.1:1/"),
				fetchCall("http://[::1]:1/"),
				fetchCall("http://[fd00::1]/"),
				fetchCall("http://[fe80::1]/"),
				fetchCall("http://[::ffff:10.0.0.1]/"),
				fetchCall("http://[::ffff:127.0.0.1]:1/"),
				fetchCall("http://localhost:1/"),
			),
			config:  webOn,
			actions: oneTurn,
		},
		{
			name: "web_fetch_refuses_other_schemes_credentials_and_bad_input",
			model: toolChain(
				fetchCall("file:///etc/passwd"),
				fetchCall("ftp://example.com/x"),
				fetchCall("https://user:pass@example.com/"),
				fetchCall("example.com"),
				fetchCall("https://example.com/", "format", "pdf"),
				fetchCall("https://example.com/", "max_bytes", -1),
				ftTool("web_fetch", ftArgs()),
			),
			config:  webOn,
			actions: oneTurn,
		},
	})
}

func TestContractWebFetchPlugin(t *testing.T) {
	cfg := pluginConfig(t, map[string]any{"deny_fetch": "forbidden.example"})
	cfg["web"] = webOn["web"]
	runScenarios(t, []scenario{
		{
			name:    "plugin_hook_denies_a_web_fetch_call_by_url",
			config:  cfg,
			model:   toolChain(fetchCall("https://forbidden.example/page"), fetchCall("http://10.0.0.1/")),
			actions: oneTurn,
		},
	})
}

func TestContractWebFetchContent(t *testing.T) {
	loop := webLoopbackDriver(webOn)
	runScenarios(t, []scenario{
		{
			name:    "web_fetch_turns_html_into_markdown",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/page"))),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_turns_html_into_text_or_returns_the_raw_source",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/page"), "format", "text"), fetchCall(webURL("/page"), "format", "raw")),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_passes_json_through_and_names_a_failing_status",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/data.json")), fetchCall(webURL("/missing"))),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_bounds_the_output_for_a_deeply_nested_page",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/hostile"), "max_bytes", 2000)),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_parses_only_the_first_two_mebibytes_of_an_html_body",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/long"), "max_bytes", 1000)),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_answers_a_binary_type_with_its_type_and_size",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/bin"))),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_returns_an_image_as_a_blob",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/pic.png"))),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_cuts_a_body_at_ten_mebibytes_and_the_text_at_max_bytes",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/big"), "max_bytes", 1000)),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_sends_a_get_with_the_harness_user_agent_and_no_cookie_or_credential",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/echo")), fetchCall(webURL("/hop-echo"))),
			actions: oneTurn,
		},
	})
}

func TestContractWebFetchRedirects(t *testing.T) {
	loop := webLoopbackDriver(webOn)
	runScenarios(t, []scenario{
		{
			name:    "web_fetch_follows_redirects_and_names_the_final_url",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/hop1"))),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_checks_the_address_of_each_redirect",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/to-private")), fetchCall(webURL("/to-file"))),
			actions: oneTurn,
		},
		{
			name:    "web_fetch_stops_after_five_redirects",
			driver:  loop,
			model:   toolChain(fetchCall(webURL("/loop"))),
			actions: oneTurn,
		},
	})
}

func TestContractWebFetchClaudeCode(t *testing.T) {
	deny := pluginConfig(t, map[string]any{"deny_fetch": "forbidden.example"})
	deny["web"] = webOn["web"]
	refused := []turnCall{{"web_fetch", map[string]any{"url": "http://10.0.0.1/"}}}
	runScenarios(t, []scenario{{
		name:    "claudecode_bridge_offers_web_fetch_when_web_fetch_is_true",
		driver:  claudeLane{mode: "normal", listTools: true, extra: webOn}.newDriver,
		actions: withActions(claudeOneTurn, claudeOfferedTools{as: "a"}),
	}, {
		name:    "claudecode_bridge_runs_web_fetch_through_the_engine",
		driver:  claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{refused}, extra: webOn}.newDriver,
		actions: claudeOneTurn,
	}, {
		name: "claudecode_plugin_hook_denies_a_web_fetch_call_of_the_cli",
		driver: claudeLane{mode: "turn_calls", extra: deny,
			turnCalls: [][]turnCall{{{"web_fetch", map[string]any{"url": "https://forbidden.example/page"}}}}}.newDriver,
		actions: claudeOneTurn,
	}})
}

func TestContractWebFetchClaudeCodeToolNames(t *testing.T) {
	lane := claudeLane{mode: "normal", extra: webOn}.newDriver
	runScenarios(t, []scenario{{
		name:   "claudecode_system_prompt_maps_webfetch_to_web_fetch_when_the_turn_offers_it",
		driver: lane,
		actions: withActions(claudeOneTurn, claudeSystemPrompt{as: "maps", contains: "Where they name WebFetch, use web_fetch."},
			claudeSystemPrompt{as: "absent", contains: "There is no TodoWrite, WebFetch"}),
	}, {
		name:   "claudecode_system_prompt_keeps_webfetch_absent_when_the_turn_does_not_offer_web_fetch",
		driver: lane,
		actions: []action{create{as: "a", allowed: []string{"bash"}}, submit{as: "a", text: "run it"}, waitIdle{as: "a"},
			claudeSystemPrompt{as: "maps", contains: "Where they name WebFetch, use web_fetch."},
			claudeSystemPrompt{as: "absent", contains: "There is no TodoWrite, WebFetch"}},
	}})
}

func webRuntime(t *testing.T, cfg config.Config, fake *harnesstest.Server) *harness.Runtime {
	t.Helper()
	t.Setenv("HARNESS_E2E_TOOL_KEY", "k")
	cfg.ContextWindowTokens = 100000
	cfg.Providers = map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_TOOL_KEY"}}
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), WorkDir: t.TempDir(), Getenv: os.Getenv, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func firstRequestTools(t *testing.T, r *harness.Runtime, fake *harnesstest.Server, allowed []string) []string {
	t.Helper()
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test", AllowedTools: allowed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	return fake.Requests()[0].Tools
}

func TestContractRuntimeWebFetchAllowedTools(t *testing.T) {
	skipShort(t)
	on := config.Config{Web: config.WebSpec{Fetch: new(true)}}
	reply := harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}}
	t.Run("a_list_that_names_web_fetch_offers_it_alone", func(t *testing.T) {
		fake := harnesstest.NewChat(t, reply)
		if got := firstRequestTools(t, webRuntime(t, on, fake), fake, []string{"web_fetch"}); strings.Join(got, ",") != "web_fetch" {
			t.Errorf("tools = %v, want [web_fetch]", got)
		}
	})
	t.Run("a_list_without_web_fetch_leaves_it_out", func(t *testing.T) {
		fake := harnesstest.NewChat(t, reply)
		if got := firstRequestTools(t, webRuntime(t, on, fake), fake, []string{"read_file"}); strings.Contains(strings.Join(got, ","), "web_fetch") {
			t.Errorf("tools = %v, want no web_fetch", got)
		}
	})
	t.Run("a_list_that_names_web_fetch_offers_nothing_when_web_fetch_is_not_true", func(t *testing.T) {
		fake := harnesstest.NewChat(t, reply)
		if got := firstRequestTools(t, webRuntime(t, config.Config{}, fake), fake, []string{"web_fetch"}); len(got) != 0 {
			t.Errorf("tools = %v, want none", got)
		}
	})
	t.Run("a_backend_that_owns_the_loop_accepts_web_fetch_by_name_only_when_web_fetch_is_true", func(t *testing.T) {
		for _, c := range []struct {
			name string
			cfg  config.Config
			ok   bool
		}{{"off", config.Config{}, false}, {"on", on, true}} {
			c.cfg.Providers = map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: fakeClaudePath()}}
			r, err := harness.New(harness.Options{Store: store.NewMemStore(), WorkDir: t.TempDir(), Config: c.cfg})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			_, err = r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet", AllowedTools: []string{"web_fetch"}})
			if c.ok != (err == nil) || !c.ok && !errors.Is(err, harness.ErrInvalidRequest) {
				t.Errorf("web.fetch %s: Create = %v, want success %v", c.name, err, c.ok)
			}
		}
	})
	t.Run("an_embedder_tool_named_web_fetch_fails_new_when_web_fetch_is_true", func(t *testing.T) {
		_, err := harness.New(harness.Options{Store: store.NewMemStore(), WorkDir: t.TempDir(), Config: on, Tools: []harness.Tool{hostWebFetch{}}})
		if err == nil || !strings.Contains(err.Error(), "web_fetch") {
			t.Errorf("New = %v, want an error that names web_fetch", err)
		}
	})
}

type hostWebFetch struct{}

func (hostWebFetch) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "web_fetch", Description: "host", InputSchema: []byte(`{"type":"object"}`)}
}

func (hostWebFetch) Run(context.Context, harness.Call) (protocol.ToolResult, error) {
	return protocol.ToolResult{Text: "host"}, nil
}
