package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/turn"
)

const maxAccountBackends = 16

type accountClient struct {
	backend   turn.Backend
	transport http.RoundTripper
	active    int
	used      uint64
	idle      chan struct{}
}

type accountLease struct {
	owner   *accountBackend
	client  *accountClient
	backend turn.Backend
	once    sync.Once
}

func (l *accountLease) release() {
	if l.owner == nil || l.client == nil {
		return
	}
	l.once.Do(func() { l.owner.release(l.client) })
}

// accountBackend routes calls by the account selected in each session.
// Each account owns a separate client and WebSocket pool. The idle LRU is
// bounded, and eviction never closes a client with an in-flight call.
type accountBackend struct {
	base      turn.Backend
	name      string
	vendor    string
	routing   *config.AccountRouting
	getenv    func(string) string
	build     func(transport func(string) http.RoundTripper) turn.Backend
	transport func() http.RoundTripper

	mu        sync.Mutex
	clock     uint64
	accounts  map[string]*accountClient
	closing   bool
	closeDone chan struct{}
}

func (a *accountBackend) account(account string, inFlight bool) (*accountLease, error) {
	if account == "" {
		return &accountLease{backend: a.base}, nil
	}
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return nil, permanentRouting(errors.New("account backend is closing"))
	}
	client := a.accounts[account]
	if client == nil {
		if a.routing == nil {
			a.mu.Unlock()
			return nil, permanentRouting(fmt.Errorf("account routing is not configured for vendor %q; set account_routing in the config", a.vendor))
		}
		if _, err := a.routing.ProxyURL(a.getenv, account); err != nil {
			a.mu.Unlock()
			return nil, permanentRouting(err)
		}
		rt, err := accountTransport(a.transport(), *a.routing, a.getenv, account)
		if err != nil {
			a.mu.Unlock()
			return nil, permanentRouting(err)
		}
		client = &accountClient{backend: a.build(func(string) http.RoundTripper { return rt }), transport: rt, idle: make(chan struct{})}
		if !inFlight {
			close(client.idle)
		}
		if a.accounts == nil {
			a.accounts = make(map[string]*accountClient)
		}
		a.accounts[account] = client
	}
	a.clock++
	client.used = a.clock
	if inFlight {
		if client.active == 0 {
			client.idle = make(chan struct{})
		}
		client.active++
	}
	lease := &accountLease{owner: a, client: client, backend: client.backend}
	closed := a.evictLocked(client)
	a.mu.Unlock()
	closeAccounts(closed)
	return lease, nil
}

func (a *accountBackend) evictLocked(protected *accountClient) []*accountClient {
	var closed []*accountClient
	for len(a.accounts) > maxAccountBackends {
		var key string
		var oldest *accountClient
		for id, candidate := range a.accounts {
			if candidate == protected || candidate.active != 0 {
				continue
			}
			if oldest == nil || candidate.used < oldest.used {
				key, oldest = id, candidate
			}
		}
		if oldest == nil {
			break
		}
		delete(a.accounts, key)
		closed = append(closed, oldest)
	}
	return closed
}

func (a *accountBackend) release(client *accountClient) {
	a.mu.Lock()
	if client.active > 0 {
		client.active--
		if client.active == 0 {
			close(client.idle)
		}
	}
	closed := a.evictLocked(nil)
	a.mu.Unlock()
	closeAccounts(closed)
}

func closeAccounts(clients []*accountClient) {
	for _, client := range clients {
		if c, ok := client.backend.(interface{ Close() }); ok {
			c.Close()
		}
		if c, ok := client.transport.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
}

// of returns the backend of the account of req without holding an in-flight lease.
func (a *accountBackend) backendOf(account string) (turn.Backend, error) {
	lease, err := a.account(account, false)
	if err != nil {
		return nil, err
	}
	return lease.backend, nil
}

func (a *accountBackend) Capabilities(model string) turn.Capabilities {
	return a.base.Capabilities(model)
}

// Run runs the turn on the backend of the account of the session.
func (a *accountBackend) Run(ctx context.Context, req turn.Request, out turn.Sink) error {
	account := req.Settings.Accounts[a.vendor]
	lease, err := a.account(account, true)
	if err != nil {
		return err
	}
	defer lease.release()
	if account != "" {
		out = accountSink{Sink: out, account: account}
	}
	return lease.backend.Run(ctx, req, out)
}

// accountSink stamps the account of the turn on the subscription usage of each call.
type accountSink struct {
	turn.Sink
	account string
}

func (s accountSink) Telemetry(t turn.Telemetry) {
	if t.SubscriptionUsage != nil {
		u := *t.SubscriptionUsage
		u.Account = s.account
		t.SubscriptionUsage = &u
	}
	s.Sink.Telemetry(t)
}

// CanWarm reports whether the base backend has a warm-up to run.
func (a *accountBackend) CanWarm(model string) bool { return turn.CanWarm(a.base, model) }

// Warm warms the backend of the account of the session.
func (a *accountBackend) Warm(ctx context.Context, req turn.Request) error {
	lease, err := a.account(req.Settings.Accounts[a.vendor], true)
	if err != nil {
		return err
	}
	defer lease.release()
	if w, ok := lease.backend.(turn.Warmer); ok {
		return w.Warm(ctx, req)
	}
	return nil
}

// Models lists the unqualified catalog of the provider.
func (a *accountBackend) Models(ctx context.Context) []modelmeta.Model {
	if l, ok := a.base.(interface {
		Models(ctx context.Context) []modelmeta.Model
	}); ok {
		return l.Models(ctx)
	}
	return modelmeta.Models(a.name)
}

// Close closes the connections of the base backend and every cached account.
func (a *accountBackend) Close() {
	a.mu.Lock()
	if a.closing {
		done := a.closeDone
		a.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	a.closing = true
	a.closeDone = make(chan struct{})
	done := a.closeDone
	var waits []chan struct{}
	for _, client := range a.accounts {
		if client.active != 0 {
			waits = append(waits, client.idle)
		}
	}
	a.mu.Unlock()
	for _, wait := range waits {
		<-wait
	}
	a.mu.Lock()
	clients := make([]*accountClient, 0, len(a.accounts))
	for _, client := range a.accounts {
		clients = append(clients, client)
	}
	a.accounts = nil
	a.mu.Unlock()
	closeAccounts(clients)
	if c, ok := a.base.(interface{ Close() }); ok {
		c.Close()
	}
	close(done)
}

// routingError is a routing failure that no retry resolves.
type routingError struct{ error }

func (e routingError) Unwrap() error { return e.error }

func (routingError) Permanent() bool { return true }

// permanentRouting marks err so that neither the transport retry nor the websocket fallback sends the request again.
// It returns nil when err is nil.
func permanentRouting(err error) error {
	if err == nil {
		return nil
	}
	return provider.MarkPermanent(routingError{err})
}

// accountTransport returns a copy of base that sends every request through the proxy of account.
// A request to a host that the environment would not proxy fails, and the proxy URL is read from the environment for each request.
// A nil base is the default transport. A base that is no *http.Transport has no proxy to set, so the account cannot reach it.
func accountTransport(base http.RoundTripper, routing config.AccountRouting, getenv func(string) string, account string) (http.RoundTripper, error) {
	var t *http.Transport
	switch b := base.(type) {
	case nil:
		t = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		t = b.Clone()
	default:
		return nil, errors.New("the model transport of the host is not an *http.Transport, so account routing cannot set its proxy")
	}
	t.Proxy = func(r *http.Request) (*url.URL, error) {
		noProxy := getenv("NO_PROXY")
		if noProxy == "" {
			noProxy = getenv("no_proxy")
		}
		if err := config.NoProxyListBypass(noProxy, r.URL); err != nil {
			return nil, permanentRouting(err)
		}
		u, err := routing.ProxyURL(getenv, account)
		return u, permanentRouting(err)
	}
	return t, nil
}
