package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// inFlightWatchdogThreshold is how long a store/create phase may be in
// flight before the watchdog starts warning about it. It is separate from
// slowPhaseThreshold (1s): slowPhaseThreshold only ever fires ONCE, on
// completion, so it says nothing about a phase that never completes. That
// gap is exactly what a production canary hit: creates hung PERMANENTLY
// mid-ensureLog with zero phase log lines at all, because completion-only
// logging is blind to a phase that never completes — a wedged network
// volume can hang a file operation indefinitely, with no error, no
// completion, and (until now) no log line naming what's stuck.
const inFlightWatchdogThreshold = 5 * time.Second

// inFlightWatchdogTick is how often the watchdog re-scans the in-flight
// table and re-warns about anything still stuck.
const inFlightWatchdogTick = 5 * time.Second

// inFlightEntry is one phase currently in flight.
type inFlightEntry struct {
	op, phase, session string // session is "" for engine store-phase entries
	since              time.Time
}

// inFlightWatchdog pairs engine.Config.OnStorePhaseStart/OnStorePhase and
// server.Options.OnCreatePhaseStart/OnCreatePhase into one small in-flight
// table and periodically (see run) warns about any entry that has been
// in flight longer than inFlightWatchdogThreshold — repeating on every tick
// for as long as it stays stuck, which is the whole point: it is the one
// piece of production observability that survives a phase that NEVER
// completes, where every other log line in this file (slowStorePhaseLogger,
// createPhaseLogger) is silent by construction.
//
// Store-phase entries carry no session identity — engine.Config.
// OnStorePhase/OnStorePhaseStart don't carry one either (see their doc
// comments: the engine has no notion of "which session" at that layer,
// since the same callback closure is shared across every session an
// instance serves) — so two sessions concurrently stuck in the identical
// op/phase collapse to one table entry. That mirrors the existing
// imprecision slowStorePhaseLogger already has (it can't disambiguate
// concurrent same-op-phase warnings either); create-phase entries don't
// have this problem, since they're additionally keyed by session ID.
//
// One instance, one mutex, one ticker goroutine for the whole process (see
// serveCmd) — not one per session — because both callback sources are
// themselves process-wide singletons already (storePhase/createPhase in
// serveCmd), and a single small table is cheap to scan every tick.
type inFlightWatchdog struct {
	logger *slog.Logger
	now    func() time.Time // injectable for tests; time.Now in production

	mu      sync.Mutex
	entries map[string]inFlightEntry
}

func newInFlightWatchdog(logger *slog.Logger) *inFlightWatchdog {
	return &inFlightWatchdog{logger: logger, now: time.Now, entries: make(map[string]inFlightEntry)}
}

func storeWatchdogKey(op, phase string) string         { return "store|" + op + "|" + phase }
func createWatchdogKey(sessionID, phase string) string { return "create|" + sessionID + "|" + phase }

// startStorePhase records an engine store op/phase beginning. Wire to
// engine.Config.OnStorePhaseStart.
func (w *inFlightWatchdog) startStorePhase(op, phase string) {
	w.start(storeWatchdogKey(op, phase), inFlightEntry{op: op, phase: phase, since: w.now()})
}

// doneStorePhase clears an engine store op/phase on completion. Wire
// alongside the existing engine.Config.OnStorePhase callback
// (slowStorePhaseLogger).
func (w *inFlightWatchdog) doneStorePhase(op, phase string) {
	w.done(storeWatchdogKey(op, phase))
}

// startCreatePhase records a server create phase beginning for sessionID.
// Wire to server.Options.OnCreatePhaseStart.
func (w *inFlightWatchdog) startCreatePhase(sessionID, phase string) {
	w.start(createWatchdogKey(sessionID, phase), inFlightEntry{op: "create", phase: phase, session: sessionID, since: w.now()})
}

// doneCreatePhase clears a server create phase for sessionID on completion.
// Wire alongside the existing server.Options.OnCreatePhase callback
// (createPhaseLogger.OnCreatePhase).
func (w *inFlightWatchdog) doneCreatePhase(sessionID, phase string) {
	w.done(createWatchdogKey(sessionID, phase))
}

func (w *inFlightWatchdog) start(key string, e inFlightEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries[key] = e
}

func (w *inFlightWatchdog) done(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.entries, key)
}

// check scans every in-flight entry and warns ("store phase in flight")
// about any older than inFlightWatchdogThreshold relative to now — one line
// per stuck entry per call, so wiring it to a 5s ticker (see run) bounds log
// volume to one line per stuck phase per 5s, repeating for as long as it
// stays stuck. Split out from run so tests can drive it directly against an
// injected now/entries instead of waiting on a real ticker (house rule: no
// raw sleeps in tests).
func (w *inFlightWatchdog) check(now time.Time) {
	w.mu.Lock()
	entries := make([]inFlightEntry, 0, len(w.entries))
	for _, e := range w.entries {
		entries = append(entries, e)
	}
	w.mu.Unlock()
	for _, e := range entries {
		inFlight := now.Sub(e.since)
		if inFlight < inFlightWatchdogThreshold {
			continue
		}
		if e.session != "" {
			w.logger.Warn("store phase in flight", "op", e.op, "phase", e.phase, "session", e.session, "in_flight_ms", inFlight.Milliseconds())
		} else {
			w.logger.Warn("store phase in flight", "op", e.op, "phase", e.phase, "in_flight_ms", inFlight.Milliseconds())
		}
	}
}

// run starts the periodic ticker loop (inFlightWatchdogTick) that drives
// check against the real clock, until ctx is cancelled. See serveCmd, which
// constructs ctx as its own cancelable child and cancels it on shutdown —
// the same lifecycle precedent as engine.NewMCPManager's retryCtx
// (engine/mcp.go): a background goroutine tied to a dedicated cancelable
// context so it never leaks past process shutdown.
func (w *inFlightWatchdog) run(ctx context.Context) {
	t := time.NewTicker(inFlightWatchdogTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			w.check(now)
		}
	}
}
