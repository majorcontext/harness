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
	"net/http"
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
	// DefaultDir is the work directory of the host process, for a request that names none. Empty means the host has none.
	DefaultDir string
	// ModelTransport and Tools are the matching harness.Options of every runtime.
	ModelTransport func(provider string) http.RoundTripper
	Tools          []harness.Tool
	// IssuerTransport is the matching harness.Options of every runtime.
	IssuerTransport http.RoundTripper
	// Logins is the matching harness.Options of every runtime.
	Logins *harness.Logins
	// Host is the harness.Options.Host of every runtime. A connection whose client can take the login (RuntimeOptions.Login) clears NoLogin.
	Host harness.HostLimits
}

// Runtimes implements acp.Runtimes.
type Runtimes struct {
	opts Options

	mu    sync.Mutex
	store harness.Store
	owner harness.Owner
	rts   map[string]*harness.Runtime
	order []string
	shut  bool
}

// New returns Runtimes that build a Runtime at the first request for a directory.
func New(opts Options) *Runtimes {
	if opts.Logins != nil {
		opts.Logins.Owner().SetTransport(opts.ModelTransport)
	}
	return &Runtimes{opts: opts, rts: map[string]*harness.Runtime{}}
}

// For returns the Runtime of cwd. After Close it fails with harness.ErrDraining. A directory that does not exist, and a config that does not load, fail with harness.ErrInvalidRequest.
func (r *Runtimes) For(_ context.Context, cwd string, opts acp.RuntimeOptions) (acp.Runtime, error) {
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
	host := r.opts.Host
	host.NoLogin = host.NoLogin && !opts.Login
	rt, err := harness.New(harness.Options{Host: host, IssuerTransport: r.opts.IssuerTransport, Logins: r.opts.Logins, Store: r.store, Owner: r.owner, Config: *cfg, WorkDir: cwd,
		Version: r.opts.Version, Getenv: r.opts.Getenv, Logger: r.opts.Logger,
		ModelTransport: r.opts.ModelTransport, Tools: r.opts.Tools, AskUserQuestion: opts.AskUserQuestion})
	if err != nil {
		return nil, err
	}
	r.rts[cwd] = rt
	r.order = append(r.order, cwd)
	return rt, nil
}

// Default returns the Runtime that For built first, else the one of DefaultDir, else nil.
func (r *Runtimes) Default(ctx context.Context, opts acp.RuntimeOptions) (acp.Runtime, error) {
	r.mu.Lock()
	if len(r.order) > 0 {
		rt := r.rts[r.order[0]]
		r.mu.Unlock()
		return rt, nil
	}
	r.mu.Unlock()
	if r.opts.DefaultDir == "" {
		return nil, nil
	}
	return r.For(ctx, r.opts.DefaultDir, opts)
}

// ToolImage returns a tool-result image of a session from the shared Store. It
// reads the log of the session to find the key, and fails with
// harness.ErrBlobNotFound for any other key. It also fails before the first For
// builds a Runtime, because the Store does not exist yet.
func (r *Runtimes) ToolImage(ctx context.Context, session, key string) (io.ReadCloser, error) {
	r.mu.Lock()
	st := r.store
	r.mu.Unlock()
	if st == nil {
		return nil, fmt.Errorf("%w: no store", harness.ErrBlobNotFound)
	}
	found, err := acp.FindToolImage(harness.ReadEvents(ctx, st, session, 0), key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", harness.ErrBlobNotFound, key)
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
