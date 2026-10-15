// Package claudeauth asks the Claude Code CLI whether it is logged in, and types the failure of a turn that needs the login.
package claudeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"time"

	"github.com/majorcontext/harness/internal/process"
)

// LoginCommand is the command that signs in to the Claude Code CLI.
const LoginCommand = "harness login claude-code"

const (
	statusBudget = 15 * time.Second
	statusGrace  = 2 * time.Second
)

// LoginRequiredError says that the Claude Code CLI is not logged in. A retry cannot resolve it.
type LoginRequiredError struct{}

func (*LoginRequiredError) Error() string {
	return "not logged in to Claude Code; run `" + LoginCommand + "`"
}

// Permanent marks the error: the turn is not sent again.
func (*LoginRequiredError) Permanent() bool { return true }

// LoginRequired marks the error for the turn: only a new login resolves it.
func (*LoginRequiredError) LoginRequired() bool { return true }

// LoggedIn asks the CLI bin for its login status with `auth status --json`. known is false when the CLI gave no
// answer, so a missing binary or a changed output never hides the error of the CLI. A nil env is the environment
// of the process, and an empty dir is its directory; a relative bin is found from dir.
func LoggedIn(ctx context.Context, bin string, env []string, dir string) (ok, known bool) {
	ctx, cancel := context.WithTimeout(ctx, statusBudget)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "auth", "status", "--json") //nolint:gosec // operator config
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout = &out
	process.KillOnCancel(cmd, statusGrace)
	_ = cmd.Run() // the CLI may exit non-zero when logged out and still print the status
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(out.Bytes(), &st) != nil || st.LoggedIn == nil {
		return false, false
	}
	return *st.LoggedIn, true
}
