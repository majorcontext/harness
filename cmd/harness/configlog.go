package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/majorcontext/harness/config"
)

// loadConfigLogged loads the effective configuration exactly like
// loadConfig, then emits the single boot-time INFO line documented in
// docs/design/managed-processes.md §8: which config file (if any) this
// process loaded, and how much it declares. Used by serveCmd and runCmd
// only — sessionsCmd and pluginProbeCmd keep the plain, silent
// loadConfig, since this line belongs at the moment the process boots,
// not every CLI invocation.
func loadConfigLogged(logger *slog.Logger) (*config.Config, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	userPath := userConfigPath()
	cfg, err := config.LoadProject(userPath, dir)
	if err != nil {
		return nil, err
	}
	logConfigSummary(logger, configSummaryOf(cfg, userPath, filepath.Join(dir, ".harness.json")))
	return cfg, nil
}

// configSummary names the config file that loadConfigLogged found and counts what it declares.
type configSummary struct {
	// path is the project file when it exists, else the user file when it exists, else empty.
	path                           string
	processes, mcpServers, plugins int
}

func configSummaryOf(cfg *config.Config, userPath, projPath string) configSummary {
	info := configSummary{processes: len(cfg.Processes), mcpServers: len(cfg.MCPServers), plugins: len(cfg.Plugins)}
	switch {
	case fileExists(projPath):
		info.path = projPath
	case fileExists(userPath):
		info.path = userPath
	}
	return info
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// logConfigSummary emits exactly one slog INFO line naming which config
// file was loaded (or that none was found) plus its declared
// processes/mcp servers/plugins counts. This is the fix for a real
// operator failure: a misnamed config file loads as silently empty today,
// indistinguishable from an operator who never intended to configure
// anything at all.
func logConfigSummary(logger *slog.Logger, info configSummary) {
	if info.path == "" {
		logger.Info("no config file found")
		return
	}
	msg := fmt.Sprintf("config: %s (%s)", info.path, formatConfigCounts(info))
	logger.Info(msg)
}

// formatConfigCounts renders the "N processes, N mcp servers, N plugins"
// clause, pluralizing each count independently (a count of exactly 1 is
// singular; everything else, including 0, is plural).
func formatConfigCounts(info configSummary) string {
	return fmt.Sprintf("%s, %s, %s",
		pluralizeCount(info.processes, "process", "processes"),
		pluralizeCount(info.mcpServers, "mcp server", "mcp servers"),
		pluralizeCount(info.plugins, "plugin", "plugins"),
	)
}

func pluralizeCount(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
