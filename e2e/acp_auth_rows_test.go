package e2e

import (
	"bytes"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/acp/acpauth"
)

// acpLoginSpec scripts the logins of an ACP row.
type acpLoginSpec struct {
	// chatgpt adds the provider entry "codex" with auth chatgpt, and the sign-in issuer. stored writes a login file;
	// refreshRejected writes one near its end, which the issuer refuses to refresh.
	chatgpt, stored, refreshRejected bool
	// claude adds the provider entry "claude-code" over fakeclaude. claudeOut makes fakeclaude report no login.
	claude, claudeOut bool
}

// acpLoginRun is a started acpLoginSpec.
type acpLoginRun struct {
	spec  acpLoginSpec
	codex *codexAuthRun
	fake  *harnesstest.Server
	cfg   map[string]any
	// sink receives the sign-in text of the host, which is what the host writes to stderr.
	sink                   *signInSink
	claudeState, claudeLog string
	emptyPath              string
	signIn                 *url.URL
	opened                 chan string
}

// signInSink keeps everything the host wrote to stderr, and sends each line that is a sign-in URL to urls.
type signInSink struct {
	mu   sync.Mutex
	all  bytes.Buffer
	line []byte
	urls chan string
}

func (s *signInSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all.Write(p)
	s.line = append(s.line, p...)
	for {
		i := bytes.IndexByte(s.line, '\n')
		if i < 0 {
			return len(p), nil
		}
		if l := string(s.line[:i]); strings.HasPrefix(l, "http") && strings.Contains(l, "/oauth/authorize") {
			select {
			case s.urls <- l:
			default:
			}
		}
		s.line = s.line[i+1:]
	}
}

func (s *signInSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all.String()
}

func startACPLogin(t *testing.T, spec acpLoginSpec, model []harnesstest.Step) *acpLoginRun {
	t.Helper()
	l := &acpLoginRun{spec: spec, opened: make(chan string, 4)}
	l.sink = &signInSink{urls: make(chan string, 4)}
	auth := codexAuth{}
	switch {
	case spec.refreshRejected:
		auth = codexAuth{label: "seed", lifetime: time.Minute, issuer: harnesstest.CodexIssuerOptions{RefreshStatus: 400}}
	case spec.stored:
		auth = codexAuth{label: "seed", lifetime: time.Hour}
	}
	l.cfg = map[string]any{}
	if spec.chatgpt {
		l.codex = startCodexAuth(t, auth)
		o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{Credentials: l.codex.accepts, AccountID: codexAccount}, model...)
		l.fake = o.Server
		l.cfg = codexConfig(o.URL(), false, nil)
		l.codex.configure(l.cfg)
	} else {
		l.fake = harnesstest.NewAnthropic(t)
		l.cfg["providers"] = map[string]any{}
		l.cfg["model"] = "claude-code/sonnet"
	}
	if spec.claude {
		dir := t.TempDir()
		l.claudeState, l.claudeLog, l.emptyPath = filepath.Join(dir, "auth"), filepath.Join(dir, "auth.log"), filepath.Join(dir, "bin")
		if err := os.Mkdir(l.emptyPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if spec.claudeOut {
			if err := os.WriteFile(l.claudeState, []byte("out"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		l.cfg["providers"].(map[string]any)["claude-code"] = map[string]any{"type": "claude-code-cli", "binary_path": fakeClaudePath()}
	}
	if l.emptyPath == "" {
		l.emptyPath = t.TempDir()
	}
	l.cfg["goal_evaluator_model"] = l.cfg["model"]
	return l
}

// register gives the connection the placeholders of the values that differ between runs.
func (l *acpLoginRun) register(c *acpConn) {
	if l == nil {
		return
	}
	if l.codex != nil {
		c.paths[l.codex.path] = "<codex-auth-file>"
		c.paths[l.cfg["providers"].(map[string]any)["codex"].(map[string]any)["base_url"].(string)] = "<model-server>/backend-api/codex"
	}
}

func (l *acpLoginRun) config() map[string]any { return l.cfg }

func (l *acpLoginRun) claudeEnv() []string {
	return []string{"FAKE_CLAUDE_AUTH=" + l.claudeState, "FAKE_CLAUDE_AUTH_LOG=" + l.claudeLog}
}

// procEnv is the environment of the binary host. A PATH with no program makes the system opener of the browser fail, so no row opens a browser.
func (l *acpLoginRun) procEnv() map[string]string {
	return map[string]string{"FAKE_CLAUDE_AUTH": l.claudeState, "FAKE_CLAUDE_AUTH_LOG": l.claudeLog, "PATH": l.emptyPath}
}

// auth builds the login of the in-process host, with the opener replaced by one that opens nothing.
func (l *acpLoginRun) auth(env acpEnv) acp.Auth {
	if l == nil {
		return nil
	}
	return acpauth.New(acpauth.Options{
		Config: func(cwd string) (*config.Config, error) {
			if cwd == "" {
				return config.Load(env.configPath)
			}
			return config.LoadProject(env.configPath, cwd)
		},
		Open: func(u string) error {
			l.opened <- u
			return nil
		},
		Out: l.sink,
		Env: append(os.Environ(), l.claudeEnv()...),
	})
}

// checkACPAuthMethods compares the ids of authMethods with want, and the logout capability with the methods.
func checkACPAuthMethods(t *testing.T, res map[string]any, want []string) {
	t.Helper()
	var got []string
	list, _ := res["authMethods"].([]any)
	for _, m := range list {
		got = append(got, m.(map[string]any)["id"].(string))
	}
	if !slices.Equal(got, want) {
		t.Errorf("authMethods ids = %v, want %v", got, want)
	}
	caps, _ := res["agentCapabilities"].(map[string]any)
	_, hasAuth := caps["auth"]
	if hasAuth != (len(want) > 0) {
		t.Errorf("agentCapabilities.auth present = %v with %d methods", hasAuth, len(want))
	}
}

// acpAuthStart sends authenticate and keeps the request under id.
type acpAuthStart struct{ id, method string }

// acpAuthCallback waits for the sign-in URL and sends the one request of the browser to its redirect URI.
type acpAuthCallback struct{}

// acpAuthURL waits for the sign-in URL.
type acpAuthURL struct{}

// acpAuthDone waits for the response of a started authenticate. A non-zero code expects that error.
type acpAuthDone struct {
	id   string
	code int
}

// acpLogout sends logout. A non-zero code expects that error.
type acpLogout struct{ code int }

func (s acpAuthStart) run(t *testing.T, c *acpConn) {
	t.Helper()
	c.pending[s.id] = c.request("authenticate", map[string]any{"methodId": s.method})
}

func (acpAuthURL) run(t *testing.T, c *acpConn) {
	t.Helper()
	l := c.env.login
	timer := time.NewTimer(waitBound)
	defer timer.Stop()
	select {
	case raw := <-l.sink.urls:
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		l.signIn = u
	case <-timer.C:
		t.Fatalf("no sign-in URL within %s", waitBound)
	}
}

func (acpAuthCallback) run(t *testing.T, c *acpConn) {
	t.Helper()
	acpAuthURL{}.run(t, c)
	l := c.env.login
	q := l.signIn.Query()
	l.codex.issuer.ExpectChallenge(q.Get("code_challenge"))
	if code := callback(t, l.signIn, harnesstest.CodexAuthCode, q.Get("state")); code != 200 {
		t.Errorf("callback status = %d, want 200", code)
	}
}

func (s acpAuthDone) run(t *testing.T, c *acpConn) {
	t.Helper()
	f := c.response(c.pending[s.id])
	if s.code != 0 {
		c.mustBeError(f, s.code)
		return
	}
	c.mustBeResult(f, "AuthenticateResponse")
}

func (s acpLogout) run(t *testing.T, c *acpConn) {
	t.Helper()
	f := c.response(c.request("logout", map[string]any{}))
	if s.code != 0 {
		c.mustBeError(f, s.code)
		return
	}
	c.mustBeResult(f, "LogoutResponse")
}

// acpCheckNoSecret fails when a frame or the stderr of the host holds a token or the authorization code.
func acpCheckNoSecret(t *testing.T, c *acpConn) {
	t.Helper()
	var all strings.Builder
	for _, f := range c.wire {
		all.Write(f.Frame)
	}
	all.WriteString(c.env.login.sink.String())
	for _, secret := range []string{"eyJ", "refresh-", harnesstest.CodexAuthCode} {
		if strings.Contains(all.String(), secret) {
			t.Errorf("a frame or a log holds %q", secret)
		}
	}
}

func acpLoginFile(t *testing.T, c *acpConn) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(c.env.login.codex.path)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

// acpListenerClosed fails when the callback listener of the last sign-in still accepts a connection.
func acpListenerClosed(t *testing.T, c *acpConn) {
	t.Helper()
	redirect, err := url.Parse(c.env.login.signIn.Query().Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := net.Dial("tcp", redirect.Host); err == nil {
		_ = conn.Close()
		t.Errorf("the callback listener at %s still accepts", redirect.Host)
	}
}

// acpClaudeCalls returns the arguments of each call that fakeclaude took as a login command.
func acpClaudeCalls(c *acpConn) []string {
	return strings.Fields(strings.TrimSpace(readFileOrEmpty(c.env.login.claudeLog)))
}

var codexReplyHi = []harnesstest.Step{{Name: "reply", Reply: codexText("hi")}}

var acpAuthScenarios = []acpScenario{
	{
		name:  "initialize_lists_the_chatgpt_method_for_a_chatgpt_provider",
		cite:  "A provider entry with `auth` `chatgpt` offers the method `chatgpt`, \"Log in with ChatGPT\".",
		login: &acpLoginSpec{chatgpt: true},
		steps: []acpStep{acpInit{version: 1, want: 1, methods: []string{"chatgpt"}}},
	},
	{
		name:  "initialize_lists_terminal_methods_only_for_a_terminal_client",
		cite:  "When the client advertises `auth.terminal`, each method has a second method of type `terminal`, with the id of the first and the suffix `-terminal`.",
		login: &acpLoginSpec{chatgpt: true, stored: true, claude: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt", "claude-code"}},
			acpInit{version: 1, want: 1, terminal: true, methods: []string{"chatgpt", "chatgpt-terminal", "claude-code", "claude-code-terminal"}},
		},
	},
	{
		name:  "authenticate_with_a_terminal_or_unknown_method_is_invalid",
		cite:  "Any other `methodId`, a terminal method among them, is `-32602`.",
		login: &acpLoginSpec{chatgpt: true, stored: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, terminal: true, methods: []string{"chatgpt", "chatgpt-terminal"}},
			acpCall{method: "authenticate", params: map[string]any{"methodId": "chatgpt-terminal"}, code: acpInvalidParams},
			acpCall{method: "authenticate", params: map[string]any{"methodId": "nope"}, code: acpInvalidParams},
		},
	},
	{
		name:  "authenticate_chatgpt_stores_the_login",
		cite:  "The `chatgpt` method runs the browser sign-in of `harness login codex`: it opens the system browser, listens on `127.0.0.1` port 1455 or 1457, stores the login, and answers `{}`.",
		login: &acpLoginSpec{chatgpt: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt"}},
			acpAuthStart{id: "auth", method: "chatgpt"},
			acpAuthCallback{},
			acpAuthDone{id: "auth"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if _, ok := acpLoginFile(t, c); !ok {
					t.Error("authenticate stored no login")
				}
				if n := c.env.login.codex.issuer.Exchanges(); n != 1 {
					t.Errorf("code exchanges = %d, want 1", n)
				}
			}),
			acpCheck(acpCheckNoSecret),
		},
	},
	{
		name:  "new_session_without_a_chatgpt_login_is_auth_required",
		cite:  "A `session/new` whose default model, or a `session/prompt` whose session model, belongs to a provider that has no login is `-32000`, \"Authentication required\", with `data.code` `login_required`.",
		login: &acpLoginSpec{chatgpt: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt"}},
			acpNew{as: "a", code: acpAuthRequired},
		},
	},
	{
		name:  "prompt_after_the_chatgpt_login_vanished_is_auth_required",
		cite:  "A ChatGPT provider has no login when the file of `harness login codex` is missing or empty.",
		login: &acpLoginSpec{chatgpt: true, stored: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt"}},
			acpNew{as: "a"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if err := os.Remove(c.env.login.codex.path); err != nil {
					t.Fatal(err)
				}
			}),
			acpPrompt{as: "a", text: "hello", code: acpAuthRequired, dataCode: "login_required"},
		},
	},
	{
		name:  "authenticate_then_prompt_succeeds",
		cite:  "The `chatgpt` method runs the browser sign-in of `harness login codex`: it opens the system browser, listens on `127.0.0.1` port 1455 or 1457, stores the login, and answers `{}`.",
		login: &acpLoginSpec{chatgpt: true},
		model: codexReplyHi,
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt"}},
			acpNew{as: "a", code: acpAuthRequired},
			acpAuthStart{id: "auth", method: "chatgpt"},
			acpAuthCallback{},
			acpAuthDone{id: "auth"},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "hello", stop: "end_turn"},
			acpCheck(acpCheckNoSecret),
		},
	},
	{
		name:  "rejected_refresh_during_a_turn_answers_auth_required",
		cite:  "A turn that fails with the error class `login_required`, because the refresh token of the ChatGPT login was rejected, answers `session/prompt` with `-32000`, `data.code` `login_required`, and the error of the turn as the message.",
		login: &acpLoginSpec{chatgpt: true, refreshRejected: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt"}},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "hello", code: acpAuthRequired, dataCode: "login_required"},
			acpCheck(acpCheckNoSecret),
		},
	},
	{
		name:  "logout_removes_the_chatgpt_login",
		cite:  "`logout` ends every login that the config offers: it removes the stored ChatGPT login under its lock, as `harness logout codex` does, and runs `claude auth logout`.",
		login: &acpLoginSpec{chatgpt: true, stored: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt"}},
			acpLogout{},
			acpCheck(func(t *testing.T, c *acpConn) {
				if _, ok := acpLoginFile(t, c); ok {
					t.Error("logout left the login file")
				}
			}),
			acpNew{as: "a", code: acpAuthRequired},
		},
	},
	{
		name:  "cancel_request_during_authenticate_stops_the_listener",
		cite:  "`$/cancel_request` stops the sign-in, closes the listener or kills the child, and answers `-32800`.",
		login: &acpLoginSpec{chatgpt: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt"}},
			acpAuthStart{id: "auth", method: "chatgpt"},
			acpAuthURL{},
			acpCancelRequest{id: "auth"},
			acpAuthDone{id: "auth", code: acpRequestCanceled},
			acpCheck(acpListenerClosed),
			acpCheck(func(t *testing.T, c *acpConn) {
				if _, ok := acpLoginFile(t, c); ok {
					t.Error("a cancelled sign-in stored a login")
				}
			}),
		},
	},
	{
		name:  "initialize_lists_the_claude_code_method_for_a_claude_code_provider",
		cite:  "A provider entry of type `claude-code-cli` offers the method `claude-code`, \"Log in with Claude Code\".",
		login: &acpLoginSpec{claude: true},
		steps: []acpStep{acpInit{version: 1, want: 1, methods: []string{"claude-code"}}},
	},
	{
		name:  "claude_code_logged_out_session_new_is_auth_required",
		cite:  "A Claude Code provider has no login when `claude auth status --json` reports `loggedIn` false.",
		login: &acpLoginSpec{claude: true, claudeOut: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"claude-code"}},
			acpNew{as: "a", code: acpAuthRequired},
		},
	},
	{
		name:  "claude_code_prompt_after_logout_is_auth_required",
		cite:  "The result of the CLI is cached for 30 s and read again after `authenticate`, after `logout`, and after a failed turn.",
		login: &acpLoginSpec{claude: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"claude-code"}},
			acpNew{as: "a"},
			acpLogout{},
			acpPrompt{as: "a", text: "hello", code: acpAuthRequired, dataCode: "login_required"},
		},
	},
	{
		name:  "authenticate_claude_code_runs_the_cli_login_then_prompt_succeeds",
		cite:  "The `claude-code` method runs `claude auth login` as a child with no terminal, which opens the browser itself, and answers `{}` when the CLI exits with status 0 and `claude auth status --json` does not report `loggedIn` false.",
		login: &acpLoginSpec{claude: true, claudeOut: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"claude-code"}},
			acpNew{as: "a", code: acpAuthRequired},
			acpAuthStart{id: "auth", method: "claude-code"},
			acpAuthDone{id: "auth"},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "hello", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpClaudeCalls(c); !slices.Contains(got, "login") {
					t.Errorf("the CLI calls = %v, want a login", got)
				}
			}),
		},
	},
	{
		name:  "logout_ends_the_chatgpt_and_the_claude_code_login",
		cite:  "A logout of the CLI ends the login of every program that uses it.",
		login: &acpLoginSpec{chatgpt: true, stored: true, claude: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt", "claude-code"}},
			acpLogout{},
			acpCheck(func(t *testing.T, c *acpConn) {
				if _, ok := acpLoginFile(t, c); ok {
					t.Error("logout left the ChatGPT login")
				}
				b, _ := os.ReadFile(c.env.login.claudeState)
				if strings.TrimSpace(string(b)) != "out" {
					t.Errorf("the CLI login state = %q, want out", b)
				}
				if calls := acpClaudeCalls(c); !slices.Contains(calls, "logout") {
					t.Errorf("the CLI calls = %q, want auth logout", calls)
				}
			}),
		},
	},
}

func readFileOrEmpty(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

const acpAuthRequired = -32000
