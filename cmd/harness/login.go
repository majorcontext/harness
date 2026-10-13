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
	"github.com/majorcontext/harness/internal/codexauth"
)

const loginUsage = "usage: harness login codex [--device] [--no-browser] [--status]\n       harness logout codex"

// defaultCodexAuth puts the Codex login beside the user config when no variable or key names it.
func defaultCodexAuth(cfg *config.Config) {
	if cfg.CodexAuthFile == "" {
		cfg.CodexAuthFile = filepath.Join(filepath.Dir(userConfigPath()), "auth", "codex.json")
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
var loginTargets = map[string]bool{"codex": true}

// codexArg checks the provider argument of login and logout.
func codexArg(args []string) ([]string, error) {
	if len(args) == 0 || !loginTargets[args[0]] {
		_, _ = fmt.Fprintln(os.Stderr, "harness: the provider is codex\n"+loginUsage)
		return nil, errUsage
	}
	return args[1:], nil
}

// loginCmd signs in to Codex with a ChatGPT account, or reports the stored login.
func loginCmd(args []string) error {
	args, err := codexArg(args)
	if err != nil {
		return err
	}
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

// logoutCmd deletes the stored Codex login.
func logoutCmd(args []string) error {
	args, err := codexArg(args)
	if err != nil {
		return err
	}
	if err := parseFlags(flag.NewFlagSet("logout codex", flag.ContinueOnError), args); err != nil {
		return err
	}
	store, _, err := codexSetup()
	if err != nil {
		return err
	}
	if err := store.Remove(); err != nil {
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
