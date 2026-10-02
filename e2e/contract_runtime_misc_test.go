package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

const unknownModel = "anthropic/no-such-model"

const noWindowError = `engine: no context window configured for model: unknown model "` + unknownModel +
	`": refusing to run without a context window (set context_window_tokens for this model, or context_window_required=false to allow it)`

func TestContractRuntimeSessionSync(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name, setting, want string
		onStderr            bool
	}{
		{"session_sync_default_reports_fsync", "", "fsync", false},
		{"session_sync_fsync_reports_fsync", "fsync", "fsync", false},
		{"session_sync_volume_is_reported_and_logged", "volume", "volume", true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cfg := map[string]any{}
			if row.setting != "" {
				cfg["session_sync"] = row.setting
			}
			d, fake := startRuntime(t, runtimeWorkdir(t, nil), cfg, replyText("ok"))
			health := d.call(t, http.MethodGet, "/health", nil)
			if got := bodyOf(t, health)["session_sync"]; got != row.want {
				t.Errorf("/health session_sync = %v, want %q", got, row.want)
			}
			runTurn(t, d, "go")
			if got := fake.Requests()[0].LastUserText(); !strings.Contains(got, " · session_sync="+row.want+" · ") {
				t.Errorf("engine status line = %q, want session_sync=%s", got, row.want)
			}
			if logged := strings.Contains(d.Stderr(), ", session_sync=volume"); logged != row.onStderr {
				t.Errorf("config summary logs session_sync=volume = %v, want %v", logged, row.onStderr)
			}
		})
	}
}

func TestContractRuntimeSessionSyncUnknownValue(t *testing.T) {
	skipShort(t)
	cfgPath := writeGoalConfigWith(t, "http://127.0.0.1:1", map[string]any{"session_sync": "bogus"})
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	cmd := exec.CommandContext(ctx, harnessBin, "serve", "-addr", freeAddr(t))
	cmd.Dir = t.TempDir()
	cmd.Env = cleanEnv(map[string]string{
		"HARNESS_RUN_TOKEN":   testToken,
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

func TestContractRuntimeContextWindow(t *testing.T) {
	skipShort(t)
	noWindow := map[string]any{"context_window_tokens": 0}
	start := func(t *testing.T, cfg map[string]any) *httpDriver {
		d, _ := startRuntime(t, runtimeWorkdir(t, nil), cfg, replyText("ok"))
		return d
	}
	create := func(t *testing.T, d *httpDriver, model string) callResult {
		return d.call(t, http.MethodPost, "/session", map[string]any{"model": model})
	}
	window := func(t *testing.T, res callResult) string {
		return bodyOf(t, res)["context"].(map[string]any)["window_tokens"].(json.Number).String()
	}
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"context_window_required_refuses_an_unknown_model_at_create", func(t *testing.T) {
			d := start(t, noWindow)
			res := create(t, d, unknownModel)
			if res.Status != http.StatusBadRequest || bodyOf(t, res)["error"] != noWindowError {
				t.Errorf("create = %d %v, want 400 %q", res.Status, res.Body, noWindowError)
			}
			if list, _ := d.ListSessions(t).Body.([]any); len(list) != 0 {
				t.Errorf("session list = %v, want no session from the refused create", list)
			}
		}},
		{"context_window_required_refuses_an_unknown_model_at_model_swap", func(t *testing.T) {
			d := start(t, noWindow)
			id := d.Create(t)
			res := d.SetModel(t, id, unknownModel)
			if res.Status != http.StatusBadRequest || bodyOf(t, res)["error"] != noWindowError {
				t.Errorf("swap = %d %v, want 400 %q", res.Status, res.Body, noWindowError)
			}
			if got := bodyOf(t, d.GetSession(t, id))["model"]; got != "anthropic/claude-fable-5" {
				t.Errorf("model after the refused swap = %v, want it unchanged", got)
			}
			res = d.SetModel(t, id, "nope/x")
			if res.Status != http.StatusBadRequest || bodyOf(t, res)["error"] != `provider "nope" is not configured` {
				t.Errorf("swap to an unconfigured provider = %d %v, want 400 naming the provider", res.Status, res.Body)
			}
		}},
		{"context_window_tokens_admits_an_unknown_model", func(t *testing.T) {
			d := start(t, map[string]any{"context_window_tokens": 200000})
			res := create(t, d, unknownModel)
			if res.Status != http.StatusCreated || window(t, res) != "200000" {
				t.Errorf("create = %d window %v, want 201 with the named window 200000", res.Status, window(t, res))
			}
		}},
		{"context_window_required_false_admits_an_unknown_model_without_a_window", func(t *testing.T) {
			d := start(t, map[string]any{"context_window_tokens": 0, "context_window_required": false})
			res := create(t, d, unknownModel)
			if res.Status != http.StatusCreated || window(t, res) != "0" {
				t.Errorf("create = %d window %v, want 201 with window 0", res.Status, window(t, res))
			}
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}
