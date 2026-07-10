package hub

import (
	"bufio"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"net"
	"net/http"
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
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write(indexHTML) //nolint:errcheck
	}
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
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		bw := bufio.NewWriter(w)
		runSpawn(r.Context(), opts.SpawnCommand, func(ev spawnEvent) {
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
// -spawn-command '' can still disable spawning without env clobbering it).
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
