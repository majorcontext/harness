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
	"syscall"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// serveBudget bounds Runtime.Close and the HTTP shutdown together.
const serveBudget = 5 * time.Second

// serveCmd serves harness.Runtime over HTTP and SSE.
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "localhost:4096", "listen address")
	corsOrigin := fs.String("cors-origin", "", "enable browser CORS by echoing this Access-Control-Allow-Origin value (e.g. a browser client's origin, or * for dev); empty disables CORS")
	noInstructions := fs.Bool("no-instructions", false, "disable automatic AGENTS.md injection for sessions served by this instance")
	var skillDirs, agentDefDirs []string
	fs.Func("skills-dir", "directory of Agent Skills to advertise (repeatable); overrides config skills_dirs", func(v string) error {
		skillDirs = append(skillDirs, v)
		return nil
	})
	fs.Func("agent-def-dir", "directory of custom task-tool agent definitions to advertise (repeatable); overrides config agent_defs_dirs", func(v string) error {
		agentDefDirs = append(agentDefDirs, v)
		return nil
	})
	unauthenticatedFlag := fs.Bool("unauthenticated", false, "serve without a bearer token on a non-loopback bind; for a deployment where a trusted external gate restricts reachability. Ignored when HARNESS_RUN_TOKEN is set. Also HARNESS_UNAUTHENTICATED=1")
	ask := fs.Bool("ask-user-question", false, "offer the Claude Code AskUserQuestion tool to root claude-code sessions; set only when a client answers parked questions through POST /sessions/{id}/requests/{request}")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token := os.Getenv("HARNESS_RUN_TOKEN")
	unauthenticated, err := resolveUnauthenticated(token, *addr, *unauthenticatedFlag || envUnauthenticated())
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	if unauthenticated {
		warnUnauthenticated(logger, *addr)
	}
	cfg, err := loadConfigLogged(logger)
	if err != nil {
		return err
	}
	if err := applyOverrides(cfg, *noInstructions, skillDirs, agentDefDirs); err != nil {
		return err
	}
	rt, err := newRuntime(cfg, *ask)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return errors.Join(err, closeRuntime(rt))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gcCtx, stopGC := context.WithCancel(ctx)
	defer stopGC()
	go newGCWatcher(logger).run(gcCtx)
	httpSrv := &http.Server{Handler: corsHandler(bearer(rt.Handler(), token, unauthenticated), *corsOrigin)}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		openStored(ctx, rt, logger)
	}()
	logger.Info("serve start", "addr", *addr, "version", version)
	select {
	case err := <-errc:
		stop()
		<-opened
		return errors.Join(err, closeRuntime(rt))
	case <-ctx.Done():
		<-opened
		budget, cancel := context.WithTimeout(context.Background(), serveBudget)
		defer cancel()
		closed := make(chan error, 1)
		go func() { closed <- rt.Close(budget) }()
		return errors.Join(httpSrv.Shutdown(budget), <-closed)
	}
}

// applyOverrides applies the HARNESS_* variables to cfg, then the serve flags,
// so a flag wins over the environment.
func applyOverrides(cfg *config.Config, noInstructions bool, skillDirs, agentDefDirs []string) error {
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return err
	}
	if noInstructions {
		off := false
		cfg.Instructions = &off
	}
	if len(skillDirs) > 0 {
		cfg.SkillsDirs = skillDirs
	}
	if len(agentDefDirs) > 0 {
		cfg.AgentDefsDirs = agentDefDirs
	}
	return nil
}

func closeRuntime(rt *harness.Runtime) error {
	ctx, cancel := context.WithTimeout(context.Background(), serveBudget)
	defer cancel()
	return rt.Close(ctx)
}

// newRuntime builds the Runtime of the working directory: cfg as given, a
// DiskStore in the session dir, and no Owner or Sync.
func newRuntime(cfg *config.Config, ask bool) (*harness.Runtime, error) {
	workDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir, err := sessionDir(false, cfg.SessionDir)
	if err != nil {
		return nil, err
	}
	return harness.New(harness.Options{Store: harness.NewDiskStore(dir), Config: *cfg, WorkDir: workDir, Version: version, AskUserQuestion: ask})
}

// openStored opens each stored session, so a turn or a queued input that a
// restart left resumes. It runs after the listener is up. A session that
// fails to open stays closed and is logged; it never stops serve.
func openStored(ctx context.Context, rt *harness.Runtime, logger *slog.Logger) {
	defer logger.Info("opened stored sessions")
	for after := ""; ctx.Err() == nil; {
		page, err := rt.List(ctx, protocol.ListSessions{After: after})
		if err != nil {
			logger.Error("list stored sessions", "error", err.Error())
			return
		}
		for _, s := range page.Sessions {
			if ctx.Err() != nil {
				return
			}
			if _, err := rt.Open(ctx, s.ID); err != nil {
				logger.Error("open stored session", "session", s.ID, "error", err.Error())
			}
		}
		if page.Next == "" {
			return
		}
		after = page.Next
	}
}

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
