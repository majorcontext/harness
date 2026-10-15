package harness

import "github.com/majorcontext/harness/internal/injectedauth"

// Logins holds the sign-in state of the injected providers of a host. Only the hosts of this module make one; nil makes each runtime build its own.
type Logins struct{ l *injectedauth.Logins }

// InjectedLogins wraps the sign-in state that a host shares between its runtimes and its ACP logins.
func InjectedLogins(l *injectedauth.Logins) *Logins { return &Logins{l: l} }

func (l *Logins) owner() *injectedauth.Logins {
	if l == nil {
		return nil
	}
	return l.l
}
