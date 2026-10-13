package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/acp/acpauth"
	"github.com/majorcontext/harness/internal/acp/cwdruntimes"
	"github.com/majorcontext/harness/store"
)

// acpCmd serves ACP over stdin and stdout. stdout holds ACP frames only; the
// logger writes to stderr. `initialize` reads the user config to find the logins to offer; no runtime exists before the first session request. SIGINT and SIGTERM end it with status 0.
func acpCmd(args []string) error {
	if len(args) > 0 && (args[0] == "login" || args[0] == "logout") {
		// A client that runs a terminal auth method appends its args to `harness acp`.
		if args[0] == "login" {
			return loginCmd(args[1:])
		}
		return logoutCmd(args[1:])
	}
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := acp.Serve(ctx, acpRuntimes(logger), os.Stdin, os.Stdout, acp.Config{Version: version, Logger: logger, MCP: true, List: acpDir() != "", Auth: acpLogin()})
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

// acpLogin offers the logins of the user config through ACP. The sign-in text goes to stderr: stdout holds ACP frames only.
func acpLogin() acp.Auth {
	return acpauth.New(acpauth.Options{
		Config: func(cwd string) (*config.Config, error) {
			var cfg *config.Config
			var err error
			if cwd == "" {
				cfg, err = config.Load(userConfigPath())
			} else {
				cfg, err = config.LoadProject(userConfigPath(), cwd)
			}
			if err != nil {
				return nil, err
			}
			return cfg, applyOverrides(cfg, false, nil, nil)
		},
		Open: openBrowser,
		Out:  os.Stderr,
	})
}

// acpRuntimes wires the real config loading, session directory, and owner into
// the runtimes of the acp command. The first work directory decides the session
// directory and the owner epoch.
func acpRuntimes(logger *slog.Logger) *cwdruntimes.Runtimes {
	dir := acpDir()
	return cwdruntimes.New(cwdruntimes.Options{
		Config: func(cwd string) (*config.Config, error) {
			cfg, err := config.LoadProject(userConfigPath(), cwd)
			if err == nil {
				err = applyOverrides(cfg, false, nil, nil)
			}
			return cfg, err
		},
		Shared: func(first *config.Config) (harness.Store, harness.Owner, error) {
			dir, err := sessionDir(first.SessionDir)
			if err != nil {
				return nil, nil, err
			}
			owner, _ := replication(first)
			return store.NewDiskStore(dir), owner, nil
		},
		DefaultDir: dir,
		Version:    version,
		Getenv:     os.Getenv,
		Logger:     logger,
	})
}

// acpDir is the work directory of the process, or empty when it has none. It is
// the runtime that serves a session/list with no cwd.
func acpDir() string {
	dir, _ := os.Getwd()
	return dir
}
