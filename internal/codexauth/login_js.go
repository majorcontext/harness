//go:build js

package codexauth

import (
	"context"
	"errors"
)

// LoginBrowser fails: the browser sign-in needs a listener on a loopback port, and this platform has none.
func LoginBrowser(context.Context, LoginOptions) error {
	return errors.New("codex login: the browser sign-in needs a localhost listener; use the device code sign-in")
}
