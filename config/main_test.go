package config

import (
	"os"
	"testing"
)

// TestMain gives the tests a PATH with no claude, so no test depends on the machine.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "config-path")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("PATH", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
