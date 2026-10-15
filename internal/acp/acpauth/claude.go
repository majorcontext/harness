//go:build !js

package acpauth

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/claudeauth"
	"github.com/majorcontext/harness/internal/process"
)

const (
	statusTTL = 30 * time.Second
	// killGrace is the time that a killed child gets to close its pipes.
	killGrace = 2 * time.Second
)

// claudeCmd returns a command of the CLI whose whole process group ends with ctx.
func (a *Auth) claudeCmd(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // operator config
	cmd.Env = a.opts.Env
	process.KillOnCancel(cmd, killGrace)
	return cmd
}

func (a *Auth) claudeLogin(ctx context.Context, cfg *config.Config, _ acp.LoginClient) error {
	bin, _ := cfg.ClaudeCLI()
	cmd := a.claudeCmd(ctx, bin, "auth", "login")
	cmd.Stdout, cmd.Stderr = a.opts.Out, a.opts.Out
	err := cmd.Run()
	a.forget(bin)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("claude auth login: %w", err)
	}
	if ok, known := a.loggedIn(ctx, bin, true); known && !ok {
		return errors.New("claude auth login ended without a login")
	}
	return nil
}

func (a *Auth) forget(bin string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.status, bin)
}

// loggedIn asks the CLI bin for its login status. known is false when the CLI gave no answer, so a missing
// binary or a changed output never blocks a turn: the turn then fails with the error of the CLI.
func (a *Auth) loggedIn(ctx context.Context, bin string, fresh bool) (ok, known bool) {
	a.mu.Lock()
	cached, hit := a.status[bin]
	a.mu.Unlock()
	if hit && !fresh && time.Since(cached.at) < statusTTL {
		return cached.loggedIn, true
	}
	ok, known = claudeauth.LoggedIn(ctx, bin, a.opts.Env)
	if !known {
		return false, false
	}
	a.mu.Lock()
	a.status[bin] = claudeStatus{at: time.Now(), loggedIn: ok}
	a.mu.Unlock()
	return ok, true
}
