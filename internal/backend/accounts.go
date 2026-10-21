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

// accountBackend sends each turn of a session that selected an account for the vendor of its entry through a backend of that account.
// A backend of one account has its own HTTP client and its own WebSocket pool, so no connection serves two accounts.
// A turn with no selected account runs on the base backend.
type accountBackend struct {
	base    turn.Backend
	name    string
	vendor  string
	routing *config.AccountRouting
	getenv  func(string) string
	// build returns a backend whose requests go through the proxy of the transport.
	build func(transport func(string) http.RoundTripper) turn.Backend
	// transport is the transport that the host supplies for the provider. nil: none.
	transport func() http.RoundTripper

	mu       sync.Mutex
	accounts map[string]turn.Backend
}

// of returns the backend of the account that req selects.
func (a *accountBackend) of(req turn.Request) (turn.Backend, error) {
	return a.backendOf(req.Settings.Accounts[a.vendor])
}

// backendOf returns the backend of account, or the base backend for an empty account.
func (a *accountBackend) backendOf(account string) (turn.Backend, error) {
	if account == "" {
		return a.base, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if be, ok := a.accounts[account]; ok {
		return be, nil
	}
	if a.routing == nil {
		return nil, permanentRouting(fmt.Errorf("account %q of vendor %q has no routing: set account_routing in the config", account, a.vendor))
	}
	// A proxy URL that cannot be built fails the call before any client exists.
	if _, err := a.routing.ProxyURL(a.getenv, account); err != nil {
		return nil, permanentRouting(err)
	}
	rt, err := accountTransport(a.transport(), *a.routing, a.getenv, account)
	if err != nil {
		return nil, permanentRouting(err)
	}
	be := a.build(func(string) http.RoundTripper { return rt })
	if a.accounts == nil {
		a.accounts = map[string]turn.Backend{}
	}
	a.accounts[account] = be
	return be, nil
}

func (a *accountBackend) Capabilities(model string) turn.Capabilities {
	return a.base.Capabilities(model)
}

// Run runs the turn on the backend of the account of the session.
func (a *accountBackend) Run(ctx context.Context, req turn.Request, out turn.Sink) error {
	be, err := a.of(req)
	if err != nil {
		return err
	}
	if account := req.Settings.Accounts[a.vendor]; account != "" {
		out = accountSink{Sink: out, account: account}
	}
	return be.Run(ctx, req, out)
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
	be, err := a.of(req)
	if err != nil {
		return err
	}
	if w, ok := be.(turn.Warmer); ok {
		return w.Warm(ctx, req)
	}
	return nil
}

// Models lists the models of the base backend, which reads no account, or the static models of the provider.
func (a *accountBackend) Models(ctx context.Context) []modelmeta.Model {
	if l, ok := a.base.(interface {
		Models(ctx context.Context) []modelmeta.Model
	}); ok {
		return l.Models(ctx)
	}
	return modelmeta.Models(a.name)
}

// Close closes the connections of the base backend and of every account.
func (a *accountBackend) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, be := range a.accounts {
		if c, ok := be.(interface{ Close() }); ok {
			c.Close()
		}
	}
	if c, ok := a.base.(interface{ Close() }); ok {
		c.Close()
	}
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
		if err := config.NoProxyBypass(noProxy, r.URL); err != nil {
			return nil, permanentRouting(err)
		}
		u, err := routing.ProxyURL(getenv, account)
		return u, permanentRouting(err)
	}
	return t, nil
}
