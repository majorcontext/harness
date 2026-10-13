// Command harness is the CLI for the harness agent runtime.
//
// Startup speed is a budget (see cmd/harness/AGENTS.md): nothing here touches
// the network or spawns processes before the selected command needs it.
// Provider auth is validated on first use, not at boot.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
)

var version = "0.1.0-dev"

// closeBudget bounds Runtime.Close at the end of a command.
const closeBudget = 5 * time.Second

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version", "--version", "-v":
		if len(os.Args) > 2 {
			versionArgs(os.Args[2:])
			return
		}
		fmt.Println("harness " + version)
		return
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	case "run":
		err = runCmd(os.Args[2:])
		if errors.Is(err, errGoalNotAchieved) {
			os.Exit(3)
		}
	case "sessions":
		err = sessionsCmd(os.Args[2:])
	case "serve":
		err = serveCmd(os.Args[2:])
	case "plugin":
		err = pluginCmd(os.Args[2:])
	default:
		_, _ = fmt.Fprintf(os.Stderr, "harness: unknown command %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	switch {
	case errors.Is(err, errHelp):
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, errorLine(err))
		os.Exit(1)
	}
}

// versionArgs handles the arguments of `harness version`: -h and --help print
// its usage, and any other argument is a usage error.
func versionArgs(args []string) {
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Println("usage: harness version")
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "harness: unexpected argument %q\nusage: harness version\n", args[0])
	os.Exit(2)
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `usage:
  harness run -p <prompt> [flags]   run a one-shot prompt
  harness run -goal <condition> [flags]
                                    pursue a goal until an evaluator judges it
                                    met (exit 0 achieved, 3 not achieved,
                                    1 turn or evaluator failed)
  harness serve [-addr host:port] [-unauthenticated] [--ask-user-question]
                                    serve the HTTP+SSE session API
  harness plugin probe              probe the configured plugins and print
                                    their hooks
  harness sessions [--json]         list persisted sessions
  harness version                   print version

Run harness <command> -h for the flags of a command.

run flags:
`)
	fs := runFlags(nil)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// sessionDir returns the directory of the session logs: HARNESS_SESSION_DIR,
// then the session_dir config key, then ~/.harness/sessions.
func sessionDir(configDir string) (string, error) {
	if dir := os.Getenv("HARNESS_SESSION_DIR"); dir != "" {
		return dir, nil
	}
	if configDir != "" {
		return configDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".harness", "sessions"), nil
}

// loadConfig loads the effective configuration once: the user config file
// plus, if present, the current directory's project override. This is the only
// disk access on the boot path (at most two file reads; missing files are
// fine) — no network, no process spawn, no directory creation.
func loadConfig() (*config.Config, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return config.LoadProject(userConfigPath(), dir)
}

// userConfigPath is $HARNESS_CONFIG if set, otherwise ~/.harness/config.json.
func userConfigPath() string {
	if p := os.Getenv("HARNESS_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".harness", "config.json")
	}
	return filepath.Join(home, ".harness", "config.json")
}

// applyOverrides applies the HARNESS_* variables to cfg, then the flags, so a
// flag wins over the environment.
func applyOverrides(cfg *config.Config, noInstructions bool, skillDirs, agentDefDirs []string) error {
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return err
	}
	defaultPluginCache(cfg)
	if noInstructions {
		off := false
		cfg.Instructions = &off
	}
	if len(skillDirs) > 0 {
		cfg.SkillsDirs = skillDirs
	}
	if len(agentDefDirs) > 0 {
		cfg.AgentDefsDirs = agentDefDirs
	}
	return nil
}

// closeRuntime closes rt within closeBudget.
func closeRuntime(rt *harness.Runtime) error {
	ctx, cancel := context.WithTimeout(context.Background(), closeBudget)
	defer cancel()
	return rt.Close(ctx)
}

// errorLine returns the text that main prints for err. A runtime error already starts with "harness: ".
func errorLine(err error) string {
	if msg := err.Error(); strings.HasPrefix(msg, "harness: ") {
		return msg
	}
	return "harness: " + err.Error()
}
