package hub

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleIndexServesEmbeddedPage(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
}

// TestHandleIndexSetsCSP asserts the served page carries a restrictive
// Content-Security-Policy: defense-in-depth for a page that holds run tokens
// in its URL fragment. The policy must still permit the page's own inline
// script/style and its fetch/SSE to arbitrary box origins (connect-src *),
// while blocking framing and every external resource load.
func TestHandleIndexSetsCSP(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header on the served page")
	}
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'unsafe-inline'",
		"style-src 'unsafe-inline'",
		"connect-src *",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing directive %q", csp, want)
		}
	}
}

func TestHandleIndexRejectsUnknownPaths(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleIndexRejectsNonGet(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestHandleSpawnRejectsGet(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{SpawnCommand: "echo hi"}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/spawn")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// TestHandleSpawnRejectsCrossOrigin is the CSRF guard: POST /spawn execs the
// deployment provision command (real side effects and cost), so a browser
// cross-origin request — one whose Origin names a host other than the hub's
// own — must be rejected before any exec. Without this, any page the operator
// visits could fetch("http://localhost:7777/spawn",{method:"POST"}) as a
// no-preflight CORS simple request and trigger a real box spawn.
func TestHandleSpawnRejectsCrossOrigin(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{SpawnCommand: "echo should-not-run"}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/spawn", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403", resp.StatusCode)
	}
}

// TestHandleSpawnAllowsSameOrigin confirms the CSRF guard is transparent to
// the real page: a same-origin POST (Origin host == request Host, which is
// exactly what the hub page's own fetch sends) runs normally.
func TestHandleSpawnAllowsSameOrigin(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{SpawnCommand: "echo TUNNEL_URL=https://x.example; echo RUN_TOKEN=tok"}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/spawn", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", srv.URL) // the served page's origin == the hub's own
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-origin POST status = %d, want 200", resp.StatusCode)
	}
}

func TestHandleSpawnStreamsSSEFrames(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{SpawnCommand: "echo TUNNEL_URL=https://x.example; echo RUN_TOKEN=tok123"}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/spawn", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	body, err := readAllLines(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(body, "\n")
	if !strings.Contains(joined, `"tunnel_url":"https://x.example"`) {
		t.Errorf("body missing tunnel_url; got:\n%s", joined)
	}
	if !strings.Contains(joined, `"run_token":"tok123"`) {
		t.Errorf("body missing run_token; got:\n%s", joined)
	}
	if !strings.Contains(joined, `"type":"done"`) {
		t.Errorf("body missing done event; got:\n%s", joined)
	}
}

// TestHandleSpawnPassesNameAsBoxNameEnv is the HTTP-level half of
// TestRunSpawnSetsBoxNameEnv (spawn_test.go): POST /spawn's JSON body
// {"name": "..."} must reach the spawn command's own environment as
// HARNESS_HUB_BOX_NAME — see AGENTS.md's spawn contract section.
func TestHandleSpawnPassesNameAsBoxNameEnv(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{SpawnCommand: `echo "NAME=$HARNESS_HUB_BOX_NAME"`}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/spawn", "application/json", strings.NewReader(`{"name":"amber-otter-07"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := readAllLines(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(body, "\n")
	if !strings.Contains(joined, "NAME=amber-otter-07") {
		t.Errorf("body missing box name passthrough; got:\n%s", joined)
	}
}

// TestHandleSpawnToleratesMissingBody confirms the pre-existing "no body at
// all" call (every caller before this field existed) still works exactly
// as before.
func TestHandleSpawnToleratesMissingBody(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{SpawnCommand: "echo hi"}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/spawn", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHandleSpawnNoCommandConfiguredReportsErrorInStream(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/spawn", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the error rides inside the SSE stream, not the HTTP status)", resp.StatusCode)
	}
	body, err := readAllLines(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(body, "\n")
	if !strings.Contains(joined, "no spawn command configured") {
		t.Errorf("body missing configuration error; got:\n%s", joined)
	}
}

func TestIsCrossOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string // "" means no Origin header at all
		host   string
		want   bool
	}{
		{"no origin (non-browser client)", "", "localhost:7777", false},
		{"same origin", "http://localhost:7777", "localhost:7777", false},
		{"different host", "http://evil.example", "localhost:7777", true},
		{"same host different port", "http://localhost:8888", "localhost:7777", true},
		{"opaque null origin", "null", "localhost:7777", true},
		{"unparseable origin", "http://[::1", "localhost:7777", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/spawn", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if got := isCrossOrigin(r); got != c.want {
				t.Errorf("isCrossOrigin(origin=%q, host=%q) = %v, want %v", c.origin, c.host, got, c.want)
			}
		})
	}
}

func readAllLines(r io.Reader) ([]string, error) {
	var lines []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

func TestResolveAddrDefaultsToLoopback(t *testing.T) {
	if got := resolveAddr(""); got != defaultAddr {
		t.Errorf("resolveAddr(\"\") = %q, want %q", got, defaultAddr)
	}
	if got := resolveAddr("localhost:9999"); got != "localhost:9999" {
		t.Errorf("resolveAddr override = %q, want localhost:9999", got)
	}
}

func TestDefaultAddrIsLoopback(t *testing.T) {
	host, _, err := net.SplitHostPort(defaultAddr)
	if err != nil {
		t.Fatal(err)
	}
	if host != "localhost" && host != "127.0.0.1" {
		t.Errorf("default host = %q, want a loopback host", host)
	}
}

func TestSpawnCommandFromEnv(t *testing.T) {
	getenv := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	t.Run("flag set wins over env even when empty", func(t *testing.T) {
		got := spawnCommandFromEnv("", true, getenv(map[string]string{spawnCommandEnv: "from-env"}))
		if got != "" {
			t.Errorf("got %q, want empty (explicit flag wins)", got)
		}
	})
	t.Run("env used when flag not passed", func(t *testing.T) {
		got := spawnCommandFromEnv("", false, getenv(map[string]string{spawnCommandEnv: "from-env"}))
		if got != "from-env" {
			t.Errorf("got %q, want from-env", got)
		}
	})
	t.Run("flag value used when env unset", func(t *testing.T) {
		got := spawnCommandFromEnv("from-flag", true, getenv(map[string]string{}))
		if got != "from-flag" {
			t.Errorf("got %q, want from-flag", got)
		}
	})
	t.Run("no flag, no env: empty", func(t *testing.T) {
		got := spawnCommandFromEnv("", false, getenv(map[string]string{}))
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}
