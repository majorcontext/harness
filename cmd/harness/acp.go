package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/majorcontext/harness/internal/acp"
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
	err := acp.Serve(ctx, newACPRuntimes(logger), os.Stdin, os.Stdout, acp.Config{Version: version, Logger: logger, MCP: true})
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}
