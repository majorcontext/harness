// Package cwdruntimes builds one harness.Runtime for each work directory, over
// one Store and one Owner, for the Runtimes interface of internal/acp. The host
// injects how a directory loads its config and where the shared Store and Owner
// come from, so the package reads no config file and no environment variable.
package cwdruntimes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
)

// Options holds what the host injects.
type Options struct {
	// Config returns the effective config of a work directory.
	Config func(cwd string) (*config.Config, error)
	// Shared returns the Store and the Owner of every runtime. It runs once, with the config of the first directory.
	Shared func(first *config.Config) (harness.Store, harness.Owner, error)
	// Version, Getenv, and Logger are the matching harness.Options.
	Version string
	Getenv  func(string) string
	Logger  *slog.Logger
}

// Runtimes implements acp.Runtimes.
type Runtimes struct {
	opts Options

	mu    sync.Mutex
	store harness.Store
	owner harness.Owner
	rts   map[string]*harness.Runtime
	shut  bool
}

// New returns Runtimes that build a Runtime at the first request for a directory.
func New(opts Options) *Runtimes {
	return &Runtimes{opts: opts, rts: map[string]*harness.Runtime{}}
}

// For returns the Runtime of cwd. After Close it fails with harness.ErrDraining. A directory that does not exist, and a config that does not load, fail with harness.ErrInvalidRequest.
func (r *Runtimes) For(_ context.Context, cwd string) (acp.Runtime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shut {
		return nil, harness.ErrDraining
	}
	if rt, ok := r.rts[cwd]; ok {
		return rt, nil
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: cwd %q is not a directory", harness.ErrInvalidRequest, cwd)
	}
	cfg, err := r.opts.Config(cwd)
	if err != nil {
		if r.opts.Logger != nil {
			r.opts.Logger.Error("acp: config", "cwd", cwd, "error", err.Error())
		}
		return nil, fmt.Errorf("%w: config of %s: %w", harness.ErrInvalidRequest, cwd, err)
	}
	if r.store == nil {
		if r.store, r.owner, err = r.opts.Shared(cfg); err != nil {
			return nil, err
		}
	}
	rt, err := harness.New(harness.Options{Store: r.store, Owner: r.owner, Config: *cfg, WorkDir: cwd,
		Version: r.opts.Version, Getenv: r.opts.Getenv, Logger: r.opts.Logger})
	if err != nil {
		return nil, err
	}
	r.rts[cwd] = rt
	return rt, nil
}

// Blob returns the blob key of a session from the shared Store. It fails
// before the first For builds a Runtime, because the Store does not exist yet.
func (r *Runtimes) Blob(ctx context.Context, session, key string) (io.ReadCloser, error) {
	r.mu.Lock()
	st := r.store
	r.mu.Unlock()
	if st == nil {
		return nil, fmt.Errorf("%w: no store", harness.ErrBlobNotFound)
	}
	return st.GetBlob(ctx, session, key)
}

// Close closes every Runtime and returns every error.
func (r *Runtimes) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shut = true
	var errs []error
	for _, rt := range r.rts {
		errs = append(errs, rt.Close(ctx))
	}
	return errors.Join(errs...)
}
