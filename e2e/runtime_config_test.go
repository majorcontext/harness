package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/config"
)

// loadRuntimeConfig loads the config of a runtime start: the file, with the project layer of workDir when it has one.
func loadRuntimeConfig(t *testing.T, configPath, workDir string, env map[string]string) *config.Config {
	t.Helper()
	c, err := config.Load(configPath)
	if _, statErr := os.Stat(filepath.Join(workDir, ".harness.json")); workDir != "" && statErr == nil {
		c, err = config.LoadProject(configPath, workDir)
	}
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := c.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("apply env: %v", err)
	}
	return c
}
