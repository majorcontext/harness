package claudecode

import (
	"context"

	"github.com/majorcontext/harness/internal/claudeauth"
)

// checkLogin turns the failure of a turn into claudeauth.LoginRequiredError when the CLI reports that it is not logged in.
// It asks only after a failure that the caller did not cause, with the environment of the turn, so the config directory of a session mirror is the one that it asks.
func (b *Backend) checkLogin(ctx context.Context, env []string, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	if ok, known := claudeauth.LoggedIn(ctx, b.p.CLIBinary(), env); known && !ok {
		return &claudeauth.LoginRequiredError{}
	}
	return err
}
