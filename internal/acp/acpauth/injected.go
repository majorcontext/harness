package acpauth

import (
	"context"
	"fmt"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/injectedauth"
)

// elicitMessage is what the client shows with the sign-in URL of an injected entry.
const elicitMessage = "Sign in on the page that opens, then return here."

// budget is the time a sign-in may take. It bounds the whole authenticate, and the polling after the accept again.
func (a *Auth) budget() time.Duration {
	if a.opts.LoginBudget > 0 {
		return a.opts.LoginBudget
	}
	return loginBudget
}

// pollInterval is the wait between two probes after the user accepts the sign-in page: 2 s, 4 s after 30 s, 8 s after 2 minutes.
func pollInterval(elapsed time.Duration) time.Duration {
	switch {
	case elapsed >= 2*time.Minute:
		return 8 * time.Second
	case elapsed >= 30*time.Second:
		return 4 * time.Second
	}
	return 2 * time.Second
}

// injectedLogin sends the user to the login page of the entry and probes until the server accepts the sign-in. A server
// with no status probe cannot say so: the accept of the user then ends the login.
func (a *Auth) injectedLogin(ctx context.Context, cfg *config.Config, name string, client acp.LoginClient) error {
	p := cfg.ProviderEntries()[name]
	entry := a.opts.Logins.Of(name, p)
	if entry == nil || p.LoginURL == "" {
		return fmt.Errorf("unknown authentication method %q", injectedPrefix+name)
	}
	entry.Reset()
	state := entry.Probe(ctx, p.ExtraHeaders)
	if state == injectedauth.SignedIn {
		return nil
	}
	prompt, err := client.ElicitURL(ctx, p.LoginURL, elicitMessage)
	if err != nil {
		return err
	}
	defer prompt.Close()
	select {
	case <-prompt.Accepted():
	case err := <-prompt.Refused():
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	if state == injectedauth.NoProbe {
		prompt.Complete()
		return nil
	}
	start := a.opts.Logins.Now()
	for {
		switch entry.Probe(ctx, p.ExtraHeaders) {
		case injectedauth.SignedIn, injectedauth.NoProbe:
			prompt.Complete()
			return nil
		}
		elapsed := a.opts.Logins.Now().Sub(start)
		if elapsed >= a.budget() {
			return context.DeadlineExceeded
		}
		timer := time.NewTimer(pollInterval(elapsed))
		select {
		case <-timer.C:
		case err := <-prompt.Refused():
			timer.Stop()
			return err
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// checkInjected fails when the server has no credential for the user. A probe that fails, takes too long, or finds no probe is unknown and raises no error.
func (a *Auth) checkInjected(ctx context.Context, name string, p config.Provider, mode acp.CheckMode) error {
	entry := a.opts.Logins.Of(name, p)
	if entry == nil {
		return nil
	}
	state, cached := entry.Cached()
	if mode == acp.CheckReprobe || (!cached && mode != acp.CheckCacheOnly) {
		state = entry.Probe(ctx, p.ExtraHeaders)
	}
	if state == injectedauth.SignedOut {
		return &acp.AuthRequiredError{Message: entry.Message()}
	}
	return nil
}
