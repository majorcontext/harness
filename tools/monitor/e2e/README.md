# tools/monitor/e2e — real-backend verification for the monitor page

`tools/monitor/index.html` is a single, build-free HTML file with zero
dependencies (see its own header comment) — that does not change here. This
directory is a separate, isolated, npm-based verification *tool* that proves
the page's board/detail/composer behavior against a **real** running harness
backend, not hand-rolled mocks. Mirrors `tools/hub/e2e`'s structure and
conventions throughout.

## What it checks

`real_e2e.mjs`, driven by `e2e_test.go`, starts a real `server.Server`
(`stub.go`'s `Start`, the same wiring as `harness serve`) backed by a
handful of small scripted providers (no API key needed — including a
**real** `bash` tool call, a short `sleep`, not a simulated delay), plus a
plain static file server for the *actual* `tools/monitor/index.html`, then
loads that real page in [jsdom](https://github.com/jsdom/jsdom) with Node's
own, **unmocked** `fetch` — real HTTP requests, real SSE streams, real
engine turns. It confirms:

1. the static server serves `tools/monitor/index.html` byte-for-byte
   (production wiring, not a stale copy);
2. a wrong run token surfaces an inline connect error (from `/session`'s
   401, never from the unauthenticated `/health`); a correct one renders a
   real `/health` identity line (version, `session_sync`) and an empty
   board;
3. a real scripted turn — streaming text deltas AND a real, briefly-blocking
   `bash` tool call — drives a board row through the streaming/tool phases
   and back to idle with outcome `completed`;
4. a session left running long enough (against shrunk, test-only staleness
   thresholds — see `index.html`'s `window.__monitorTuning` seam) crosses
   the `quiet` and `stalled` tiers live;
5. opening a session's detail view via a **real row click** renders its
   durable history — operator/assistant/tool entries, a completed tool fold
   starting collapsed — and a fold that is still genuinely running at the
   moment it's observed renders open, then settles to completed live
   without ever being force-collapsed;
6. the composer's `prompt.queued` optimistic entry appears for a send into
   a **busy** session and is replaced (not duplicated) once the durable,
   template-wrapped message lands; a send into an idle session runs a
   normal turn (a `message` event, not `prompt.queued`); a send against an
   unknown session id surfaces the server's real non-2xx error text inline;
7. killing the box's HTTP layer **server-side** flips the header to
   "reconnecting…", and restarting it resumes the stream — proven live by a
   brand-new session created after the restart still arriving via SSE.

## The staleness test seam

Production `QUIET_MS`/`STALL_MS` are 15s/60s — far too slow for a CI-sane
test. `index.html` reads `window.__monitorTuning = { QUIET_MS, STALL_MS }`
(set here via jsdom's `beforeParse`, so it lands before the page's inline
script ever runs) to override them; nothing in production ever sets that
global, so it is a no-op outside this harness. See `index.html`'s comment
just after `TESTABLE-END` and `real_e2e.mjs`'s `TUNING` constant (which also
explains why the QUIET/STALL gap must stay wider than the board's own 1s
ticker, or the "quiet" tier can fall entirely between two samples).

## Running it

No manual setup step is required. Just run the same command already used to
verify this repo:

```sh
go test -race ./...          # or narrower: go test ./tools/monitor/e2e/...
```

`TestRealEndToEnd` installs its own dependency (`npm ci`, using the
package-lock.json committed here) the first time it runs if jsdom isn't
already present in this directory, then drives the real check. `node` (and
therefore `npm`, which ships with it) is already a hard requirement of this
repo's `node --test tools/monitor/*_test.mjs` check, so this test only skips
in the one case where that other required command would ALSO be unrunnable
— no Node toolchain on `PATH` at all. It fails loudly (not a silent skip) if
`node`/`npm` ARE present but the dependency install itself fails (e.g. no
network access to npm's registry on first run).

To drive it by hand instead (e.g. to poke at the real backend from an
actual browser), write a small one-off `main` package that calls
`e2e.Start()` and prints the returned `Stub`'s `BoxBase`/`MonitorBase`/
`Token` (see `stub.go`), then:

```sh
node tools/monitor/e2e/real_e2e.mjs <box_base> <monitor_base> <token>
# or open monitor_base in a real browser and connect with box_base + token by hand
```
