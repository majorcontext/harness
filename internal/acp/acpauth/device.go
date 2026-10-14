package acpauth

import (
	"context"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/codexauth"
)

// chatGPTDeviceLogin runs the device code flow and shows the verification page to the user as an elicitation in URL mode.
// It stops when the client declines or cancels the elicitation, or when ctx ends.
func (a *Auth) chatGPTDeviceLogin(ctx context.Context, cfg *config.Config, client acp.LoginClient) error {
	o := codexauth.LoginOptions{Store: store(cfg), Issuer: cfg.CodexIssuer, Out: a.opts.Out, HTTP: a.opts.HTTP}
	code, err := codexauth.StartDevice(ctx, o)
	if err != nil {
		return err
	}
	prompt, err := client.ElicitURL(ctx, code.URL(), "Sign in with ChatGPT. Open the link and, if the page asks, enter the code "+code.UserCode+". Continue only if you started this sign-in.")
	if err != nil {
		return err
	}
	defer prompt.Close()
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- code.Wait(waitCtx) }()
	select {
	case err := <-done:
		if err == nil {
			prompt.Complete()
		}
		return err
	case err := <-prompt.Refused():
		cancel()
		<-done
		return err
	}
}
