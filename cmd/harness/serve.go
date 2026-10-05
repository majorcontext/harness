package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// serveBudget bounds Runtime.Close and the HTTP shutdown together.
const serveBudget = 5 * time.Second

// serveOptions are the flags of `harness serve`.
type serveOptions struct {
	addr, corsOrigin string
	noInstructions   bool
	unauthenticated  bool
	ask              bool
	skillDirs        []string
	agentDefDirs     []string
}

func serveFlags(args []string) (*serveOptions, error) {
	var o serveOptions
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.addr, "addr", "localhost:4096", "listen address")
	fs.StringVar(&o.corsOrigin, "cors-origin", "", "enable browser CORS by echoing this Access-Control-Allow-Origin value (e.g. a browser client's origin, or * for dev); empty disables CORS")
	fs.BoolVar(&o.noInstructions, "no-instructions", false, "disable automatic AGENTS.md injection for sessions served by this instance")
	fs.Func("skills-dir", "directory of Agent Skills to advertise (repeatable); overrides config skills_dirs", func(v string) error {
		o.skillDirs = append(o.skillDirs, v)
		return nil
	})
	fs.Func("agent-def-dir", "directory of custom task-tool agent definitions to advertise (repeatable); overrides config agent_defs_dirs", func(v string) error {
		o.agentDefDirs = append(o.agentDefDirs, v)
		return nil
	})
	fs.BoolVar(&o.unauthenticated, "unauthenticated", false, "serve without a bearer token on a non-loopback bind; for a deployment where a trusted external gate restricts reachability. Ignored when HARNESS_RUN_TOKEN is set. Also HARNESS_UNAUTHENTICATED=1")
	fs.BoolVar(&o.ask, "ask-user-question", false, "offer the Claude Code AskUserQuestion tool to root claude-code sessions; set only when a client answers parked questions through POST /sessions/{id}/requests/{request}")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return &o, nil
}

// serveCmd serves harness.Runtime over HTTP and SSE.
func serveCmd(args []string) error {
	o, err := serveFlags(args)
	if err != nil {
		return err
	}
	token := os.Getenv("HARNESS_RUN_TOKEN")
	unauthenticated, err := resolveUnauthenticated(token, o.addr, o.unauthenticated || envUnauthenticated())
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	if unauthenticated {
		warnUnauthenticated(logger, o.addr)
	}
	cfg, err := loadConfigLogged(logger)
	if err != nil {
		return err
	}
	if err := applyOverrides(cfg, o.noInstructions, o.skillDirs, o.agentDefDirs); err != nil {
		return err
	}
	dir, err := sessionDir(cfg.SessionDir)
	if err != nil {
		return err
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := removeStopReport(dir); err != nil {
		return err
	}
	store := newObservedStore(harness.NewDiskStore(dir), logger)
	rt, err := harness.New(harness.Options{Store: store, Config: *cfg, WorkDir: workDir, Version: version, AskUserQuestion: o.ask,
		ServeURL: serveURLForAddr(o.addr), RunToken: token})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", o.addr)
	if err != nil {
		return errors.Join(err, closeRuntime(rt))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	background, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	go newGCWatcher(logger).run(background)
	go store.watch(background)
	httpSrv := &http.Server{Handler: corsHandler(bearer(rt.Handler(), token, unauthenticated), o.corsOrigin)}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	var caughtUp atomic.Bool
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		startWork(ctx, rt, store, logger, &caughtUp)
	}()
	replicated := func() bool { return cfg.Sync != nil && caughtUp.Load() }
	logger.Info("serve start", "addr", o.addr, "version", version)
	select {
	case err := <-errc:
		stop()
		<-resumed
		closeErr := closeRuntime(rt)
		return errors.Join(err, closeErr, writeStopReport(dir, closeErr, replicated()))
	case <-ctx.Done():
		<-resumed
		budget, cancel := context.WithTimeout(context.Background(), serveBudget)
		defer cancel()
		closed := make(chan error, 1)
		go func() { closed <- rt.Close(budget) }()
		shutErr := httpSrv.Shutdown(budget)
		closeErr := <-closed
		return errors.Join(shutErr, closeErr, writeStopReport(dir, closeErr, replicated()))
	}
}

// serveURLForAddr derives the URL that a plugin dials to reach this serve
// from its listen address. A host that is empty or unspecified becomes the
// loopback address.
func serveURLForAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// startWork runs what a restart left, after the listener is up: it replicates
// every stored session through Sync, and opens each session that has work to
// resume. A session that does not replay, or does not open, stays as it is
// and is logged; it never stops serve.
func startWork(ctx context.Context, rt *harness.Runtime, store harness.Store, logger *slog.Logger, caughtUp *atomic.Bool) {
	var wg sync.WaitGroup
	wg.Go(func() {
		err := rt.CatchUp(ctx)
		caughtUp.Store(err == nil)
		if err != nil && !errors.Is(err, harness.ErrDraining) && ctx.Err() == nil {
			logger.Error("catch up stored sessions", "error", err.Error())
		}
	})
	wg.Go(func() { openStored(ctx, rt, store, logger) })
	wg.Wait()
}

// openStored opens each stored session that has work to resume: a turn that
// runs or is suspended, a queued input, an active or paused goal, a command
// that no owner finished, or a child that has not settled. A session with
// none stays closed, and a log that does not replay is skipped.
func openStored(ctx context.Context, rt *harness.Runtime, store harness.Store, logger *slog.Logger) {
	defer logger.Info("opened stored sessions")
	for after := ""; ctx.Err() == nil; {
		ids, err := store.Sessions(ctx, after, storedPage)
		if err != nil {
			logger.Error("list stored sessions", "error", err.Error())
			return
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			v, err := harness.OpenView(ctx, store, id)
			if err != nil {
				logger.Error("skip stored session that does not replay", "session", id, "error", err.Error())
				continue
			}
			if !v.Resumable() {
				continue
			}
			if _, err := rt.Open(ctx, id); err != nil {
				logger.Error("open stored session", "session", id, "error", err.Error())
			}
		}
		if len(ids) < storedPage {
			return
		}
		after = ids[len(ids)-1]
	}
}

// storedPage is the number of session IDs that openStored reads at once.
const storedPage = 100

// corsHandler adds the CORS headers of origin to every response and answers
// a preflight itself, before any token check: a preflight carries no
// credentials. An empty origin leaves next as it is.
func corsHandler(next http.Handler, origin string) http.Handler {
	if origin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func warnUnauthenticated(logger *slog.Logger, addr string) {
	if isLoopbackAddr(addr) {
		logger.Warn("serving unauthenticated on loopback", "addr", addr, "reason", "no run token set")
		return
	}
	logger.Warn("serving unauthenticated on a non-loopback bind", "addr", addr, "reason", "explicit -unauthenticated opt-in trusting an external network gate")
}

// bearer rejects each request except GET /health that lacks the run token.
func bearer(next http.Handler, token string, unauthenticated bool) http.Handler {
	if unauthenticated {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if (r.Method == http.MethodGet && r.URL.Path == "/health") || ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(protocol.ErrorBody{Error: protocol.Error{Code: "unauthorized", Message: "unauthorized", Details: map[string]any{}}})
	})
}

// isLoopbackAddr reports whether addr binds loopback only. An empty host, an
// unspecified or routable IP, a hostname other than localhost, and an
// address that does not parse all fail closed.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// envUnauthenticated reads HARNESS_UNAUTHENTICATED as a bool; an unset or
// malformed value is false.
func envUnauthenticated() bool {
	v, _ := strconv.ParseBool(os.Getenv("HARNESS_UNAUTHENTICATED"))
	return v
}

// resolveUnauthenticated decides whether serve runs without a bearer token.
// A token always wins. An empty token is allowed on a loopback bind, and on
// any bind with the explicit opt-in. It is never inferred from an empty
// token alone.
func resolveUnauthenticated(token, addr string, explicit bool) (bool, error) {
	switch {
	case token != "":
		return false, nil
	case isLoopbackAddr(addr), explicit:
		return true, nil
	}
	return false, fmt.Errorf("HARNESS_RUN_TOKEN is required")
}
