package server

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"sort"
	"strings"
	"time"
)

// Runtime profiles, served on THIS server's mux and only when Options.PProf
// is set.
//
// These handlers are written against runtime/pprof and runtime/trace
// directly, and this package deliberately does NOT import net/http/pprof.
// That package's init registers /debug/pprof/* on http.DefaultServeMux
// unconditionally, for the whole linked binary — so importing it, even to
// borrow its handler functions behind a flag, would expose profiling in any
// program that links this package and serves the default mux
// (http.ListenAndServe(addr, nil), an ordinary shape), with no opt-in and
// no way for Options.PProf to prevent it. A library must not register
// global handlers as an import side effect.
// TestPProf_NotRegisteredOnDefaultServeMux enforces the absence.
//
// Not served: /debug/pprof/symbol. `go tool pprof` symbolizes a profile
// locally against the binary it was taken from, which is how a box profile
// is read anyway.

// Profile durations for the CPU profile and the execution trace. A duration
// is bounded on both ends: a caller-chosen value must never turn into an
// unbounded profile holding a runtime-wide lock, and a zero-second profile
// would return an empty file that reads as a bug.
const (
	defaultProfileSeconds = 30 * time.Second
	minProfileSeconds     = 1 * time.Second
	maxProfileSeconds     = 60 * time.Second
)

// registerPProf adds the profiling routes to mux, each wrapped by auth.
// routes() calls it only when Options.PProf is set.
func registerPProf(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /debug/pprof/", auth(handlePProfIndex))
	mux.HandleFunc("GET /debug/pprof/{name}", auth(handlePProfNamed))
}

// handlePProfIndex lists the profiles this server can serve. It answers the
// collection path exactly; a deeper path that reached here matched no
// profile route, so it is a 404 rather than a redirect to this listing.
func handlePProfIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/debug/pprof/" {
		writeErr(w, http.StatusNotFound, "no such profile")
		return
	}
	names := []string{"profile", "trace", "cmdline"}
	for _, p := range pprof.Profiles() {
		names = append(names, p.Name())
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("profiles:\n")
	for _, name := range names {
		fmt.Fprintf(&b, "\t/debug/pprof/%s\n", name)
	}
	b.WriteString("\nprofile and trace take ?seconds=N (")
	fmt.Fprintf(&b, "%d-%d, default %d)\n", int(minProfileSeconds.Seconds()), int(maxProfileSeconds.Seconds()), int(defaultProfileSeconds.Seconds()))
	b.WriteString("every other profile takes ?debug=1 or ?debug=2 for text; heap takes ?gc=1\n")
	b.WriteString("symbolize with `go tool pprof <binary> <url>`\n")

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(b.String())) //nolint:errcheck // best-effort diagnostic write
}

// handlePProfNamed serves one profile: the CPU profile, the execution
// trace, the process command line, or any profile the runtime registers
// (goroutine, heap, allocs, block, mutex, threadcreate).
func handlePProfNamed(w http.ResponseWriter, r *http.Request) {
	switch name := r.PathValue("name"); name {
	case "profile":
		serveCPUProfile(w, r)
	case "trace":
		serveTrace(w, r)
	case "cmdline":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(strings.Join(os.Args, "\x00"))) //nolint:errcheck // best-effort diagnostic write
	default:
		serveRuntimeProfile(w, r, name)
	}
}

// serveRuntimeProfile writes one registered profile. debug=0 (the default)
// is the binary pprof format `go tool pprof` reads; debug=1 or 2 is the
// human-readable text form.
func serveRuntimeProfile(w http.ResponseWriter, r *http.Request, name string) {
	p := pprof.Lookup(name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "no such profile")
		return
	}
	query := r.URL.Query()
	debug, ok := intParam(w, query, "debug")
	if !ok {
		return
	}
	gc, ok := intParam(w, query, "gc")
	if !ok {
		return
	}
	// heap?gc=1 runs a collection first, so the profile reflects live data
	// rather than whatever survived the last cycle.
	if name == "heap" && gc > 0 {
		runtime.GC()
	}
	if debug > 0 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		setProfileDownloadHeaders(w, name)
	}
	p.WriteTo(w, debug) //nolint:errcheck // best-effort diagnostic write; nothing to do once headers are sent
}

// serveCPUProfile profiles the CPU for the requested duration. Only one CPU
// profile can run in a process, so a second concurrent request is a 409
// rather than a 500: the caller's own action is fine, it is just not
// possible right now.
func serveCPUProfile(w http.ResponseWriter, r *http.Request) {
	d, ok := profileSeconds(w, r)
	if !ok {
		return
	}
	setProfileDownloadHeaders(w, "profile")
	if err := pprof.StartCPUProfile(w); err != nil {
		writeErr(w, http.StatusConflict, "a CPU profile is already running: "+err.Error())
		return
	}
	sleepForProfile(r, d)
	pprof.StopCPUProfile()
}

// serveTrace records an execution trace for the requested duration. Like
// the CPU profile, a concurrent trace is a 409.
func serveTrace(w http.ResponseWriter, r *http.Request) {
	d, ok := profileSeconds(w, r)
	if !ok {
		return
	}
	setProfileDownloadHeaders(w, "trace")
	if err := trace.Start(w); err != nil {
		writeErr(w, http.StatusConflict, "a trace is already running: "+err.Error())
		return
	}
	sleepForProfile(r, d)
	trace.Stop()
}

// sleepForProfile waits d, or returns early when the client disconnects.
// An abandoned request must not keep a runtime-wide profile running for its
// full duration.
func sleepForProfile(r *http.Request, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.Context().Done():
	}
}

// setProfileDownloadHeaders marks a binary profile as a file, so a browser
// saves it instead of rendering bytes.
func setProfileDownloadHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	// The body length is unknown until the profile finishes, and a stale
	// Content-Length would truncate it.
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// profileSeconds is the ?seconds= duration. An absent parameter takes the
// default. A present one is parsed by this package's own intParam, so a
// malformed or repeated value is a 400 like everywhere else in this API
// rather than a silently substituted default. A well-formed value outside
// the bounds is CLAMPED, not rejected: "profile for an hour" is a coherent
// intention, just not one this server will hold a runtime-wide lock for.
func profileSeconds(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	query := r.URL.Query()
	if !query.Has("seconds") {
		return defaultProfileSeconds, true
	}
	n, ok := intParam(w, query, "seconds")
	if !ok {
		return 0, false
	}
	d := time.Duration(n) * time.Second
	if d < minProfileSeconds {
		return minProfileSeconds, true
	}
	if d > maxProfileSeconds {
		return maxProfileSeconds, true
	}
	return d, true
}
