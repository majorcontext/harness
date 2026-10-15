package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/claudeauth"
	"github.com/majorcontext/harness/internal/codexauth"
)

const loginUsage = "usage: harness login codex [--device] [--no-browser] [--status]\n       harness login claude-code [--console] [--sso] [--status]\n       harness logout codex\n       harness logout claude-code"

// defaultCodexAuth puts the Codex login beside $HARNESS_CONFIG (or in ~/.harness) when no variable or key names it.
func defaultCodexAuth(cfg *config.Config) {
	if cfg.CodexAuthFile == "" {
		cfg.CodexAuthFile = filepath.Join(userDir(), "auth", "codex.json")
	}
}

// codexSetup reads the user config alone and returns the login store and the issuer.
func codexSetup() (codexauth.Store, string, error) {
	cfg, err := config.Load(userConfigPath())
	if err != nil {
		return codexauth.Store{}, "", err
	}
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return codexauth.Store{}, "", err
	}
	defaultCodexAuth(cfg)
	return codexauth.Store{Path: cfg.CodexAuthFile}, cfg.CodexIssuer, nil
}

// loginTargets are the names that login and logout take.
var loginTargets = map[string]func(args []string) error{"codex": loginCodex, "claude-code": loginClaude}

// logoutTargets are the names that logout takes.
var logoutTargets = map[string]func(args []string) error{"codex": logoutCodex, "claude-code": logoutClaude}

// target runs the command of the provider argument of login and logout with the other arguments.
func target(targets map[string]func(args []string) error, args []string) error {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "harness: the provider is codex or claude-code\n"+loginUsage)
		return errUsage
	}
	run, ok := targets[args[0]]
	if !ok {
		_, _ = fmt.Fprintln(os.Stderr, "harness: the provider is codex or claude-code\n"+loginUsage)
		return errUsage
	}
	return run(args[1:])
}

// claudeBinary is the CLI of the user config. A config that does not load is an error: no other program runs in its place.
func claudeBinary() (string, error) {
	cfg, err := config.Load(userConfigPath())
	if err != nil {
		return "", err
	}
	if bin, ok := cfg.ClaudeCLI(); ok {
		return bin, nil
	}
	return config.Provider{}.CLIBinary(), nil
}

// runClaude runs the Claude Code CLI with the terminal of this process. The CLI keeps its own credentials; Harness never reads them.
func runClaude(args ...string) error {
	bin, err := claudeBinary()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // operator config
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// loginClaude signs in through `claude auth login`, or prints the status of the CLI.
func loginClaude(args []string) error {
	fs := flag.NewFlagSet("login claude-code", flag.ContinueOnError)
	console := fs.Bool("console", false, "bill the API usage of an Anthropic Console account")
	sso := fs.Bool("sso", false, "force the SSO sign-in")
	status := fs.Bool("status", false, "print the status of the login")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *status {
		return runClaude("auth", "status", "--text")
	}
	cli := []string{"auth", "login"}
	if *console {
		cli = append(cli, "--console")
	}
	if *sso {
		cli = append(cli, "--sso")
	}
	if err := runClaude(cli...); err != nil {
		return err
	}
	if claudeLoggedIn(context.Background()) {
		saveDefaultModel(config.SubscriptionClaudeModel)
	}
	return nil
}

// claudeLoggedIn asks the CLI whether the sign-in holds. A CLI that gives no answer counts as not signed in.
func claudeLoggedIn(ctx context.Context) bool {
	bin, err := claudeBinary()
	if err != nil {
		return false
	}
	ok, known := claudeauth.LoggedIn(ctx, bin, nil, "")
	return ok && known
}

// saveDefaultModel makes ref the default model of the user config when that file sets none. It never fails the login:
// a file that cannot change gets one line that says why.
func saveDefaultModel(ref string) {
	path := userConfigPath()
	switch set, err := config.SetModel(path, ref); {
	case err != nil:
		fmt.Printf("Default model not set: %v\n", err)
	case set:
		fmt.Printf("Default model set to %s in %s.\n", ref, path)
	}
}

// loginCmd runs the login of the provider that the first argument names.
func loginCmd(args []string) error { return target(loginTargets, args) }

// loginCodex signs in to Codex with a ChatGPT account, or reports the stored login.
func loginCodex(args []string) error {
	fs := flag.NewFlagSet("login codex", flag.ContinueOnError)
	device := fs.Bool("device", false, "sign in with a device code, for a machine with no browser")
	noBrowser := fs.Bool("no-browser", false, "print the sign-in URL and open no browser")
	status := fs.Bool("status", false, "print the account and the expiry of the stored login")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	store, issuer, err := codexSetup()
	if err != nil {
		return err
	}
	if *status {
		return loginStatus(store)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	o := codexauth.LoginOptions{Store: store, Issuer: issuer, Out: os.Stdout}
	if !*noBrowser {
		o.Open = openBrowser
	}
	if *device {
		err = codexauth.LoginDevice(ctx, o)
	} else {
		err = codexauth.LoginBrowser(ctx, o)
	}
	if err != nil {
		return err
	}
	fmt.Println("Signed in to Codex.")
	saveDefaultModel(config.SubscriptionCodexModel)
	fmt.Printf("Try: harness run -model %s -p \"...\"\n", config.SubscriptionCodexModel)
	return nil
}

// loginStatus prints the account, the expiry, and whether a refresh token exists. It never prints a token.
func loginStatus(store codexauth.Store) error {
	c, err := store.Load()
	if err != nil {
		return err
	}
	fmt.Println("account:", c.AccountID)
	if exp, ok := c.Expiry(); ok {
		fmt.Println("access token expires:", exp.UTC().Format(time.RFC3339))
	} else {
		fmt.Println("access token expires: unknown")
	}
	has := "no"
	if c.RefreshToken != "" {
		has = "yes"
	}
	fmt.Println("refresh token:", has)
	return nil
}

// logoutCmd runs the logout of the provider that the first argument names.
func logoutCmd(args []string) error { return target(logoutTargets, args) }

// logoutClaude runs `claude auth logout`.
func logoutClaude(args []string) error {
	if err := parseFlags(flag.NewFlagSet("logout claude-code", flag.ContinueOnError), args); err != nil {
		return err
	}
	return runClaude("auth", "logout")
}

// logoutCodex deletes the stored Codex login.
func logoutCodex(args []string) error {
	if err := parseFlags(flag.NewFlagSet("logout codex", flag.ContinueOnError), args); err != nil {
		return err
	}
	store, _, err := codexSetup()
	if err != nil {
		return err
	}
	if err := store.Remove(context.Background()); err != nil {
		return err
	}
	fmt.Println("Signed out of Codex.")
	return nil
}

// openBrowser opens the sign-in URL, which harness built from the issuer, in the default browser.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return errors.New("no browser opener")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
