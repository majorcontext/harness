// Package acpauth implements the Auth interface of internal/acp for the two
// logins that Harness offers: the ChatGPT subscription login of the codex
// provider entries, and the login of the Claude Code CLI. Harness reads no
// Claude Code credential; it asks the CLI.
package acpauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/codexauth"
	"github.com/majorcontext/harness/internal/process"
)

// Method ids.
const (
	chatGPT     = "chatgpt"
	chatGPTTerm = "chatgpt-terminal"
	claudeCode  = "claude-code"
	claudeTerm  = "claude-code-terminal"
)

const (
	statusTTL    = 30 * time.Second
	statusBudget = 15 * time.Second
	// loginBudget bounds a sign-in, as the browser sign-in of codexauth bounds itself.
	loginBudget = 10 * time.Minute
	// killGrace is the time that a killed child gets to close its pipes.
	killGrace = 2 * time.Second
)

// Options holds what the host injects.
type Options struct {
	// Config returns the effective config of a work directory. An empty cwd asks for the config of the work directory of the process, which is what initialize advertises from.
	Config func(cwd string) (*config.Config, error)
	// Open opens the sign-in URL of the ChatGPT login in a browser. Nil opens none.
	Open func(url string) error
	// Out receives the sign-in instructions and the output of the Claude Code CLI. Nil discards them.
	Out io.Writer
	// Env is the environment of the Claude Code CLI. Nil is the environment of the process.
	Env []string
	// LookPath resolves the program of the Claude Code CLI, as exec.LookPath does. Nil is exec.LookPath.
	LookPath func(file string) (string, error)
}

// Auth implements acp.Auth.
type Auth struct {
	opts Options

	mu     sync.Mutex
	status map[string]claudeStatus
	gen    int
	stop   context.CancelFunc
	// running serializes the sign-ins.
	running sync.Mutex
}

type claudeStatus struct {
	at       time.Time
	loggedIn bool
}

// New returns an Auth that reads config only when the adapter asks.
func New(opts Options) *Auth {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	return &Auth{opts: opts, status: map[string]claudeStatus{}}
}

// target is what the user config offers a login for.
type target struct {
	chatgpt bool
	claude  string // the CLI binary, empty when no entry uses the CLI
}

// targets offers the ChatGPT login always, because the native codex provider needs no entry. It offers the Claude Code login when the program resolves on PATH, which starts no process.
func (a *Auth) targets(cfg *config.Config) target {
	t := target{chatgpt: true}
	if bin, ok := cfg.ClaudeCLI(); ok {
		if _, err := a.opts.LookPath(bin); err == nil {
			t.claude = bin
		}
	}
	return t
}

func (a *Auth) userTargets() target {
	cfg, err := a.opts.Config("")
	if err != nil || cfg == nil {
		return target{}
	}
	return a.targets(cfg)
}

// Methods implements acp.Auth.
func (a *Auth) Methods(terminal bool) []acp.Method {
	t := a.userTargets()
	var out []acp.Method
	if t.chatgpt {
		out = append(out, acp.Method{ID: chatGPT, Name: "Log in with ChatGPT", Description: "Open your browser and sign in with your ChatGPT subscription.", Logout: true})
		if terminal {
			out = append(out, acp.Method{ID: chatGPTTerm, Name: "Log in with ChatGPT in a terminal", Description: "Run harness login codex in a terminal.", Terminal: true, Args: strings.Fields("login codex")})
		}
	}
	if t.claude != "" {
		out = append(out, acp.Method{ID: claudeCode, Name: "Log in with Claude Code", Description: "Sign in with the Claude Code CLI. The CLI opens your browser."})
		if terminal {
			out = append(out, acp.Method{ID: claudeTerm, Name: "Log in with Claude Code in a terminal", Description: "Run harness login claude-code in a terminal.", Terminal: true, Args: strings.Fields("login claude-code")})
		}
	}
	return out
}

func store(cfg *config.Config) codexauth.Store {
	return codexauth.Store{Path: cfg.CodexAuthFile}
}

// logins runs the login of each agent method.
var logins = map[string]func(a *Auth, ctx context.Context, cfg *config.Config) error{
	chatGPT:    (*Auth).chatGPTLogin,
	claudeCode: (*Auth).claudeLogin,
}

// Authenticate implements acp.Auth. A newer call cancels the one that runs, whatever its method, and waits for it to end.
// The cancelled call fails with context.Canceled.
func (a *Auth) Authenticate(ctx context.Context, id string) error {
	login, ok := logins[id]
	if !ok {
		return fmt.Errorf("unknown authentication method %q", id)
	}
	cfg, err := a.opts.Config("")
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.gen++
	gen := a.gen
	if a.stop != nil {
		a.stop()
	}
	a.mu.Unlock()
	a.running.Lock()
	defer a.running.Unlock()
	ctx, cancel := context.WithTimeout(ctx, loginBudget)
	defer cancel()
	a.mu.Lock()
	if a.gen != gen {
		a.mu.Unlock()
		return context.Canceled
	}
	a.stop = cancel
	a.mu.Unlock()
	return login(a, ctx, cfg)
}

func (a *Auth) chatGPTLogin(ctx context.Context, cfg *config.Config) error {
	return codexauth.LoginBrowser(ctx, codexauth.LoginOptions{Store: store(cfg), Issuer: cfg.CodexIssuer, Out: a.opts.Out, Open: a.opts.Open})
}

// claudeCmd returns a command of the CLI whose whole process group ends with ctx.
func (a *Auth) claudeCmd(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // operator config
	cmd.Env = a.opts.Env
	process.KillOnCancel(cmd, killGrace)
	return cmd
}

func (a *Auth) claudeLogin(ctx context.Context, cfg *config.Config) error {
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

// Logout implements acp.Auth. It ends the ChatGPT login only. The Claude Code CLI owns its login.
func (a *Auth) Logout(ctx context.Context) error {
	cfg, err := a.opts.Config("")
	if err != nil {
		return err
	}
	return store(cfg).Remove(ctx)
}

// Check implements acp.Auth.
func (a *Auth) Check(ctx context.Context, cwd, model string, fresh bool) error {
	cfg, err := a.opts.Config(cwd)
	if err != nil || cfg == nil {
		return nil
	}
	provider, _, _ := strings.Cut(cfg.ResolveModel(model), "/")
	p, ok := cfg.ProviderEntries()[provider]
	switch {
	case !ok:
		return nil
	case p.UsesChatGPTLogin():
		if _, err := store(cfg).Load(); codexauth.IsLoginRequired(err) {
			return &acp.AuthRequiredError{Message: err.Error()}
		}
	case p.UsesClaudeCLI() && a.userTargets().claude != "":
		if ok, known := a.loggedIn(ctx, p.CLIBinary(), fresh); known && !ok {
			return &acp.AuthRequiredError{Message: "not logged in to Claude Code; run `harness login claude-code`"}
		}
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
	ctx, cancel := context.WithTimeout(ctx, statusBudget)
	defer cancel()
	var out bytes.Buffer
	cmd := a.claudeCmd(ctx, bin, "auth", "status", "--json")
	cmd.Stdout = &out
	_ = cmd.Run() // the CLI may exit non-zero when logged out and still print the status
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(out.Bytes(), &st) != nil || st.LoggedIn == nil {
		return false, false
	}
	a.mu.Lock()
	a.status[bin] = claudeStatus{at: time.Now(), loggedIn: *st.LoggedIn}
	a.mu.Unlock()
	return *st.LoggedIn, true
}
