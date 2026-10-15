package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/internal/tool/webfetch"
	"github.com/majorcontext/harness/protocol"
)

const webEnvChildURL = "HARNESS_E2E_WEB_ENV_URL"

// TestWebFetchEnvChild is the body of a child process that the test below
// starts. It fetches the URL in webEnvChildURL with the production proxy choice:
// the seam leaves Proxy unset, so only the variables of the process decide.
func TestWebFetchEnvChild(t *testing.T) {
	target := os.Getenv(webEnvChildURL)
	if target == "" {
		t.Skip("runs only as the child of a web_fetch environment test")
	}
	tool := webfetch.New(webfetch.Options{Test: webfetch.Seam{
		AllowLoopback: true,
		Resolver: fakeWebResolver{
			"proxied.example": {netip.MustParseAddr("93.184.216.34")},
			"direct.example":  {netip.MustParseAddr("127.0.0.1")},
		},
	}})
	args, _ := json.Marshal(map[string]string{"url": target})
	res, err := tool.Run(t.Context(), protocol.ToolCall{Arguments: args})
	if err != nil {
		fmt.Printf("RESULT: error: %v\n", err)
		return
	}
	fmt.Printf("RESULT: %s\n", strings.ReplaceAll(res.Text, "\n", " "))
}

func TestWebFetchReadsTheProxyVariablesOfTheEnvironment(t *testing.T) {
	skipShort(t)
	for _, tc := range []struct {
		name, noProxy, url, want string
		hits                     int32
	}{
		{"http_proxy_carries_the_request", "", "http://proxied.example/page", "via-proxy=true", 1},
		{"no_proxy_sends_the_host_direct", "direct.example", webHostURL("direct.example", "/page"), "Fixture heading", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				_, _ = fmt.Fprintf(w, "proxy saw %s %s via-proxy=true", r.Method, r.URL)
			}))
			defer proxy.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestWebFetchEnvChild$", "-test.count=1")
			cmd.Env = childEnv(map[string]string{webEnvChildURL: tc.url, "HTTP_PROXY": proxy.URL, "HTTPS_PROXY": proxy.URL, "NO_PROXY": tc.noProxy})
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("child output lacks %q:\n%s", tc.want, out)
			}
			if got := hits.Load(); got != tc.hits {
				t.Errorf("the proxy saw %d requests, want %d", got, tc.hits)
			}
		})
	}
}

// childEnv is the environment of this process without any proxy variable,
// plus set.
func childEnv(set map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "REQUEST_METHOD":
			continue
		}
		env = append(env, kv)
	}
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	return env
}
