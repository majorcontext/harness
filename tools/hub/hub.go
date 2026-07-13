package hub

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

// defaultAddr is deliberately loopback-only: the hub is a local, single-
// operator dev tool (see AGENTS.md, "Development hub") and never listens on
// every interface by default.
const defaultAddr = "localhost:7777"

// spawnCommandEnv is the environment-variable fallback for -spawn-command,
// so a wrapper script can configure the hub without a flag.
const spawnCommandEnv = "HARNESS_HUB_SPAWN"

// contentSecurityPolicy hardens the served page (defense-in-depth for a page
// that carries run tokens in its URL fragment). The hub is a single inline
// file that loads NO external resources, so default-src 'none' blocks every
// external fetch/script/style/image/frame; the page's own inline script and
// inline style attributes are permitted with 'unsafe-inline' (a per-response
// nonce/hash is not viable on a byte-for-byte go:embed'd, no-build file);
// connect-src * is required because the page fetches/streams from arbitrary,
// operator-added box origins the hub cannot enumerate (it keeps no state).
// frame-ancestors/base-uri/form-action are pinned to 'none' explicitly since
// they do not inherit from default-src.
const contentSecurityPolicy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src *; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// Options configures a hub server. The zero value is not directly useful;
// Run below builds one from flags/env for the `harness hub` subcommand, but
// tests construct Options directly to avoid touching flags or the process
// environment.
type Options struct {
	// SpawnCommand is executed via `sh -c` by POST /spawn. Empty disables
	// spawning: the endpoint reports the "no spawn command configured"
	// error from runSpawn rather than failing to start the hub itself — a
	// hub with no spawn command configured is still useful for driving
	// boxes added by hand.
	SpawnCommand string
}

// NewHandler builds the hub's HTTP handler: the embedded page at "/" and
// the single POST /spawn API described in AGENTS.md. Everything else the
// page needs (session state, box CRUD) is client-side — see index.html.
func NewHandler(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/spawn", handleSpawn(opts))
	return mux
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write(indexHTML) //nolint:errcheck
	}
}

// spawnRequest is POST /spawn's optional JSON body: {"name": "..."} passes
// the box name the page generated (or is re-using for a Respawn/ADOPT) —
// see AGENTS.md's spawn contract section and runSpawn's `name` parameter.
// A missing or empty body is fine (no name passthrough), matching every
// spawn command that predates this field.
type spawnRequest struct {
	Name string `json:"name"`
}

// isCrossOrigin reports whether r is a browser cross-origin request: an
// Origin header is present and names a host other than the one the request
// was addressed to (r.Host). This is the CSRF guard for the state-changing
// POST /spawn, which execs the deployment provision command. A browser
// attaches Origin to every cross-origin POST, so an attacker page's
// fetch("http://localhost:7777/spawn") is rejected (its Origin is the
// attacker's, not the hub's). The hub page's own same-origin fetch sends
// Origin == Host and passes; a request with no Origin at all (curl, scripts,
// server-side callers — no ambient browser credentials, so not a CSRF
// vector) also passes. An Origin we cannot parse, or the opaque "null"
// origin a sandboxed context sends, is treated as cross-origin.
func isCrossOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true
	}
	return u.Host != r.Host
}

// handleSpawn streams runSpawn's events to the client as an SSE response.
// The request context is what runSpawn's exec.CommandContext keys off of,
// so a client disconnect kills the spawn process directly — see spawn.go.
func handleSpawn(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// CSRF guard: reject a browser cross-origin POST before any exec.
		if isCrossOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		var req spawnRequest
		if r.Body != nil {
			// A body is entirely optional (an empty POST is the pre-existing,
			// still-supported contract): only a non-EOF decode error is a real
			// problem, and even then we don't fail the request over it — a
			// malformed body just means no name passthrough, same as none.
			_ = json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		bw := bufio.NewWriter(w)
		runSpawn(r.Context(), opts.SpawnCommand, req.Name, func(ev spawnEvent) {
			bw.Write(ev.marshal()) //nolint:errcheck
			bw.Flush()             //nolint:errcheck
			flusher.Flush()
		})
	}
}

// resolveAddr applies -addr's default and documents the loopback-by-default
// promise: an address with an empty or unspecified host (e.g. ":7777" or
// "0.0.0.0:7777") is passed through as given — the operator asked for it
// explicitly by supplying -addr — but the flag's own default is always the
// loopback address, so doing nothing at all stays local.
func resolveAddr(addr string) string {
	if addr == "" {
		return defaultAddr
	}
	return addr
}

// spawnCommandFromEnv resolves -spawn-command's fallback: the
// HARNESS_HUB_SPAWN environment variable, consulted only when the flag was
// not passed at all (flagSet, not merely flagValue == "", so an explicit
// -spawn-command ” can still disable spawning without env clobbering it).
func spawnCommandFromEnv(flagValue string, flagSet bool, getenv func(string) string) string {
	if flagSet {
		return flagValue
	}
	if v := getenv(spawnCommandEnv); v != "" {
		return v
	}
	return flagValue
}

// Run implements the `harness hub` subcommand: parse flags, build the
// handler, serve until interrupted. It is the only network-facing entry
// point in this package — NewHandler/Options above are what tests exercise
// directly.
func Run(args []string) error {
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var addr string
	fs.StringVar(&addr, "addr", defaultAddr, "listen address (loopback by default — this is a local, single-operator tool)")
	var spawnCommand string
	fs.StringVar(&spawnCommand, "spawn-command", "", "shell command (run via `sh -c`) that POST /spawn execs to bring up a new box; falls back to $"+spawnCommandEnv+"; see AGENTS.md's spawn-command contract")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var spawnFlagSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "spawn-command" {
			spawnFlagSet = true
		}
	})
	spawnCommand = spawnCommandFromEnv(spawnCommand, spawnFlagSet, os.Getenv)

	handler := NewHandler(Options{SpawnCommand: spawnCommand})
	httpSrv := &http.Server{Addr: resolveAddr(addr), Handler: handler}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", httpSrv.Addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "harness hub listening on http://%s\n", ln.Addr())

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}
