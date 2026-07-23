package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/majorcontext/harness/config"
)

// loadConfigLogged loads the effective configuration exactly like
// loadConfig, then emits the single boot-time INFO line documented in
// docs/design/managed-processes.md §8: which config file (if any) this
// process loaded, and how much it declares. Used by serveCmd and runCmd
// only — sessionsCmd and pluginProbeCmd keep the plain, silent
// loadConfig, since this line belongs at the "the engine is booting"
// moment, not every CLI invocation.
func loadConfigLogged(logger *slog.Logger) (*config.Config, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cfg, info, err := config.LoadProjectWithInfo(dir)
	if err != nil {
		return nil, err
	}
	logConfigSummary(logger, info)
	return cfg, nil
}

// logConfigSummary emits exactly one slog INFO line naming which config
// file was loaded (or that none was found) plus its declared
// processes/mcp servers/plugins counts. This is the fix for a real
// operator failure: a misnamed config file loads as silently empty today,
// indistinguishable from an operator who never intended to configure
// anything at all.
func logConfigSummary(logger *slog.Logger, info config.LoadInfo) {
	if info.Path == "" {
		logger.Info("no config file found")
		return
	}
	msg := fmt.Sprintf("config: %s (%s)", info.Path, formatConfigCounts(info))
	// Only called out when it changes behavior from the built-in default:
	// "" and the explicit "fsync" are behaviorally identical (see
	// engine.Config.SessionSync), so only "volume" is worth an operator's
	// attention here.
	if info.SessionSync == "volume" {
		msg += fmt.Sprintf(", session_sync=%s", info.SessionSync)
	}
	logger.Info(msg)
}

// formatConfigCounts renders the "N processes, N mcp servers, N plugins"
// clause, pluralizing each count independently (a count of exactly 1 is
// singular; everything else, including 0, is plural).
func formatConfigCounts(info config.LoadInfo) string {
	return fmt.Sprintf("%s, %s, %s",
		pluralizeCount(info.Processes, "process", "processes"),
		pluralizeCount(info.MCPServers, "mcp server", "mcp servers"),
		pluralizeCount(info.Plugins, "plugin", "plugins"),
	)
}

func pluralizeCount(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
