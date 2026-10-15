package harness

import (
	"net/http"

	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/internal/injectedauth"
)

// Logins holds the sign-in state of the injected providers. One value can be shared by runtimes that serve one client.
type Logins struct{ l *injectedauth.Logins }

// NewLogins returns an empty Logins. A runtime that gets it in Options.Logins supplies its ModelTransport to the probes.
func NewLogins() *Logins {
	return &Logins{l: injectedauth.New(injectedauth.Options{Probe: backend.ProbeOf})}
}

// Owner returns the state itself, for the packages of this module that run the login of a connection.
func (l *Logins) Owner() *injectedauth.Logins {
	if l == nil {
		return nil
	}
	return l.l
}

// owner returns the state for a runtime that uses transport, or nil when l is nil.
func (l *Logins) owner(transport func(provider string) http.RoundTripper) *injectedauth.Logins {
	if l == nil {
		return nil
	}
	l.l.SetTransport(transport)
	return l.l
}
