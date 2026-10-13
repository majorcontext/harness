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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/codexauth"
)

// Method ids.
const (
	chatGPT      = "chatgpt"
	chatGPTTerm  = "chatgpt-terminal"
	claudeCode   = "claude-code"
	claudeTerm   = "claude-code-terminal"
	statusTTL    = 30 * time.Second
	statusBudget = 15 * time.Second
)

// Options holds what the host injects.
type Options struct {
	// Config returns the effective config of a work directory. An empty cwd asks for the user config alone.
	Config func(cwd string) (*config.Config, error)
	// Open opens the sign-in URL of the ChatGPT login in a browser. Nil opens none.
	Open func(url string) error
	// Out receives the sign-in instructions and the output of the Claude Code CLI. Nil discards them.
	Out io.Writer
	// Env is the environment of the Claude Code CLI. Nil is the environment of the process.
	Env []string
}

// Auth implements acp.Auth.
type Auth struct {
	opts Options

	mu     sync.Mutex
	status map[string]claudeStatus
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
	return &Auth{opts: opts, status: map[string]claudeStatus{}}
}

// target is what the user config offers a login for.
type target struct {
	chatgpt bool
	claude  string // the CLI binary, empty when no entry uses the CLI
}

func (a *Auth) targets(cfg *config.Config) target {
	var t target
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		p := cfg.Providers[name]
		t.chatgpt = t.chatgpt || p.UsesChatGPTLogin()
		if p.UsesClaudeCLI() && t.claude == "" {
			t.claude = p.CLIBinary()
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
		out = append(out, acp.Method{ID: chatGPT, Name: "Log in with ChatGPT", Description: "Open your browser and sign in with your ChatGPT subscription."})
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

func (a *Auth) store(cfg *config.Config) codexauth.Store {
	return codexauth.Store{Path: cfg.CodexAuthFile}
}

// logins runs the login of each agent method.
var logins = map[string]func(a *Auth, ctx context.Context, cfg *config.Config) error{
	chatGPT:    (*Auth).chatGPTLogin,
	claudeCode: (*Auth).claudeLogin,
}

// Authenticate implements acp.Auth.
func (a *Auth) Authenticate(ctx context.Context, id string) error {
	login, ok := logins[id]
	if !ok {
		return fmt.Errorf("unknown authentication method %q", id)
	}
	cfg, err := a.opts.Config("")
	if err != nil {
		return err
	}
	return login(a, ctx, cfg)
}

func (a *Auth) chatGPTLogin(ctx context.Context, cfg *config.Config) error {
	return codexauth.LoginBrowser(ctx, codexauth.LoginOptions{Store: a.store(cfg), Issuer: cfg.CodexIssuer, Out: a.opts.Out, Open: a.opts.Open})
}

func (a *Auth) claudeLogin(ctx context.Context, cfg *config.Config) error {
	bin := a.targets(cfg).claude
	cmd := exec.CommandContext(ctx, bin, "auth", "login") //nolint:gosec // operator config
	cmd.Env, cmd.Stdout, cmd.Stderr = a.opts.Env, a.opts.Out, a.opts.Out
	err := cmd.Run()
	a.forget(bin)
	if err != nil {
		return fmt.Errorf("claude auth login: %w", err)
	}
	if ok, known := a.loggedIn(ctx, bin, true); known && !ok {
		return errors.New("claude auth login ended without a login")
	}
	return nil
}

// Logout implements acp.Auth. It ends the ChatGPT login and the login of the Claude Code CLI, for each one that the user config offers.
func (a *Auth) Logout(ctx context.Context) error {
	cfg, err := a.opts.Config("")
	if err != nil {
		return err
	}
	t := a.targets(cfg)
	var errs []error
	if t.chatgpt {
		errs = append(errs, a.store(cfg).Remove(ctx))
	}
	if t.claude != "" {
		cmd := exec.CommandContext(ctx, t.claude, "auth", "logout") //nolint:gosec // operator config
		cmd.Env, cmd.Stdout, cmd.Stderr = a.opts.Env, a.opts.Out, a.opts.Out
		if err := cmd.Run(); err != nil {
			errs = append(errs, errors.New("claude auth logout failed"))
		}
		a.forget(t.claude)
	}
	return errors.Join(errs...)
}

// Check implements acp.Auth.
func (a *Auth) Check(ctx context.Context, cwd, model string, fresh bool) error {
	cfg, err := a.opts.Config(cwd)
	if err != nil || cfg == nil {
		return nil
	}
	provider, _, _ := strings.Cut(cfg.ResolveModel(model), "/")
	p, ok := cfg.Providers[provider]
	switch {
	case !ok:
		return nil
	case p.UsesChatGPTLogin():
		if _, err := a.store(cfg).Load(); codexauth.IsLoginRequired(err) {
			return &acp.AuthRequiredError{Message: err.Error()}
		}
	case p.UsesClaudeCLI():
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
	cmd := exec.CommandContext(ctx, bin, "auth", "status", "--json") //nolint:gosec // operator config
	cmd.Env, cmd.Stdout = a.opts.Env, &out
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
