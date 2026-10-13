package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

const codexAccount = "acct-1"

// codexAuth scripts the stored Codex login of a row: the access token that the
// auth file holds, and the issuer that refreshes it.
type codexAuth struct {
	// label and lifetime make the stored access token. Empty label: no auth file.
	label    string
	lifetime time.Duration
	issuer   harnesstest.CodexIssuerOptions
}

// codexAuthRun is a started codexAuth.
type codexAuthRun struct {
	spec   codexAuth
	issuer *harnesstest.CodexIssuer
	path   string
	writes int
	tokens []string
}

func (a *codexAuthRun) write(t *testing.T, label string, lifetime time.Duration, refresh string) {
	t.Helper()
	access := harnesstest.AccessToken(label, codexAccount, lifetime)
	a.tokens = append(a.tokens, access)
	body, err := json.Marshal(map[string]string{
		"access_token":  access,
		"refresh_token": refresh,
		"account_id":    codexAccount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	a.writes++
	mtime := time.Now().Add(time.Duration(a.writes) * time.Hour)
	if err := os.Chtimes(a.path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func startCodexAuth(t *testing.T, spec codexAuth) *codexAuthRun {
	t.Helper()
	a := &codexAuthRun{spec: spec, issuer: harnesstest.NewCodexIssuer(t, spec.issuer), path: filepath.Join(t.TempDir(), "auth", "codex.json")}
	if spec.label != "" {
		a.write(t, spec.label, spec.lifetime, harnesstest.CodexInitialRefresh)
	}
	return a
}

// accepts is the bearer test of the Responses server: the token "revoked" is the one that it rejects.
func (a *codexAuthRun) accepts(token string) (string, bool) {
	label, ok := harnesstest.TokenLabel(token)
	return label, ok && label != "revoked"
}

// configure points the codex entry at the stored login.
func (a *codexAuthRun) configure(cfg map[string]any) {
	entry := cfg["providers"].(map[string]any)["codex"].(map[string]any)
	delete(entry, "api_key_env")
	entry["auth"] = "chatgpt"
	cfg["codex_auth_file"] = a.path
	cfg["codex_issuer"] = a.issuer.URL()
}

// rewriteCodexAuth replaces the stored login, as another process that signs in again does.
type rewriteCodexAuth struct{ label string }

func (rewriteCodexAuth) run(t *testing.T, _ *run) {
	t.Fatal("rewriteCodexAuth runs only under runCodexScenario")
}
func (a rewriteCodexAuth) runWire(t *testing.T, r *run, _ *harnesstest.OpenAI) {
	r.codex.write(t, a.label, time.Hour, "refresh-other")
}

// recordCodexAuth records what a row says about the login without a secret: the
// refreshes of the issuer, the stored login after the row, and whether a token
// reached the error of the turn or the log.
type recordCodexAuth struct{ as string }

func (recordCodexAuth) run(t *testing.T, _ *run) {
	t.Fatal("recordCodexAuth runs only under runCodexScenario")
}
func (a recordCodexAuth) runWire(t *testing.T, r *run, _ *harnesstest.OpenAI) {
	state := map[string]any{"refreshes": r.codex.issuer.Refreshes()}
	var stored struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Account string `json:"account_id"`
	}
	secrets := r.codex.tokens
	if raw, err := os.ReadFile(r.codex.path); err == nil && json.Unmarshal(raw, &stored) == nil {
		label, _ := harnesstest.TokenLabel(stored.Access)
		info, _ := os.Stat(r.codex.path)
		dir, _ := os.Stat(filepath.Dir(r.codex.path))
		state["stored_access"], state["stored_refresh"], state["stored_account"] = label, stored.Refresh, stored.Account
		state["file_mode"], state["dir_mode"] = info.Mode().Perm().String(), dir.Mode().Perm().String()
		secrets = append(secrets[:len(secrets):len(secrets)], stored.Access)
	}
	res := r.drv.GetSession(t, r.id(t, a.as))
	msg := ""
	if body, ok := res.Body.(map[string]any); ok {
		if lt, ok := body["last_turn"].(map[string]any); ok {
			msg, _ = lt["error"].(string)
			state["turn_stop_reason"], state["turn_cause"] = lt["stop_reason"], lt["cause"]
		}
	}
	state["error_says_login"] = strings.Contains(msg, "run `harness login codex`")
	logs := r.drv.Stderr() + msg
	leak := strings.Contains(logs, "refresh-") || strings.Contains(logs, "eyJ")
	for _, s := range secrets {
		leak = leak || strings.Contains(logs, s)
	}
	state["secret_in_error_or_log"] = leak
	r.record(t, "codex_login", a.as, callResult{Status: 200, Body: state})
}

func codexAuthRows() []codexScenario {
	step := func(user, reply string) harnesstest.Step { return codexTurnStep(reply, user, reply) }
	record := []action{recordWire{}, recordCodexAuth{as: "a"}}
	return []codexScenario{
		{
			scenario: scenario{name: "codex_login_headers_reach_the_request", model: codexHi, actions: codexSession(codexTurn("a", "hello"), record)},
			auth:     &codexAuth{label: "seed", lifetime: time.Hour},
		},
		{
			scenario:  scenario{name: "codex_login_websocket_dial_carries_the_login", model: codexHi, actions: codexSession(codexTurn("a", "hello"), record)},
			websocket: true,
			auth:      &codexAuth{label: "seed", lifetime: time.Hour},
		},
		{
			scenario: scenario{
				concurrent: true,
				name:       "codex_login_near_expiry_refreshes_once_for_concurrent_sessions",
				model:      []harnesstest.Step{step("one", "1"), step("two", "2"), step("three", "3")},
				actions:    []action{create{as: "a"}, create{as: "b"}, create{as: "c"}, submit{as: "a", text: "one"}, submit{as: "b", text: "two"}, submit{as: "c", text: "three"}, waitIdle{as: "a"}, waitIdle{as: "b"}, waitIdle{as: "c"}, recordWire{}, recordCodexAuth{as: "a"}},
			},
			auth: &codexAuth{label: "seed", lifetime: 2 * time.Minute},
		},
		{
			scenario: scenario{name: "codex_login_rotated_refresh_token_is_stored", model: codexHi, actions: codexSession(codexTurn("a", "hello"), record)},
			auth:     &codexAuth{label: "seed", lifetime: time.Minute, issuer: harnesstest.CodexIssuerOptions{Rotate: true}},
		},
		{
			scenario: scenario{name: "codex_login_rejected_token_refreshes_and_retries", model: codexHi, actions: codexSession(codexTurn("a", "hello"), record)},
			auth:     &codexAuth{label: "revoked", lifetime: time.Hour},
		},
		{
			scenario: scenario{
				name:    "codex_login_rewritten_file_is_used_by_the_next_request",
				model:   []harnesstest.Step{step("one", "1"), step("two", "2")},
				actions: codexSession(codexTurn("a", "one"), []action{rewriteCodexAuth{label: "second"}}, codexTurn("a", "two"), record),
			},
			auth: &codexAuth{label: "seed", lifetime: time.Hour},
		},
		{
			scenario: scenario{name: "codex_login_refresh_failure_fails_the_turn_with_guidance", actions: codexSession(codexTurn("a", "hello"), record)},
			auth:     &codexAuth{label: "seed", lifetime: time.Minute, issuer: harnesstest.CodexIssuerOptions{RefreshStatus: 400}},
		},
		{
			scenario: scenario{name: "codex_login_missing_fails_the_turn_with_guidance", actions: codexSession(codexTurn("a", "hello"), record)},
			auth:     &codexAuth{},
		},
	}
}
