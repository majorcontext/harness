//go:build js

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/acp"
)

// errNoFilesystem is the sentence for a host with no file system. wasm_exec.js then installs a stub whose every call
// answers ENOSYS, and a directory check would read as a wrong path.
const errNoFilesystem = "no file system is installed on globalThis.fs: install one before wasm_exec.js loads, " +
	"for example installMemoryFs() from @majorcontext/harness-embed/memoryfs, or start the module with wasm({ worker: false })"

type checkedRuntimes struct {
	acp.Runtimes
	defaultDir string
}

func checkFilesystem(dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, syscall.ENOSYS) {
		return fmt.Errorf("%w: %s", harness.ErrInvalidRequest, errNoFilesystem)
	}
	return nil
}

func (c checkedRuntimes) For(ctx context.Context, cwd string, opts acp.RuntimeOptions) (acp.Runtime, error) {
	if err := checkFilesystem(cwd); err != nil {
		return nil, err
	}
	return c.Runtimes.For(ctx, cwd, opts)
}

func (c checkedRuntimes) Default(ctx context.Context, opts acp.RuntimeOptions) (acp.Runtime, error) {
	if c.defaultDir != "" {
		if err := checkFilesystem(c.defaultDir); err != nil {
			return nil, err
		}
	}
	return c.Runtimes.Default(ctx, opts)
}
