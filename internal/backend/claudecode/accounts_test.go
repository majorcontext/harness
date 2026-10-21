package claudecode_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

const accountProxy = "http://base:pw@proxy.test:3128"

// accountRun runs one turn of a session that selects accounts, on a CLI entry of vendor "anthropic-sub", and returns the end of the turn
// and the environment that the CLI saw, nil when the CLI did not start.
func accountRun(t *testing.T, routing *config.AccountRouting, accounts map[string]string, env ...string) (protocol.TurnEnded, []string) {
	t.Helper()
	envLog := filepath.Join(t.TempDir(), "env")
	fakeClaude(t, "thinking_reserved_id", append([]string{"FAKE_CLAUDE_ENV_LOG", envLog}, env...)...)
	t.Setenv("ACCT_PROXY", accountProxy)
	t.Setenv("HTTPS_PROXY", "http://stale.test:1")
	t.Setenv("http_proxy", "http://stale.test:1")
	bin, err := fakeClaudeBin()
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Getenv: os.Getenv, Config: config.Config{RequestMaxRetries: &retries, StreamMaxRetries: &retries,
		AccountRouting: routing,
		Providers:      map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: bin, Vendor: "anthropic-sub"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime(t, r)
	s := createClaude(t, r, nil)
	if accounts != nil {
		if _, err := s.Update(bg, protocol.SettingsPatch{Accounts: accounts}); err != nil {
			t.Fatal(err)
		}
	}
	after := s.Snapshot().HeadSeq
	if _, err := s.Prompt(bg, text("a", "hi")); err != nil {
		t.Fatal(err)
	}
	var ended protocol.TurnEnded
	for e, err := range s.Events(bg, after) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			if err := json.Unmarshal(e.Data, &ended); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	var got []string
	if data, err := os.ReadFile(envLog); err == nil {
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
	}
	return ended, got
}

func lookupEnv(env []string, key string) string {
	for _, kv := range slices.Backward(env) {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

var testRouting = &config.AccountRouting{ProxyURLEnv: "ACCT_PROXY", Separator: ":"}

func TestClaudeCodeRoutesTheSelectedAccountThroughTheProxy(t *testing.T) {
	ended, env := accountRun(t, testRouting, map[string]string{"anthropic-sub": "acct_7"})
	if ended.Error != "" {
		t.Fatalf("turn failed: %s", ended.Error)
	}
	const want = "http://base%3Aacct_7:pw@proxy.test:3128"
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
		if got := lookupEnv(env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestClaudeCodeKeepsTheProxyOfAnUnselectedAccount(t *testing.T) {
	ended, env := accountRun(t, testRouting, map[string]string{"other-vendor": "acct_7"})
	if ended.Error != "" {
		t.Fatalf("turn failed: %s", ended.Error)
	}
	if got := lookupEnv(env, "HTTPS_PROXY"); got != "http://stale.test:1" {
		t.Fatalf("HTTPS_PROXY = %q, want the inherited value", got)
	}
}

func TestClaudeCodeFailsBeforeStartWhenTheAccountCannotBeRouted(t *testing.T) {
	tests := []struct {
		name    string
		routing *config.AccountRouting
		env     []string
		want    string
	}{
		{name: "no routing", want: "has no routing"},
		{name: "proxy variable unset", routing: &config.AccountRouting{ProxyURLEnv: "MISSING_PROXY", Separator: ":"}, want: "MISSING_PROXY is empty"},
		{name: "NO_PROXY lists the API host", routing: testRouting, env: []string{"NO_PROXY", "anthropic.com"}, want: "would bypass the proxy"},
		{name: "base URL is loopback", routing: testRouting, env: []string{"ANTHROPIC_BASE_URL", "http://127.0.0.1:9"}, want: "would bypass the proxy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ended, env := accountRun(t, tc.routing, map[string]string{"anthropic-sub": "acct_7"}, tc.env...)
			if ended.StopReason != protocol.StopFailed || !strings.Contains(ended.Error, tc.want) {
				t.Fatalf("ended = %+v, want a failure with %q", ended, tc.want)
			}
			if env != nil {
				t.Fatal("the CLI started")
			}
		})
	}
}
