//go:build js

package acpauth

import (
	"context"
	"errors"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
)

// This platform starts no process, so it offers no Claude Code login and reads no login status.

func (a *Auth) claudeLogin(context.Context, *config.Config, acp.LoginClient) error {
	return errors.New("claude auth login needs a process, and this host starts none")
}

func (a *Auth) forget(string) {}

func (a *Auth) loggedIn(context.Context, string, bool) (ok, known bool) { return false, false }
