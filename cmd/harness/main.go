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
	"os"
	"path/filepath"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
)

var version = "0.1.0-dev"

// closeBudget bounds Runtime.Close at the end of a command.
const closeBudget = 5 * time.Second

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("harness " + version)
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
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  harness run -p <prompt> [flags]   run a one-shot prompt
  harness run -goal <condition> [flags]
                                    pursue a goal until an evaluator judges it
                                    met (exit 0 achieved, 3 not achieved)
  harness serve [-addr host:port] [-unauthenticated] [--ask-user-question]
                                    serve the HTTP+SSE session API
  harness plugin probe              probe the configured plugins and print
                                    their hooks
  harness sessions [--json]         list persisted sessions
  harness version                   print version

run flags:
`)
	runFlags(nil).PrintDefaults()
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
	return config.LoadProject(dir)
}

// applyOverrides applies the HARNESS_* variables to cfg, then the flags, so a
// flag wins over the environment.
func applyOverrides(cfg *config.Config, noInstructions bool, skillDirs, agentDefDirs []string) error {
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return err
	}
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
