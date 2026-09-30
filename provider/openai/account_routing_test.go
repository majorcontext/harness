package openai

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(origin.Close)
	proxy.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.authorizations <- r.Header.Get("Proxy-Authorization")
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		clientConn, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		upstream, err := net.Dial("tcp", origin.Listener.Addr().String())
		if err != nil {
			clientConn.Close()
			return
		}
		_, _ = fmt.Fprint(buffered, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := buffered.Flush(); err != nil {
			clientConn.Close()
			upstream.Close()
			return
		}
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, clientConn)
			upstream.Close()
			close(done)
		}()
		_, _ = io.Copy(clientConn, upstream)
		clientConn.Close()
		upstream.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func accountRouteHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return &http.Client{Transport: transport}
}

func TestAccountRoutingProductionEntrypoints(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	for _, transport := range []string{"http", "ws_stream", "ws_prewarm"} {
		t.Run(transport, func(t *testing.T) {
			client := &Client{HTTPClient: accountRouteHTTPClient(), APIKey: "placeholder", BaseURL: "https://chatgpt.com", Family: CodexFamily, UseWebSocketTransport: transport != "http"}
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

func TestCodexAccountRoutingRejectsHTTPAndWrongOriginBeforeProxy(t *testing.T) {
	for _, target := range []string{"http://chatgpt.com/backend-api/codex", "https://api.openai.com/v1"} {
		t.Run(target, func(t *testing.T) {
			proxy := newAccountRouteProxy(t)
			proxyURL, err := url.Parse(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			proxyURL.User = url.UserPassword("subject|box|accounts-v1=eyJjb2RleCI6ImFjY3RfYSJ9", "password")
			client := &Client{APIKey: "placeholder", BaseURL: target, Family: CodexFamily}
			ctx := provider.WithAccountRouting(context.Background(), provider.AccountRouting{ProxyURL: proxyURL.String()})
			stream, err := client.Stream(ctx, wsRequest("same-session"))
			if err == nil {
				_ = stream.Close()
				t.Fatal("unsupported Codex origin reached the proxy")
			}
			select {
			case <-proxy.authorizations:
				t.Fatal("unsupported Codex origin sent proxy authorization")
			default:
			}
		})
	}
}

func TestAccountRoutingIsolatesOpenAIHTTPAndWebSocketSessions(t *testing.T) {
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.TLSClientConfig = &tls.Config{ServerName: "api.example"}
	baseJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	baseClient := &http.Client{Transport: baseTransport, Timeout: 3 * time.Second, Jar: baseJar}
	client := &Client{HTTPClient: baseClient, BaseURL: "https://chatgpt.com", UseWebSocketTransport: true}
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
	if httpA.Jar != nil || httpB.Jar != nil || httpA.Jar == baseClient.Jar || httpB.Jar == baseClient.Jar {
		t.Fatal("routed clients share the base cookie jar")
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

func TestAccountRoutingRejectsCodexEndpointExcludedByNoProxy(t *testing.T) {
	t.Setenv("NO_PROXY", "chatgpt.com")
	client := &Client{BaseURL: "https://chatgpt.com"}
	ctx := provider.WithAccountRouting(context.Background(), provider.AccountRouting{ProxyURL: "http://subject%7Cbox:password@proxy.example"})
	if _, _, err := client.httpClientForAccount(ctx, &provider.Request{SessionKey: "session"}); err == nil {
		t.Fatal("account-routed Codex endpoint was silently excluded by NO_PROXY")
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

func TestAccountRoutingRedirectStaysOnCodexOrigin(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	client := &Client{BaseURL: "https://chatgpt.com"}
	ctx := provider.WithAccountRouting(context.Background(), provider.AccountRouting{ProxyURL: "http://subject%7Cbox:password@proxy.example"})
	httpClient, _, err := client.httpClientForAccount(ctx, &provider.Request{SessionKey: "session"})
	if err != nil {
		t.Fatal(err)
	}
	via := []*http.Request{httptest.NewRequest(http.MethodGet, "https://chatgpt.com/start", nil)}
	if err := httpClient.CheckRedirect(httptest.NewRequest(http.MethodGet, "https://chatgpt.com/next", nil), via); err != nil {
		t.Fatalf("same-origin redirect was rejected: %v", err)
	}
	if err := httpClient.CheckRedirect(httptest.NewRequest(http.MethodGet, "https://evil.example/next", nil), via); err == nil {
		t.Fatal("cross-origin redirect retained brokered account authorization")
	}
}

func TestAccountHTTPClientCacheIsBoundedAndRouteSensitive(t *testing.T) {
	client := &Client{BaseURL: "https://chatgpt.com"}
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
