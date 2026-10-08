package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
)

// probeBound bounds the probe of each plugin.
const probeBound = 30 * time.Second

// pluginCmd dispatches `harness plugin <subcommand>`.
func pluginCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: harness plugin probe")
	}
	switch args[0] {
	case "probe":
		return pluginProbeCmd(args[1:])
	default:
		return fmt.Errorf("unknown plugin subcommand %q (want: probe)", args[0])
	}
}

// defaultPluginCache puts the plugin cache beside the user config when no
// variable or key names it, so a machine with its own HARNESS_CONFIG has its
// own cache.
func defaultPluginCache(cfg *config.Config) {
	if cfg.PluginCache == "" {
		cfg.PluginCache = filepath.Join(filepath.Dir(config.Path()), "plugin_cache.json")
	}
}

// pluginProbeCmd probes every configured plugin again, stores the manifests
// in the plugin cache, and prints each name and its hooks. It confirms that a
// new plugin is wired correctly before a session uses it, and it refreshes
// the cache after a plugin is rebuilt.
func pluginProbeCmd(args []string) error {
	fs := flag.NewFlagSet("plugin probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Plugins) == 0 {
		fmt.Println("no plugins configured")
		return nil
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return err
	}
	defaultPluginCache(cfg)
	rt, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: *cfg, WorkDir: workDir, Version: version})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeBound*time.Duration(len(cfg.Plugins)))
	defer cancel()
	plugins, err := rt.ProbePlugins(ctx)
	if err != nil {
		return errors.Join(err, closeRuntime(rt))
	}
	for _, p := range plugins {
		fmt.Printf("%s: %s\n", p.Name, strings.Join(p.Hooks, ", "))
	}
	return closeRuntime(rt)
}
