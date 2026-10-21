package backend

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

type fakeAccountBackend struct {
	id     string
	closed *atomic.Int32
}

func (f *fakeAccountBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (*fakeAccountBackend) Run(_ context.Context, _ turn.Request, out turn.Sink) error {
	out.Telemetry(turn.Telemetry{SubscriptionUsage: &protocol.SubscriptionUsage{Provider: "codex"}})
	return nil
}

func (f *fakeAccountBackend) Models(context.Context) []modelmeta.Model {
	return []modelmeta.Model{{ID: f.id}}
}

func (f *fakeAccountBackend) Close() {
	if f.closed != nil {
		f.closed.Add(1)
	}
}

func accountCacheFixture() *accountBackend {
	return &accountBackend{
		base:    &fakeAccountBackend{id: "base"},
		name:    "p",
		vendor:  "v",
		routing: &config.AccountRouting{ProxyURLEnv: "PROXY", Separator: "|"},
		getenv: func(string) string {
			return "http://base:password@proxy.test:8080"
		},
		transport: func() http.RoundTripper { return nil },
		build: func(func(string) http.RoundTripper) turn.Backend {
			return &fakeAccountBackend{id: "account", closed: new(atomic.Int32)}
		},
	}
}

func TestAccountCacheEvictsIdleClientsWithoutClosingAnInFlightClient(t *testing.T) {
	a := accountCacheFixture()
	for i := 0; i < maxAccountBackends; i++ {
		if _, err := a.backendOf(fmt.Sprintf("acct_%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	idle := a.accounts["acct_01"]
	active, err := a.account("acct_00", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.backendOf("acct_new"); err != nil {
		t.Fatal(err)
	}
	if len(a.accounts) > maxAccountBackends {
		t.Fatalf("cached accounts = %d, cap = %d", len(a.accounts), maxAccountBackends)
	}
	if idle.backend.(*fakeAccountBackend).closed.Load() != 1 {
		t.Fatal("least-recently-used idle backend stayed open")
	}
	if active.client.backend.(*fakeAccountBackend).closed.Load() != 0 {
		t.Fatal("in-flight backend was closed")
	}
	active.release()
}

func TestAccountCloseWaitsForActiveLease(t *testing.T) {
	a := accountCacheFixture()
	lease, err := a.account("acct_0", true)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		a.Close()
		close(closed)
	}()
	for {
		a.mu.Lock()
		closing := a.closing
		a.mu.Unlock()
		if closing {
			break
		}
		runtime.Gosched()
	}
	if lease.client.backend.(*fakeAccountBackend).closed.Load() != 0 {
		t.Fatal("active backend closed before its lease returned")
	}
	lease.release()
	<-closed
	if lease.client.backend.(*fakeAccountBackend).closed.Load() != 1 {
		t.Fatal("account backend stayed open after Close")
	}
}

func TestAccountClosingClosesBackendAndTransport(t *testing.T) {
	backendClosed, transportClosed := new(atomic.Int32), new(atomic.Int32)
	closeAccounts([]*accountClient{{backend: &fakeAccountBackend{closed: backendClosed}, transport: closeTrackingTransport{closed: transportClosed}}})
	if backendClosed.Load() != 1 || transportClosed.Load() != 1 {
		t.Fatalf("close counts = backend %d transport %d", backendClosed.Load(), transportClosed.Load())
	}
}

type closeTrackingTransport struct{ closed *atomic.Int32 }

func (t closeTrackingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
func (t closeTrackingTransport) CloseIdleConnections()                           { t.closed.Add(1) }
