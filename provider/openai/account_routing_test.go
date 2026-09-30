package openai

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/majorcontext/harness/provider"
)

type accountRouteProxy struct {
	*httptest.Server
	authorizations chan string
	upgrades       atomic.Int32
}

func newAccountRouteProxy(t *testing.T) *accountRouteProxy {
	t.Helper()
	proxy := &accountRouteProxy{authorizations: make(chan string, 4)}
	proxy.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.authorizations <- r.Header.Get("Proxy-Authorization")
		if r.Header.Get("Upgrade") == "" {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, frame := range wsCannedFrames {
				_, _ = w.Write([]byte(sse(wsFrameEventName(frame), frame)))
			}
			return
		}
		proxy.upgrades.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusInternalError, "")
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		for _, frame := range wsCannedFrames {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(frame)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func TestAccountRoutingProductionEntrypoints(t *testing.T) {
	for _, transport := range []string{"http", "ws_stream", "ws_prewarm"} {
		t.Run(transport, func(t *testing.T) {
			client := &Client{APIKey: "placeholder", BaseURL: "http://codex.invalid", Family: CodexFamily, UseWebSocketTransport: transport != "http"}
			for _, account := range []string{"acct_a", "acct_b"} {
				proxy := newAccountRouteProxy(t)
				selector := base64.RawURLEncoding.EncodeToString([]byte(`{"codex":"` + account + `"}`))
				username := "subject|box|accounts-v1=" + selector
				proxyURL, err := url.Parse(proxy.URL)
				if err != nil {
					t.Fatal(err)
				}
				proxyURL.User = url.UserPassword(username, "password-"+account)
				ctx := provider.WithAccountRouting(context.Background(), provider.AccountRouting{ProxyURL: proxyURL.String()})
				req := wsRequest("same-session")
				if transport == "ws_prewarm" {
					err = client.Prewarm(ctx, req)
				} else {
					var stream provider.Stream
					stream, err = client.Stream(ctx, req)
					if err == nil {
						_ = collect(t, stream)
						_ = stream.Close()
					}
				}
				if err != nil {
					t.Fatalf("%s account %s: %v", transport, account, err)
				}
				wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":password-"+account))
				if got := <-proxy.authorizations; got != wantAuth {
					t.Fatalf("proxy authorization for %s = %q, want selected account auth", account, got)
				}
				if transport != "http" && proxy.upgrades.Load() != 1 {
					t.Fatalf("websocket upgrades for %s = %d, want 1", account, proxy.upgrades.Load())
				}
			}
		})
	}
}

func TestAccountRoutingIsolatesOpenAIHTTPAndWebSocketSessions(t *testing.T) {
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.TLSClientConfig = &tls.Config{ServerName: "api.example"}
	baseClient := &http.Client{Transport: baseTransport, Timeout: 3 * time.Second}
	client := &Client{HTTPClient: baseClient, BaseURL: "http://codex.invalid", UseWebSocketTransport: true}
	request := &provider.Request{SessionKey: "child-session"}
	ctxA := provider.WithAccountRouting(context.Background(), provider.AccountRouting{ProxyURL: "http://subject%7Cbox%7Caccounts-v1=YQ:password@proxy.example"})
	ctxB := provider.WithAccountRouting(context.Background(), provider.AccountRouting{ProxyURL: "http://subject%7Cbox%7Caccounts-v1=Yg:password@proxy.example"})

	httpA, keyA, err := client.httpClientForAccount(ctxA, request)
	if err != nil {
		t.Fatal(err)
	}
	httpB, keyB, err := client.httpClientForAccount(ctxB, request)
	if err != nil {
		t.Fatal(err)
	}
	if httpA == httpB || httpA.Transport == httpB.Transport {
		t.Fatal("sibling account selections share an HTTP client or transport")
	}
	if httpA.Timeout != baseClient.Timeout || httpB.Timeout != baseClient.Timeout {
		t.Fatal("account client did not preserve timeout")
	}
	if got := httpA.Transport.(*http.Transport).TLSClientConfig.ServerName; got != "api.example" {
		t.Fatalf("cloned transport TLS server name = %q", got)
	}
	if keyA == keyB {
		t.Fatal("sibling account selections share a session pool key")
	}
	pool := client.wsPoolFor()
	if pool.entryFor(keyA) == pool.entryFor(keyB) {
		t.Fatal("sibling account selections share a websocket pool entry")
	}
}

func TestAccountRoutingPreservesNoProxyAndRejectsBypassingRedirect(t *testing.T) {
	t.Setenv("NO_PROXY", "example.com")
	client := &Client{}
	ctx := provider.WithAccountRouting(context.Background(), provider.AccountRouting{ProxyURL: "http://subject%7Cbox:password@proxy.example"})
	httpClient, _, err := client.httpClientForAccount(ctx, &provider.Request{SessionKey: "session"})
	if err != nil {
		t.Fatal(err)
	}
	transport := httpClient.Transport.(*http.Transport)
	direct := httptest.NewRequest(http.MethodGet, "https://api.example.com/path", nil)
	if proxy, err := transport.Proxy(direct); proxy != nil || err == nil || !strings.Contains(err.Error(), "excluded by NO_PROXY") {
		t.Fatalf("NO_PROXY destination proxy = (%v, %v), want a closed error", proxy, err)
	}
	proxied := httptest.NewRequest(http.MethodGet, "https://api.other.test/path", nil)
	proxy, err := transport.Proxy(proxied)
	if err != nil || proxy == nil || proxy.Host != "proxy.example" {
		t.Fatalf("account destination proxy = (%v, %v)", proxy, err)
	}
	redirect := httpClient.CheckRedirect
	if err := redirect(direct, []*http.Request{proxied}); err == nil || !strings.Contains(err.Error(), "would bypass the proxy") {
		t.Fatalf("account-routed redirect error = %v", err)
	}
	for _, tc := range []struct {
		pattern string
		target  string
	}{
		{pattern: ".example.com", target: "https://api.example.com"},
		{pattern: "*.example.com", target: "https://api.example.com"},
		{pattern: "::1", target: "http://[::1]:8080"},
		{pattern: "10.0.0.0/8", target: "http://10.2.3.4"},
		{pattern: "api.other.test:8443", target: "https://api.other.test:8443"},
	} {
		t.Setenv("NO_PROXY", tc.pattern)
		if err := provider.ValidateAccountRoutingTarget("http://subject%7Cbox:password@proxy.example", tc.target); err == nil {
			t.Errorf("NO_PROXY %q did not reject %q", tc.pattern, tc.target)
		}
	}
}

func TestAccountHTTPClientCacheIsBoundedAndRouteSensitive(t *testing.T) {
	client := &Client{BaseURL: "http://codex.invalid"}
	request := &provider.Request{SessionKey: "same-session"}
	firstKey := ""
	lastKey := ""
	for i := 0; i < accountHTTPClientCacheLimit+1; i++ {
		proxyURL := fmt.Sprintf("http://subject%%7Cbox:password%d@proxy.example", i)
		ctx := provider.WithAccountRouting(context.Background(), provider.AccountRouting{ProxyURL: proxyURL})
		_, key, err := client.httpClientForAccount(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstKey = key
		}
		lastKey = key
	}
	if firstKey == lastKey {
		t.Fatal("changed proxy credentials reused a route key")
	}
	client.accountHTTPClientsMu.Lock()
	defer client.accountHTTPClientsMu.Unlock()
	if len(client.accountHTTPClients) != accountHTTPClientCacheLimit {
		t.Fatalf("account HTTP client cache has %d entries, want limit %d", len(client.accountHTTPClients), accountHTTPClientCacheLimit)
	}
	if _, ok := client.accountHTTPClients[firstKey]; ok {
		t.Fatal("oldest account HTTP client was not evicted")
	}
}

func TestHTTPClientUsesDefaultWhenNoAccountRoute(t *testing.T) {
	client := &Client{}
	got, key, err := client.httpClientForAccount(context.Background(), &provider.Request{SessionKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if got != http.DefaultClient || key != "s" {
		t.Fatalf("default route = (%p, %q), want (%p, %q)", got, key, http.DefaultClient, "s")
	}
}
