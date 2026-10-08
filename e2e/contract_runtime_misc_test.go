package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
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
				if got := bodyOf(t, d.call(t, http.MethodGet, "/health", nil))["session_sync"]; got != row.want {
					t.Errorf("/health session_sync = %v, want %q", got, row.want)
				}
			})
		}
	})
}

func TestContractRuntimeHealth(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name  string
		check func(t *testing.T, body map[string]any, before time.Time)
	}{
		{"health_names_the_build_and_the_start", func(t *testing.T, body map[string]any, before time.Time) {
			for _, key := range []string{"version", "vcs_revision", "vcs_time", "session_sync", "started_at", "capabilities"} {
				if _, ok := body[key]; !ok {
					t.Errorf("/health has no %s: %v", key, body)
				}
			}
			if body["session_sync"] != "fsync" {
				t.Errorf("/health session_sync = %v, want fsync", body["session_sync"])
			}
			if body["version"] == "" {
				t.Errorf("/health version is empty")
			}
			if at, err := time.Parse(time.RFC3339, fmt.Sprint(body["started_at"])); err != nil || at.Before(before) || at.After(time.Now().Add(time.Minute)) {
				t.Errorf("/health started_at = %v (%v), want the start of the host", body["started_at"], err)
			}
		}},
		{"health_reports_the_delta_row_identity_capability", func(t *testing.T, body map[string]any, _ time.Time) {
			if caps, _ := body["capabilities"].([]any); len(caps) != 1 || caps[0] != "delta_row_identity" {
				t.Errorf("/health capabilities = %v, want [delta_row_identity]", body["capabilities"])
			}
		}},
	}
	onHosts(t, func(t *testing.T, h host) {
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				before := time.Now().Add(-time.Minute)
				d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil)
				res := d.call(t, http.MethodGet, "/health", nil)
				body := bodyOf(t, res)
				if res.Status != http.StatusOK || body["status"] != "ok" {
					t.Fatalf("/health = %d %v, want 200 ok", res.Status, body)
				}
				row.check(t, body, before)
			})
		}
	})
}

func TestContractRuntimeSessionSyncUnknownValue(t *testing.T) {
	skipShort(t)
	t.Parallel()
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
