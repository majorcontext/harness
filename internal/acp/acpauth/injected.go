package acpauth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/injectedauth"
)

// elicitMessage is what the client shows with the sign-in URL of an injected entry.
const elicitMessage = "Sign in on the page that opens, then return here."

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

func (a *Auth) probeTransport(provider string) http.RoundTripper {
	if a.opts.ProbeTransport == nil {
		return nil
	}
	return a.opts.ProbeTransport(provider)
}

// injectedLogin sends the user to the login page of the entry and probes until the server accepts the sign-in.
func (a *Auth) injectedLogin(ctx context.Context, cfg *config.Config, name string, client acp.LoginClient) error {
	p := cfg.ProviderEntries()[name]
	entry := injectedauth.Of(name, p)
	if entry == nil || p.LoginURL == "" {
		return fmt.Errorf("unknown authentication method %q", injectedPrefix+name)
	}
	entry.Reset()
	probe := func() bool {
		return entry.Probe(ctx, a.probeTransport(name), p.ExtraHeaders) == injectedauth.SignedIn
	}
	if probe() {
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
	start := time.Now()
	for {
		if probe() {
			prompt.Complete()
			return nil
		}
		timer := time.NewTimer(pollInterval(time.Since(start)))
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

// checkInjected fails when the server has no credential for the user. A probe that fails or takes too long is unknown and raises no error.
func (a *Auth) checkInjected(ctx context.Context, name string, p config.Provider, mode acp.CheckMode) error {
	entry := injectedauth.Of(name, p)
	if entry == nil {
		return nil
	}
	state, cached := entry.Cached()
	if mode == acp.CheckFresh || (!cached && mode != acp.CheckCached) {
		state = entry.Probe(ctx, a.probeTransport(name), p.ExtraHeaders)
	}
	if state == injectedauth.SignedOut {
		return &acp.AuthRequiredError{Message: entry.Message()}
	}
	return nil
}
