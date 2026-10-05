package e2e

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestContractRuntimeSessionSync(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name, setting, want string
	}{
		{"session_sync_default_reports_fsync", "", "fsync"},
		{"session_sync_fsync_reports_fsync", "fsync", "fsync"},
		{"session_sync_volume_is_reported_in_the_engine_status", "volume", "volume"},
	}
	onHosts(t, func(t *testing.T, h host) {
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				cfg := map[string]any{}
				if row.setting != "" {
					cfg["session_sync"] = row.setting
				}
				d, fake := startOn(t, h, runtimeWorkdir(t, nil), cfg, replyText("ok"))
				runTurn(t, d, "go")
				if got := fake.Requests()[0].LastUserText(); !strings.Contains(got, " · session_sync="+row.want+" · ") {
					t.Errorf("engine status line = %q, want session_sync=%s", got, row.want)
				}
			})
		}
	})
}

func TestContractRuntimeSessionSyncUnknownValue(t *testing.T) {
	skipShort(t)
	cfgPath := writeGoalConfigWith(t, "http://127.0.0.1:1", map[string]any{"session_sync": "bogus"})
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	cmd := exec.CommandContext(ctx, harnessBin, "serve", "-addr", freeAddr(t))
	cmd.Dir = t.TempDir()
	cmd.Env = cleanEnv(map[string]string{
		"HARNESS_RUN_TOKEN":   "e2e-run-token",
		"HARNESS_SESSION_DIR": t.TempDir(),
		"HARNESS_CONFIG":      cfgPath,
		"ANTHROPIC_API_KEY":   "e2e-dummy-key",
	})
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("serve with session_sync bogus = %v, want a non-zero exit\n%s", err, out)
	}
	if want := `session_sync: unknown value "bogus"`; !strings.Contains(string(out), want) {
		t.Errorf("serve output does not name the bad value, want %q in:\n%s", want, out)
	}
}
