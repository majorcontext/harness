package backend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

type accountRunSink struct{}

func (accountRunSink) Item(protocol.LogMessage) error            { return nil }
func (accountRunSink) Delta(string, turn.Delta)                  {}
func (accountRunSink) Discard(string)                            {}
func (accountRunSink) Alive()                                    {}
func (accountRunSink) Telemetry(turn.Telemetry)                  {}
func (accountRunSink) Steer() ([]protocol.LogMessage, error)     { return nil, nil }
func (accountRunSink) State(string) (turn.Snapshot, error)       { return turn.Snapshot{}, nil }
func (accountRunSink) SaveState(string, turn.Snapshot) error     { return nil }
func (accountRunSink) Compacted(string) error                    { return nil }
func (accountRunSink) Status(protocol.StatusFrame)               {}
func (accountRunSink) Ask(string, string, json.RawMessage) error { return nil }
func (accountRunSink) Resolution(string) (protocol.RequestResolved, bool) {
	return protocol.RequestResolved{}, false
}
func (accountRunSink) Asked(string) (json.RawMessage, bool) { return nil, false }

type fakeAccountBackend struct {
	id string
	rt http.RoundTripper
}

func (f *fakeAccountBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (f *fakeAccountBackend) Run(_ context.Context, _ turn.Request, out turn.Sink) error {
	out.Telemetry(turn.Telemetry{SubscriptionUsage: &protocol.SubscriptionUsage{Provider: "codex", Plan: f.id}})
	return nil
}

func (f *fakeAccountBackend) Models(context.Context) []modelmeta.Model {
	return []modelmeta.Model{{ID: "model-of-" + f.id}}
}

type telemetrySink struct {
	turn.Sink
	got []turn.Telemetry
}

func (s *telemetrySink) Telemetry(t turn.Telemetry) { s.got = append(s.got, t) }

// accountFixture returns an accountBackend of vendor "v" whose proxy is a test server. seen receives the Proxy-Authorization
// header of each request that reaches the proxy.
func accountFixture(t *testing.T, noProxy string, routing *config.AccountRouting) (a *accountBackend, built *atomic.Int32, seen chan string) {
	t.Helper()
	seen = make(chan string, 8)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Proxy-Authorization")
	}))
	t.Cleanup(proxy.Close)
	env := map[string]string{"ACCT_PROXY": "http://base:pw@" + strings.TrimPrefix(proxy.URL, "http://"), "NO_PROXY": noProxy}
	if routing != nil && routing.ProxyURLEnv == "" {
		routing = &config.AccountRouting{ProxyURLEnv: "ACCT_PROXY", Separator: ":"}
	}
	built = new(atomic.Int32)
	a = &accountBackend{base: &fakeAccountBackend{id: "base"}, name: "p", vendor: "v", routing: routing,
		getenv:    func(k string) string { return env[k] },
		transport: func() http.RoundTripper { return nil },
		build: func(tr func(string) http.RoundTripper) turn.Backend {
			built.Add(1)
			return &fakeAccountBackend{id: "built", rt: tr("p")}
		}}
	return a, built, seen
}

var anyRouting = &config.AccountRouting{}

func requestOf(account string) turn.Request {
	return turn.Request{Settings: protocol.Settings{Accounts: map[string]string{"v": account}}}
}

func TestCodexRequestsUseOnlyTheSelectedSessionProxyAccount(t *testing.T) {
	requests := new(atomic.Int32)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer model.Close()
	auth := make(chan string, 2)
	direct := &http.Client{Transport: &http.Transport{Proxy: nil}}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth <- r.Header.Get("Proxy-Authorization")
		r.Header.Del("Proxy-Authorization")
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(model.URL, "http://")
		r.Host, r.RequestURI = r.URL.Host, ""
		resp, err := direct.Do(r)
		if err != nil {
			http.Error(w, "upstream failed", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(k, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	proxyURL := "http://subject%7Cbox:password@" + strings.TrimPrefix(proxy.URL, "http://")
	env := map[string]string{"KEY": "key", "ACCOUNT_PROXY": proxyURL}
	base := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "api.test:80" {
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(model.URL, "http://"))
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	r := New(config.Config{Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, BaseURL: "http://api.test", APIKeyEnv: "KEY", Vendor: "codex"}},
		AccountRouting: &config.AccountRouting{ProxyURLEnv: "ACCOUNT_PROXY", Separator: "|"}}, "", func(string) http.RoundTripper { return base }, Env{Getenv: func(k string) string { return env[k] }})
	defer r.Close()
	for _, accounts := range []map[string]string{nil, {"codex": "acct_7"}} {
		req := turn.Request{SessionID: "session", Model: "codex/gpt-5", Settings: protocol.Settings{Accounts: accounts},
			History: []protocol.LogMessage{{Role: protocol.RoleUser, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: "hi"}}}}}
		if err := r.Run(context.Background(), req, accountRunSink{}); err != nil {
			t.Fatalf("Run with accounts %v: %v", accounts, err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("model requests = %d, want 2", requests.Load())
	}
	got := <-auth
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "Basic "))
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "subject|box|acct_7:password" {
		t.Fatalf("proxy credentials = %q", decoded)
	}
	select {
	case extra := <-auth:
		t.Fatalf("parent request unexpectedly used proxy credentials %q", extra)
	default:
	}
}

func TestAccountBackendQualifiesTheProxyUsernameOfTheSelectedAccount(t *testing.T) {
	a, _, seen := accountFixture(t, "", anyRouting)
	be, err := a.of(requestOf("acct_7"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://upstream.test/v1", nil)
	resp, err := be.(*fakeAccountBackend).rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(<-seen, "Basic "))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "base:acct_7:pw" {
		t.Fatalf("proxy credentials = %q, want base:acct_7:pw", got)
	}
}

func TestAccountBackendFailsPermanentlyWhenNoProxyListsTheTarget(t *testing.T) {
	a, _, seen := accountFixture(t, "upstream.test", anyRouting)
	be, err := a.of(requestOf("acct_7"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://upstream.test/v1", nil)
	_, err = be.(*fakeAccountBackend).rt.RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "would bypass the proxy") {
		t.Fatalf("error = %v, want a bypass error", err)
	}
	if !provider.DeclaresPermanent(err) {
		t.Fatalf("error %v is retryable", err)
	}
	if len(seen) != 0 {
		t.Fatal("a request reached the proxy")
	}
}

func TestAccountBackendKeepsOneBackendForEachAccount(t *testing.T) {
	a, built, _ := accountFixture(t, "", anyRouting)
	first, _ := a.of(requestOf("a1"))
	again, _ := a.of(requestOf("a1"))
	other, _ := a.of(requestOf("a2"))
	base, _ := a.of(requestOf(""))
	if first != again || first == other || base != a.base || built.Load() != 2 {
		t.Fatalf("backends: same=%v distinct=%v base=%v built=%d", first == again, first != other, base == a.base, built.Load())
	}
}

func TestAccountBackendFailsWhenTheSelectedAccountCannotBeRouted(t *testing.T) {
	tests := []struct {
		name    string
		routing *config.AccountRouting
		want    string
	}{
		{name: "no routing", want: "has no routing"},
		{name: "proxy variable unset", routing: &config.AccountRouting{ProxyURLEnv: "MISSING", Separator: ":"}, want: "MISSING is empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, built, _ := accountFixture(t, "", tc.routing)
			err := a.Run(context.Background(), requestOf("acct_7"), &telemetrySink{})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !provider.DeclaresPermanent(err) {
				t.Fatalf("error = %v, want a permanent error with %q", err, tc.want)
			}
			if built.Load() != 0 {
				t.Fatal("a backend was built")
			}
		})
	}
}

func TestAccountBackendStampsTheAccountOnSubscriptionUsage(t *testing.T) {
	a, _, _ := accountFixture(t, "", anyRouting)
	sink := &telemetrySink{}
	if err := a.Run(context.Background(), requestOf("acct_7"), sink); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background(), requestOf(""), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.got) != 2 || sink.got[0].SubscriptionUsage.Account != "acct_7" || sink.got[1].SubscriptionUsage.Account != "" {
		t.Fatalf("accounts of the usage = %+v", sink.got)
	}
}
