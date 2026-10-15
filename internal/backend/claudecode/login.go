package claudecode

import (
	"context"

	"github.com/majorcontext/harness/internal/claudeauth"
)

// checkLogin turns the failure of a turn into claudeauth.LoginRequiredError when the CLI reports that it is not logged in.
// It asks only after a failure that the caller did not cause.
func (b *Backend) checkLogin(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	if ok, known := claudeauth.LoggedIn(ctx, b.p.CLIBinary(), nil); known && !ok {
		return &claudeauth.LoginRequiredError{}
	}
	return err
}
