//go:build e2e

package workload

import (
	"context"
	"os/exec"
	"testing"
)

// runShell runs cmd via `sh -c` and fails the test loudly on error, logging
// combined output either way. It is the escape hatch a few rows use to
// invoke infra-owned chaos/rotation hooks named by an env var — this repo
// never hardcodes what those hooks do, only that they exist and how to
// call them (see row4's ROTATE_LOGS_CMD and row6's BOXES_CHAOS_KILL_CMD).
func runShell(t *testing.T, ctx context.Context, cmd string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	t.Logf("shell hook %q output:\n%s", cmd, out)
	if err != nil {
		t.Fatalf("shell hook %q failed: %v", cmd, err)
	}
}
