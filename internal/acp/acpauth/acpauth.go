// Package acpauth implements the Auth interface of internal/acp for the two
// logins that Harness offers: the ChatGPT subscription login of the codex
// provider entries, and the login of the Claude Code CLI. Harness reads no
// Claude Code credential; it asks the CLI.
package acpauth

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/claudeauth"
	"github.com/majorcontext/harness/internal/codexauth"
	"github.com/majorcontext/harness/internal/injectedauth"
)

// Method ids.
const (
	chatGPT     = "chatgpt"
	chatGPTTerm = "chatgpt-terminal"
	claudeCode  = "claude-code"
	claudeTerm  = "claude-code-terminal"
	// injectedPrefix starts the id of the method of an injected provider entry.
	injectedPrefix = "injected-"
)

const (
	// loginBudget bounds a sign-in, as the browser sign-in of codexauth bounds itself.
	loginBudget = 10 * time.Minute
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
	// NoLogin says that the host has no file login: it offers no ChatGPT method, reads no login file, and owes no terminal method.
	NoLogin bool
	// LoginBudget bounds a sign-in from the start of authenticate. Zero: 10 minutes.
	LoginBudget time.Duration
	// Logins owns the login state of the injected provider entries. It carries the transport of the status probe, which is the transport of the model calls. nil: no entry is injected.
	Logins *injectedauth.Logins
	// LookPath resolves the program of the Claude Code CLI, as exec.LookPath does. Nil is exec.LookPath.
	LookPath func(file string) (string, error)
}

// Auth implements acp.Auth.
type Auth struct {
	opts Options

	mu     sync.Mutex
	status map[statusKey]claudeStatus
	gen    int
	stop   context.CancelFunc
	// running serializes the sign-ins.
	running sync.Mutex
}

// statusKey is what one cached login status answers for: a program asked from a directory.
type statusKey struct{ bin, dir string }

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
	return &Auth{opts: opts, status: map[statusKey]claudeStatus{}}
}

// target is what the user config offers a login for.
type target struct {
	// injected names the provider entries with auth injected and a login_url, sorted.
	injected []string
	chatgpt  bool
	claude   string // the CLI binary, empty when no entry uses the CLI
}

// targets offers the ChatGPT login always, because the native codex provider needs no entry. It offers the Claude Code login when the program resolves on PATH, which starts no process.
func (a *Auth) targets(cfg *config.Config, c acp.ClientAuth) target {
	t := target{chatgpt: !a.opts.NoLogin}
	if entries := cfg.ProviderEntries(); c.URL && a.opts.Logins != nil {
		for _, name := range slices.Sorted(maps.Keys(entries)) {
			if p := entries[name]; p.UsesInjectedAuth() && p.LoginURL != "" {
				t.injected = append(t.injected, name)
			}
		}
	}
	if bin, ok := cfg.ClaudeCLI(); ok {
		if _, err := a.opts.LookPath(bin); err == nil {
			t.claude = bin
		}
	}
	return t
}

func (a *Auth) userTargets(c acp.ClientAuth) target {
	cfg, err := a.opts.Config("")
	if err != nil || cfg == nil {
		return target{}
	}
	return a.targets(cfg, c)
}

// Methods implements acp.Auth.
func (a *Auth) Methods(c acp.ClientAuth) []acp.Method {
	t := a.userTargets(c)
	terminal := c.Terminal
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
	for _, name := range t.injected {
		out = append(out, acp.Method{ID: injectedPrefix + name, Name: "Log in to " + name, Description: "Sign in on the page of " + name + "."})
	}
	return out
}

func store(cfg *config.Config) codexauth.Store {
	return codexauth.Store{Path: cfg.CodexAuthFile}
}

// logins runs the login of each agent method.
var logins = map[string]func(a *Auth, ctx context.Context, cfg *config.Config, client acp.LoginClient) error{
	chatGPT:    (*Auth).chatGPTLogin,
	claudeCode: (*Auth).claudeLogin,
}

// Authenticate implements acp.Auth. A newer call cancels the one that runs, whatever its method, and waits for it to end.
// The cancelled call fails with context.Canceled.
func (a *Auth) Authenticate(ctx context.Context, id string, client acp.LoginClient) error {
	login, ok := logins[id]
	if name, found := strings.CutPrefix(id, injectedPrefix); found && !ok {
		login, ok = func(a *Auth, ctx context.Context, cfg *config.Config, client acp.LoginClient) error {
			return a.injectedLogin(ctx, cfg, name, client)
		}, true
	}
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
	ctx, cancel := context.WithTimeout(ctx, a.budget())
	defer cancel()
	a.mu.Lock()
	if a.gen != gen {
		a.mu.Unlock()
		return context.Canceled
	}
	a.stop = cancel
	a.mu.Unlock()
	return login(a, ctx, cfg, client)
}

func (a *Auth) chatGPTLogin(ctx context.Context, cfg *config.Config, _ acp.LoginClient) error {
	return codexauth.LoginBrowser(ctx, codexauth.LoginOptions{Store: store(cfg), Issuer: cfg.CodexIssuer, Out: a.opts.Out, Open: a.opts.Open})
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
func (a *Auth) Check(ctx context.Context, cwd, model string, mode acp.CheckMode) error {
	fresh := mode == acp.CheckReprobe
	cfg, err := a.opts.Config(cwd)
	if err != nil || cfg == nil {
		return nil
	}
	provider, _, _ := strings.Cut(cfg.ResolveModel(model), "/")
	p, ok := cfg.ProviderEntries()[provider]
	switch {
	case !ok:
		return nil
	case p.UsesInjectedAuth():
		return a.checkInjected(ctx, provider, p, mode)
	case mode == acp.CheckProbed:
	case p.UsesChatGPTLogin() && !a.opts.NoLogin:
		if _, err := store(cfg).Load(); codexauth.IsLoginRequired(err) {
			return &acp.AuthRequiredError{Message: err.Error()}
		}
	case p.UsesClaudeCLI() && a.userTargets(acp.ClientAuth{}).claude != "":
		if ok, known := a.loggedIn(ctx, p.CLIBinary(), cwd, fresh); known && !ok {
			return &acp.AuthRequiredError{Message: (&claudeauth.LoginRequiredError{}).Error()}
		}
	}
	return nil
}
