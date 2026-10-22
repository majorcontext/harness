//go:build unix

// Command coverwrap runs the covered harness binary beside it with GOCOVERDIR
// set to a directory of its own process. A covered binary rewrites the
// meta-data file of GOCOVERDIR on every exit through a temporary name taken
// from the clock alone, so two processes that exit in the same tick race on
// the rename and one prints an error to stderr. The e2e suite moves the files
// back into GOCOVERDIR before it reports.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

func main() {
	self, err := os.Executable()
	if err != nil {
		fail(err)
	}
	if root := os.Getenv("GOCOVERDIR"); root != "" {
		dir := filepath.Join(root, strconv.Itoa(os.Getpid()))
		if err := os.Mkdir(dir, 0o755); err != nil {
			fail(err)
		}
		if err := os.Setenv("GOCOVERDIR", dir); err != nil {
			fail(err)
		}
	}
	bin := filepath.Join(filepath.Dir(self), "harness-covered")
	fail(syscall.Exec(bin, os.Args, os.Environ()))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "coverwrap:", err)
	os.Exit(1)
}
