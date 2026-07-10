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
