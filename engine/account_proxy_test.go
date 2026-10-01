package engine

import (
	"encoding/base64"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestAccountProxyURLEncodesCanonicalSelectorAndReplacesSuffix(t *testing.T) {
	base := "https://subject%7Cbox:pass%3Aword@proxy.example:8443/path?keep=1"
	selected, err := accountProxyURL(base, map[string]string{"codex": "acct_b", "claude": "acct_a"})
	if err != nil {
		t.Fatal(err)
	}
	firstURL, err := url.Parse(selected)
	if err != nil {
		t.Fatal(err)
	}
	firstEncoded := strings.TrimPrefix(strings.Split(firstURL.User.Username(), "|")[2], "accounts-v1=")
	firstData, err := base64.RawURLEncoding.DecodeString(firstEncoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstData) != `{"claude":"acct_a","codex":"acct_b"}` {
		t.Fatalf("selector payload is not canonical: %s", firstData)
	}
	replaced, err := accountProxyURL(selected, map[string]string{"codex": "acct_c"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(replaced)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.User.Username(), "|accounts-v1=") || strings.Count(u.User.Username(), "accounts-v1=") != 1 {
		t.Fatalf("proxy username = %q", u.User.Username())
	}
	password, ok := u.User.Password()
	if !ok || password != "pass:word" || u.Host != "proxy.example:8443" || u.Path != "/path" || u.RawQuery != "keep=1" {
		t.Fatalf("proxy base fields changed: URL host=%q path=%q query=%q", u.Host, u.Path, u.RawQuery)
	}
	encoded := strings.TrimPrefix(strings.Split(u.User.Username(), "|")[2], "accounts-v1=")
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"codex":"acct_c"}` {
		t.Fatalf("selector payload = %s", data)
	}
}

func TestProxyEnvironmentReplacesOnlyProxyVariables(t *testing.T) {
	original := []string{"HTTP_PROXY=old", "http_proxy=old-lower", "HTTPS_PROXY=old-secure", "https_proxy=old-secure-lower", "NO_PROXY=localhost", "SSL_CERT_FILE=/ca.pem", "HTTP_PROXY=duplicate"}
	got := proxyEnvironment(original, "http://subject%7Cbox:pass@proxy.example")
	want := []string{"NO_PROXY=localhost", "SSL_CERT_FILE=/ca.pem", "HTTP_PROXY=http://subject%7Cbox:pass@proxy.example", "http_proxy=http://subject%7Cbox:pass@proxy.example", "HTTPS_PROXY=http://subject%7Cbox:pass@proxy.example", "https_proxy=http://subject%7Cbox:pass@proxy.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proxy environment = %#v, want %#v", got, want)
	}
	if len(original) != 7 {
		t.Fatalf("input environment was changed: %#v", original)
	}
}

func TestAccountProxyErrorsDoNotExposeInputURL(t *testing.T) {
	secretURL := "http://subject:secret-password@proxy.example"
	if err := ValidateAccountProxyURL(secretURL); err == nil || strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("validation error exposes proxy input: %v", err)
	}
}
