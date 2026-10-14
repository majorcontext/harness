package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

const codexAccount = "acct-1"

// codexSentinelKey is the OPENAI_API_KEY of a row that checks the key goes nowhere.
const codexSentinelKey = "sk-sentinel-must-not-leak"

// codexAuth scripts the stored Codex login of a row: the access token that the
// auth file holds, and the issuer that refreshes it.
type codexAuth struct {
	// label and lifetime make the stored access token. Empty label: no auth file.
	label    string
	lifetime time.Duration
	issuer   harnesstest.CodexIssuerOptions
	// readOnly makes the directory of the file refuse a write after the seed.
	readOnly bool
	// rejectAll makes the Responses server reject every token.
	rejectAll bool
	// secret is a value that no request to the issuer may carry in a header.
	secret string
}

// codexAuthRun is a started codexAuth.
type codexAuthRun struct {
	spec   codexAuth
	issuer *harnesstest.CodexIssuer
	path   string
	writes int
	tokens []string
	// configPath is the user config of the row.
	configPath string
	logout     *exec.Cmd
	logoutOut  *bytes.Buffer
}

// secrets lists every token that the issuer minted or that a row seeded.
func (a *codexAuthRun) secrets() []string {
	return append(a.issuer.Secrets(), a.tokens...)
}

// holdsSecret reports whether text holds one of secrets.
func holdsSecret(text string, secrets []string) bool {
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			return true
		}
	}
	return false
}

func (a *codexAuthRun) write(t *testing.T, label string, lifetime time.Duration, refresh string) {
	t.Helper()
	access := harnesstest.AccessToken(label, codexAccount, lifetime)
	a.tokens = append(a.tokens, access, refresh)
	if parts := strings.Split(access, "."); len(parts) == 3 {
		a.tokens = append(a.tokens, parts[1])
	}
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
	spec.issuer.Secret = spec.secret
	a := &codexAuthRun{spec: spec, issuer: harnesstest.NewCodexIssuer(t, spec.issuer), path: filepath.Join(t.TempDir(), "auth", "codex.json")}
	if spec.label != "" {
		a.write(t, spec.label, spec.lifetime, harnesstest.CodexInitialRefresh)
	}
	if spec.readOnly {
		if os.Geteuid() == 0 {
			t.Skip("a root user writes to a read-only directory")
		}
		dir := filepath.Dir(a.path)
		if err := os.WriteFile(a.path+".lock", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}
	return a
}

// accepts is the bearer test of the Responses server: the token "revoked" is the one that it rejects.
func (a *codexAuthRun) accepts(token string) (string, bool) {
	if a.spec.rejectAll {
		return "", false
	}
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

// awaitIssuerRefresh waits until a refresh grant is held at the issuer.
type awaitIssuerRefresh struct{}

func (awaitIssuerRefresh) run(t *testing.T, _ *run) {
	t.Fatal("awaitIssuerRefresh runs only under runCodexScenario")
}
func (awaitIssuerRefresh) runWire(t *testing.T, r *run, _ *harnesstest.OpenAI) {
	if !r.codex.issuer.AwaitRefresh(waitBound) {
		t.Fatalf("no refresh reached the issuer within %s", waitBound)
	}
}

// releaseIssuerRefresh lets the held refresh grant answer.
type releaseIssuerRefresh struct{}

func (releaseIssuerRefresh) run(t *testing.T, _ *run) {
	t.Fatal("releaseIssuerRefresh runs only under runCodexScenario")
}
func (releaseIssuerRefresh) runWire(_ *testing.T, r *run, _ *harnesstest.OpenAI) {
	r.codex.issuer.ReleaseRefresh()
}

// recordRedirect records how many requests reached the target of the redirect and whether one carried a credential.
type recordRedirect struct{}

func (recordRedirect) run(t *testing.T, _ *run) {
	t.Fatal("recordRedirect runs only under runCodexScenario")
}
func (recordRedirect) runWire(t *testing.T, r *run, o *harnesstest.OpenAI) {
	hits, creds := o.Redirected()
	r.record(t, "redirect", "", callResult{Status: 200, Body: map[string]any{"target_requests": hits, "target_got_credentials": creds}})
}

// recordSecret records whether any request of the Responses double or the issuer carried the secret of the row in a header.
type recordSecret struct{}

func (recordSecret) run(t *testing.T, _ *run) {
	t.Fatal("recordSecret runs only under runCodexScenario")
}
func (recordSecret) runWire(t *testing.T, r *run, o *harnesstest.OpenAI) {
	r.record(t, "secret", "", callResult{Status: 200, Body: map[string]any{"sent_to_the_model_server": o.SawSecret(), "sent_to_the_issuer": r.codex.issuer.SawSecret()}})
}

// startLogout runs `harness logout codex` on the login of the row, without waiting for it.
type startLogout struct{}

func (startLogout) run(t *testing.T, _ *run) { t.Fatal("startLogout runs only under runCodexScenario") }
func (startLogout) runWire(t *testing.T, r *run, _ *harnesstest.OpenAI) {
	cmd := exec.CommandContext(t.Context(), harnessBin, "logout", "codex")
	cmd.Env = cleanEnv(map[string]string{"HARNESS_CONFIG": r.codex.configPath})
	r.codex.logoutOut = &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = r.codex.logoutOut, r.codex.logoutOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.codex.logout = cmd
}

// awaitLogout waits for the logout and fails the row when it failed.
type awaitLogout struct{}

func (awaitLogout) run(t *testing.T, _ *run) { t.Fatal("awaitLogout runs only under runCodexScenario") }
func (awaitLogout) runWire(t *testing.T, r *run, _ *harnesstest.OpenAI) {
	if err := r.codex.logout.Wait(); err != nil {
		t.Fatalf("logout: %v\n%s", err, r.codex.logoutOut)
	}
}

// recordFallback records whether the HTTP path got a request after the websocket was rejected.
type recordFallback struct{}

func (recordFallback) run(t *testing.T, _ *run) {
	t.Fatal("recordFallback runs only under runCodexScenario")
}
func (recordFallback) runWire(t *testing.T, r *run, o *harnesstest.OpenAI) {
	ws, sse := o.Rejections()
	r.record(t, "fallback", "", callResult{Status: 200, Body: map[string]any{"websocket_rejected": ws > 0, "http_fallback_attempted": sse > 0}})
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
type recordCodexAuth struct {
	as string
	// stable leaves out the refresh count and the stored tokens, which a race between warm-up and turn can change.
	stable bool
}

func (recordCodexAuth) run(t *testing.T, _ *run) {
	t.Fatal("recordCodexAuth runs only under runCodexScenario")
}
func (a recordCodexAuth) runWire(t *testing.T, r *run, _ *harnesstest.OpenAI) {
	state := map[string]any{"refreshes": r.codex.issuer.Refreshes(), "file_exists": false}
	var stored struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Account string `json:"account_id"`
	}
	secrets := r.codex.secrets()
	if raw, err := os.ReadFile(r.codex.path); err == nil && json.Unmarshal(raw, &stored) == nil {
		label, _ := harnesstest.TokenLabel(stored.Access)
		info, _ := os.Stat(r.codex.path)
		dir, _ := os.Stat(filepath.Dir(r.codex.path))
		state["stored_access"], state["stored_refresh"], state["stored_account"] = label, stored.Refresh, stored.Account
		state["file_exists"] = true
		state["file_mode"], state["dir_mode"] = info.Mode().Perm().String(), dir.Mode().Perm().String()
		secrets = append(secrets, stored.Access, stored.Refresh)
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
	leak := holdsSecret(logs, secrets)
	state["secret_in_error_or_log"] = leak
	if a.stable {
		delete(state, "refreshes")
		delete(state, "stored_access")
		delete(state, "stored_refresh")
	}
	r.record(t, "codex_login", a.as, callResult{Status: 200, Body: state})
}

func codexAuthRows() []codexScenario {
	return slices.Concat(codexAuthLoginRows(), codexAuthFailureRows())
}

func codexAuthLoginRows() []codexScenario {
	step := func(user, reply string) harnesstest.Step { return codexTurnStep(reply, user, reply) }
	record := []action{recordWire{}, recordCodexAuth{as: "a"}}
	return []codexScenario{
		{
			scenario: scenario{name: "codex_login_models_list_the_models_of_the_backend", actions: []action{models{}}},
			opts:     harnesstest.OpenAIOptions{Models: chatgptCatalog},
			auth:     &codexAuth{label: "seed", lifetime: time.Hour},
		},
		{
			scenario:   scenario{name: "codex_login_models_list_of_the_entry_makes_no_catalog_request", actions: []action{models{}, noModelRequests{}}},
			opts:       harnesstest.OpenAIOptions{Models: chatgptCatalog},
			auth:       &codexAuth{label: "seed", lifetime: time.Hour},
			listModels: []string{"gpt-5.6-sol", "my-model"},
		},
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
	}
}

func codexAuthFailureRows() []codexScenario {
	step := func(user, reply string) harnesstest.Step { return codexTurnStep(reply, user, reply) }
	record := []action{recordWire{}, recordCodexAuth{as: "a"}}
	return []codexScenario{
		{
			scenario: scenario{
				name:  "codex_login_cancel_during_refresh_keeps_the_rotated_token",
				model: codexHi,
				actions: codexSession([]action{submit{as: "a", text: "first"}}, []action{awaitIssuerRefresh{}, interrupt{as: "a"}, awaitTurnEnd{as: "a"}, releaseIssuerRefresh{}},
					codexTurn("a", "hello"), record),
			},
			auth: &codexAuth{label: "seed", lifetime: 2 * time.Minute, issuer: harnesstest.CodexIssuerOptions{Rotate: true, HoldRefresh: true}},
		},
		{
			scenario: scenario{name: "codex_login_unsaved_refresh_stays_in_memory", model: []harnesstest.Step{step("one", "1"), step("two", "2")},
				actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), record)},
			auth: &codexAuth{label: "seed", lifetime: 2 * time.Minute, readOnly: true, issuer: harnesstest.CodexIssuerOptions{Rotate: true}},
		},
		{
			scenario: scenario{name: "codex_login_transient_refresh_failure_does_not_retry_on_every_request",
				model:   []harnesstest.Step{step("one", "1"), step("two", "2"), step("three", "3")},
				actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), codexTurn("a", "three"), record)},
			auth: &codexAuth{label: "seed", lifetime: 2 * time.Minute, issuer: harnesstest.CodexIssuerOptions{RefreshStatus: 503}},
		},
		{
			scenario: scenario{name: "codex_login_redirect_from_the_bound_url_is_refused", actions: codexSession(codexTurn("a", "hello"), []action{recordRedirect{}}, record)},
			opts:     harnesstest.OpenAIOptions{RedirectTo: "/backend-api/codex/stolen"},
			auth:     &codexAuth{label: "seed", lifetime: time.Hour},
		},
		{
			scenario: scenario{name: "codex_login_never_sends_the_openai_api_key", actions: codexSession(codexTurn("a", "hello"), []action{recordSecret{}})},
			opts:     harnesstest.OpenAIOptions{RedirectTo: "/backend-api/codex/stolen", Secret: codexSentinelKey},
			auth:     &codexAuth{label: "seed", lifetime: 2 * time.Minute, secret: codexSentinelKey},
			env:      map[string]string{"OPENAI_API_KEY": codexSentinelKey},
		},
		{
			scenario: scenario{
				name:  "codex_login_logout_during_a_refresh_leaves_no_file",
				model: codexHi,
				actions: codexSession([]action{submit{as: "a", text: "first"}}, []action{awaitIssuerRefresh{}, startLogout{}, releaseIssuerRefresh{}, waitIdle{as: "a"}, awaitLogout{}},
					[]action{recordCodexAuth{as: "a"}}),
			},
			auth: &codexAuth{label: "seed", lifetime: 2 * time.Minute, issuer: harnesstest.CodexIssuerOptions{HoldRefresh: true}},
		},
		{
			scenario:  scenario{name: "codex_login_rejected_websocket_fails_the_turn_without_the_http_fallback", actions: codexSession(codexTurn("a", "hello"), []action{recordFallback{}, recordCodexAuth{as: "a", stable: true}})},
			websocket: true,
			auth:      &codexAuth{label: "seed", lifetime: time.Hour, rejectAll: true},
		},
		{
			scenario: scenario{name: "codex_login_malformed_refresh_answer_fails_the_turn_with_guidance", actions: codexSession(codexTurn("a", "hello"), record)},
			auth:     &codexAuth{label: "seed", lifetime: time.Minute, issuer: harnesstest.CodexIssuerOptions{RefreshStatus: 200}},
		},
		{
			scenario: scenario{name: "codex_login_missing_fails_the_turn_with_guidance", actions: codexSession(codexTurn("a", "hello"), record)},
			auth:     &codexAuth{},
		},
	}
}
