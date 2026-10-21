package config

import (
	"net/url"
	"strings"
	"testing"
)

func TestAccountRoutingProxyURL(t *testing.T) {
	routing := AccountRouting{ProxyURLEnv: "PROXY", Separator: ":"}
	tests := []struct {
		name    string
		routing AccountRouting
		env     string
		account string
		want    string
		wantErr string
	}{
		{name: "username and password", routing: routing, env: "http://base:secret@proxy.test:8080", account: "acct_1", want: "http://base%3Aacct_1:secret@proxy.test:8080"},
		{name: "username only", routing: routing, env: "https://base@proxy.test", account: "a", want: "https://base%3Aa@proxy.test"},
		{name: "account is escaped", routing: routing, env: "http://base@proxy.test", account: "a@b/c", want: "http://base%3Aa%40b%2Fc@proxy.test"},
		{name: "empty variable", routing: routing, env: "", account: "a", wantErr: "PROXY is empty"},
		{name: "no host", routing: routing, env: "http://base@", account: "a", wantErr: "not a proxy URL"},
		{name: "unparsable", routing: routing, env: "http://base:pw@proxy.test:bad", account: "a", wantErr: "not a proxy URL"},
		{name: "no username", routing: routing, env: "http://proxy.test", account: "a", wantErr: "no username"},
		{name: "unsupported scheme", routing: routing, env: "ftp://base@proxy.test", account: "a", wantErr: "unsupported proxy scheme"},
		{name: "empty account", routing: routing, env: "http://base@proxy.test", account: "", wantErr: "needs an account ID"},
		{name: "no separator", routing: AccountRouting{ProxyURLEnv: "PROXY"}, env: "http://base@proxy.test", account: "a", wantErr: "requires proxy_url_env and separator"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := tc.routing.ProxyURL(func(string) string { return tc.env }, tc.account)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "pw") {
					t.Fatalf("error holds a password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if u.String() != tc.want {
				t.Fatalf("URL = %s, want %s", u, tc.want)
			}
		})
	}
}

func TestAccountRoutingProxyURLNeedsGetenv(t *testing.T) {
	if _, err := (AccountRouting{ProxyURLEnv: "PROXY", Separator: ":"}).ProxyURL(nil, "a"); err == nil {
		t.Fatal("a nil Getenv gave no error")
	}
}

func TestNoProxyBypass(t *testing.T) {
	tests := []struct {
		name    string
		noProxy string
		target  string
		bypass  bool
	}{
		{name: "empty list", target: "https://api.anthropic.com"},
		{name: "unrelated host", noProxy: "example.com,10.0.0.0/8", target: "https://api.anthropic.com"},
		{name: "wildcard", noProxy: "*", target: "https://api.anthropic.com", bypass: true},
		{name: "exact host", noProxy: "api.anthropic.com", target: "https://api.anthropic.com", bypass: true},
		{name: "parent domain", noProxy: "anthropic.com", target: "https://api.anthropic.com", bypass: true},
		{name: "leading dot matches subdomain", noProxy: ".anthropic.com", target: "https://api.anthropic.com", bypass: true},
		{name: "leading dot skips the domain", noProxy: ".anthropic.com", target: "https://anthropic.com"},
		{name: "similar suffix", noProxy: "thropic.com", target: "https://api.anthropic.com"},
		{name: "cidr", noProxy: "10.0.0.0/8", target: "https://10.1.2.3", bypass: true},
		{name: "port match", noProxy: "api.openai.com:443", target: "https://api.openai.com", bypass: true},
		{name: "port mismatch", noProxy: "api.openai.com:8443", target: "https://api.openai.com"},
		{name: "websocket default port", noProxy: "api.openai.com:443", target: "wss://api.openai.com/v1", bypass: true},
		{name: "case", noProxy: "API.OpenAI.com", target: "https://api.openai.com", bypass: true},
		{name: "loopback address", target: "http://127.0.0.1:9000", bypass: true},
		{name: "localhost", target: "http://localhost:9000", bypass: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			got := NoProxyBypass(tc.noProxy, u) != nil
			if got != tc.bypass {
				t.Fatalf("bypass = %v, want %v", got, tc.bypass)
			}
		})
	}
}
