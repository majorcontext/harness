package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/store"
)

// acpRuntimes builds one harness.Runtime for each work directory, with the
// project config of that directory, over one Store and one Owner. The store
// directory, and the owner epoch, come from the config of the first directory.
type acpRuntimes struct {
	log *slog.Logger

	mu    sync.Mutex
	store harness.Store
	owner harness.Owner
	rts   map[string]*harness.Runtime
}

func newACPRuntimes(log *slog.Logger) *acpRuntimes {
	return &acpRuntimes{log: log, rts: map[string]*harness.Runtime{}}
}

// For returns the Runtime of cwd and builds it at the first call.
func (r *acpRuntimes) For(_ context.Context, cwd string) (acp.Runtime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rt, ok := r.rts[cwd]; ok {
		return rt, nil
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: cwd %q is not a directory", harness.ErrInvalidRequest, cwd)
	}
	cfg, err := config.LoadProject(userConfigPath(), cwd)
	if err == nil {
		err = applyOverrides(cfg, false, nil, nil)
	}
	if err != nil {
		r.log.Error("acp: config", "cwd", cwd, "error", err.Error())
		return nil, fmt.Errorf("%w: config of %s: %w", harness.ErrInvalidRequest, cwd, err)
	}
	if r.store == nil {
		dir, err := sessionDir(cfg.SessionDir)
		if err != nil {
			return nil, err
		}
		r.store = store.NewDiskStore(dir)
		r.owner, _ = replication(cfg)
	}
	rt, err := harness.New(harness.Options{Store: r.store, Owner: r.owner, Config: *cfg, WorkDir: cwd, Version: version, Getenv: os.Getenv, Logger: r.log})
	if err != nil {
		return nil, err
	}
	r.rts[cwd] = rt
	return rt, nil
}

// Close closes every Runtime.
func (r *acpRuntimes) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for _, rt := range r.rts {
		errs = append(errs, rt.Close(ctx))
	}
	return errors.Join(errs...)
}
