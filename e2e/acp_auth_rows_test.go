package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/acp/acpauth"
	"github.com/majorcontext/harness/internal/testpoll"
)

// acpLoginSpec scripts the logins of an ACP row.
type acpLoginSpec struct {
	// chatgpt adds the provider entry "codex" with auth chatgpt, and the sign-in issuer. stored writes a login file;
	// refreshRejected writes one near its end, which the issuer refuses to refresh.
	chatgpt, stored, refreshRejected bool
	// claude adds the provider entry "claude-code" over fakeclaude. claudeOut makes fakeclaude report no login.
	claude, claudeOut bool
	// claudeHold makes `claude auth login` start a child and wait, as a login that waits for a browser does.
	claudeHold bool
}

// acpLoginRun is a started acpLoginSpec.
type acpLoginRun struct {
	spec  acpLoginSpec
	codex *codexAuthRun
	fake  *harnesstest.Server
	cfg   map[string]any
	// sink receives the sign-in text of the host, which is what the host writes to stderr.
	sink                               *signInSink
	claudeState, claudeLog, claudeHold string
	emptyPath                          string
	signIn                             *url.URL
	opened                             chan string
	heldPIDs                           []int
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
		if spec.claudeHold {
			l.claudeHold = filepath.Join(dir, "hold")
		}
		if spec.claudeOut {
			if err := os.WriteFile(l.claudeState, []byte("out"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		l.cfg["providers"].(map[string]any)["claude-code"] = map[string]any{"type": "claude-code-cli", "binary_path": fakeClaudePath(), "extra_args": []string{"--fake-env=FAKE_CLAUDE_AUTH=" + l.claudeState}}
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
	return []string{"FAKE_CLAUDE_AUTH=" + l.claudeState, "FAKE_CLAUDE_AUTH_LOG=" + l.claudeLog, "FAKE_CLAUDE_AUTH_HOLD=" + l.claudeHold}
}

// procEnv is the environment of the binary host. A PATH with no program makes the system opener of the browser fail, so no row opens a browser.
func (l *acpLoginRun) procEnv() map[string]string {
	return map[string]string{"FAKE_CLAUDE_AUTH": l.claudeState, "FAKE_CLAUDE_AUTH_LOG": l.claudeLog, "FAKE_CLAUDE_AUTH_HOLD": l.claudeHold, "PATH": l.emptyPath}
}

// auth builds the login of the in-process host, with the opener replaced by one that opens nothing.
func (l *acpLoginRun) auth(env acpEnv) acp.Auth {
	if l == nil {
		return nil
	}
	return acpauth.New(acpauth.Options{
		Config: func(cwd string) (*config.Config, error) {
			if cwd == "" {
				cwd = env.defaultDir
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
	if wantLogout := slices.Contains(want, "chatgpt"); hasAuth != wantLogout {
		t.Errorf("agentCapabilities.auth present = %v with the methods %v", hasAuth, want)
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

// acpListenerClosed fails when the callback of the last sign-in still completes it. A closed listener refuses the
// connection. Another test can own the port by then, so a connection is not a failure by itself: the valid callback
// of the stopped sign-in must complete nothing.
func acpListenerClosed(t *testing.T, c *acpConn) {
	t.Helper()
	l := c.env.login
	redirect, err := url.Parse(l.signIn.Query().Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", redirect.Host, time.Second)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	q := url.Values{"code": {harnesstest.CodexAuthCode}, "state": {l.signIn.Query().Get("state")}}
	_, _ = fmt.Fprintf(conn, "GET %s?%s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", redirect.Path, q.Encode(), redirect.Host)
	_, _ = io.Copy(io.Discard, conn)
	if n := l.codex.issuer.Exchanges(); n != 0 {
		t.Errorf("the callback of the stopped sign-in completed it: %d code exchanges", n)
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
		cite:  "A turn that fails with the error class `login_required`, because the refresh token of the ChatGPT login was rejected, answers `session/prompt` with `-32000`, `data.code` `login_required`, and the error of the turn as the message, without its leading `[class]` tag.",
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
		cite:  "`logout` removes the stored ChatGPT login under its lock, as `harness logout codex` does.",
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
		cite:  "`$/cancel_request` stops the sign-in, closes the listener or kills the process group of the child, and answers `-32800`.",
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
		cite:  "A Claude Code provider has no login when `claude auth status --json` reports `loggedIn` false, and only when `initialize` advertised the method `claude-code`",
		login: &acpLoginSpec{claude: true, claudeOut: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"claude-code"}},
			acpNew{as: "a", code: acpAuthRequired},
		},
	},
	{
		name:  "claude_code_failed_turn_with_no_login_is_auth_required",
		cite:  "The result of the CLI is cached for 30 s and read again after `authenticate` and after a failed turn.",
		login: &acpLoginSpec{claude: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"claude-code"}},
			acpNew{as: "a"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if err := os.WriteFile(c.env.login.claudeState, []byte("out"), 0o644); err != nil {
					t.Fatal(err)
				}
			}),
			acpPrompt{as: "a", text: "hello", code: acpAuthRequired, dataCode: "login_required"},
		},
	},
	{
		name:  "authenticate_claude_code_runs_the_cli_login_then_prompt_succeeds",
		cite:  "The `claude-code` method runs `claude auth login` as a child with no terminal, which opens the browser itself",
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
		name:  "logout_ends_the_chatgpt_login_and_leaves_the_claude_code_login",
		cite:  "`logout` removes the stored ChatGPT login under its lock, as `harness logout codex` does. It never runs `claude auth logout`: the CLI owns its login, and `harness logout claude-code` ends it.",
		login: &acpLoginSpec{chatgpt: true, stored: true, claude: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt", "claude-code"}},
			acpLogout{},
			acpCheck(func(t *testing.T, c *acpConn) {
				if _, ok := acpLoginFile(t, c); ok {
					t.Error("logout left the ChatGPT login")
				}
				if calls := acpClaudeCalls(c); len(calls) != 0 {
					t.Errorf("the CLI calls = %q, want none", calls)
				}
			}),
		},
	},
	{
		name:  "project_only_claude_code_provider_raises_no_auth_required_without_a_method",
		cite:  "a provider that only a project file adds, in a directory that `initialize` did not see, raises no auth-required error.",
		login: &acpLoginSpec{chatgpt: true, stored: true, claude: false, claudeOut: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt"}},
			acpCheck(func(t *testing.T, c *acpConn) {
				project := `{"model":"claude-code/sonnet","providers":{"claude-code":{"type":"claude-code-cli","binary_path":"` + fakeClaudePath() + `"}}}`
				if err := os.WriteFile(filepath.Join(c.dir("main"), ".harness.json"), []byte(project), 0o644); err != nil {
					t.Fatal(err)
				}
			}),
			acpNew{as: "a"},
		},
	},
	{
		name:  "logout_is_not_offered_for_a_claude_code_provider_alone",
		cite:  "`agentCapabilities.auth.logout` is `{}` when the method `chatgpt` is offered, and absent otherwise.",
		login: &acpLoginSpec{claude: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"claude-code"}},
			acpCall{method: "logout", params: map[string]any{}, code: acpMethodNotFound},
		},
	},
	{
		name:  "cancel_request_during_claude_authenticate_kills_the_login_process_tree",
		cite:  "`$/cancel_request` stops the sign-in, closes the listener or kills the process group of the child, and answers `-32800`.",
		login: &acpLoginSpec{claude: true, claudeOut: true, claudeHold: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"claude-code"}},
			acpAuthStart{id: "auth", method: "claude-code"},
			acpHoldPIDs{},
			acpCancelRequest{id: "auth"},
			acpAuthDone{id: "auth", code: acpRequestCanceled},
			acpCheck(acpHeldGone),
		},
	},
	{
		name:  "second_authenticate_cancels_the_one_in_flight",
		cite:  "A new `authenticate` cancels the one in flight, whatever the method, and answers `-32800` to it. Sign-ins run one at a time.",
		login: &acpLoginSpec{chatgpt: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt"}},
			acpAuthStart{id: "first", method: "chatgpt"},
			acpAuthURL{},
			acpAuthStart{id: "second", method: "chatgpt"},
			acpAuthDone{id: "first", code: acpRequestCanceled},
			acpCheck(acpListenerClosed),
			acpAuthCallback{},
			acpAuthDone{id: "second"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if _, ok := acpLoginFile(t, c); !ok {
					t.Error("the second sign-in stored no login")
				}
			}),
		},
	},
	{
		name:  "chatgpt_authenticate_cancels_a_claude_login_in_flight",
		cite:  "A new `authenticate` cancels the one in flight, whatever the method, and answers `-32800` to it. Sign-ins run one at a time.",
		login: &acpLoginSpec{chatgpt: true, claude: true, claudeOut: true, claudeHold: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"chatgpt", "claude-code"}},
			acpAuthStart{id: "claude", method: "claude-code"},
			acpHoldPIDs{},
			acpAuthStart{id: "chatgpt", method: "chatgpt"},
			acpAuthDone{id: "claude", code: acpRequestCanceled},
			acpCheck(acpHeldGone),
			acpAuthCallback{},
			acpAuthDone{id: "chatgpt"},
		},
	},
}

// acpHoldPIDs waits until the held login of fakeclaude has written the pids of its process and its child.
type acpHoldPIDs struct{}

func (acpHoldPIDs) run(t *testing.T, c *acpConn) {
	t.Helper()
	l := c.env.login
	testpoll.Until(t, waitBound, "the held login did not start", func() bool {
		b, err := os.ReadFile(l.claudeHold)
		if err != nil || !bytes.HasSuffix(b, []byte("\n")) {
			return false
		}
		l.heldPIDs = nil
		for _, f := range strings.Fields(string(b)) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				return false
			}
			l.heldPIDs = append(l.heldPIDs, pid)
		}
		return len(l.heldPIDs) == 2
	})
}

// acpHeldGone fails when the held login or its child still runs.
func acpHeldGone(t *testing.T, c *acpConn) {
	t.Helper()
	for _, pid := range c.env.login.heldPIDs {
		pid := pid
		testpoll.Until(t, waitBound, fmt.Sprintf("process %d still runs", pid), func() bool {
			return syscall.Kill(pid, 0) != nil
		})
	}
}

func readFileOrEmpty(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

const acpAuthRequired = -32000
