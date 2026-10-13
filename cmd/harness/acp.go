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
	"github.com/majorcontext/harness/internal/acp/cwdruntimes"
	"github.com/majorcontext/harness/store"
)

// acpCmd serves ACP over stdin and stdout. stdout holds ACP frames only; the
// logger writes to stderr. It reads no config before the first session/new. SIGINT and SIGTERM end it with status 0.
func acpCmd(args []string) error {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := acp.Serve(ctx, acpRuntimes(logger), os.Stdin, os.Stdout, acp.Config{Version: version, Logger: logger, MCP: true})
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

// acpRuntimes wires the real config loading, session directory, and owner into
// the runtimes of the acp command. The first work directory decides the session
// directory and the owner epoch.
func acpRuntimes(logger *slog.Logger) *cwdruntimes.Runtimes {
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
		Version: version,
		Getenv:  os.Getenv,
		Logger:  logger,
	})
}
