# tools/hub/e2e — real-backend verification for the hub page

`tools/hub/index.html` is a single, build-free HTML file with zero
dependencies (see its own header comment) — that does not change here.
This directory is a separate, isolated, npm-based verification *tool* that
proves the page's incremental-rendering behavior against a **real** running
harness backend, not hand-rolled mocks.

## What it checks

`real_e2e.mjs`, driven by `e2e_test.go`, starts:

- a real `server.Server` (`stub.go`'s `Start`, the same wiring as
  `harness serve`) backed by a small scripted provider (no API key needed),
  and
- a real `hub.NewHandler` (the same wiring as `harness hub`), serving the
  *actual* embedded `tools/hub/index.html`,

then loads that real page in [jsdom](https://github.com/jsdom/jsdom) with
Node's own, **unmocked** `fetch` — real HTTP requests, real SSE streams,
real engine turns. It confirms:

1. the hub server serves `tools/hub/index.html` byte-for-byte (production
   wiring, not a stale copy);
2. a populated URL fragment renders its box skeleton synchronously — no
   "no boxes yet" flash;
3. a real `/health`+`/session` poll resolves to a healthy dot and a real
   `vcs_revision`, and the box card's DOM node survives a real session
   being created (no needless rebuild);
4. a real engine turn renders as a keyed, durable message; expanding its
   reasoning block survives a **second** real, server-driven turn — the
   keyed, append-only timeline, exercised over an actual SSE stream, not a
   simulated one;
5. a scrolled-up timeline is left alone by a real subsequent render
   (pinned-tail autoscroll).

## Running it

No manual setup step is required. Just run the same command already used to
verify this repo:

```sh
go test -race ./...          # or narrower: go test ./tools/hub/e2e/...
```

`TestRealEndToEnd` installs its own dependency (`npm ci`, using the
package-lock.json committed here) the first time it runs if jsdom isn't
already present in this directory, then drives the real check. `node` (and
therefore `npm`, which ships with it) is already a hard requirement of this
repo's `node --test tools/hub/*_test.mjs` check, so this test only skips in
the one case where that other required command would ALSO be unrunnable —
no Node toolchain on `PATH` at all. It fails loudly (not a silent skip) if
`node`/`npm` ARE present but the dependency install itself fails (e.g. no
network access to npm's registry on first run) — an offline environment is
a real verification gap, not something to paper over.

To drive it by hand instead (e.g. to poke at the real backend from an
actual browser):

```sh
cd tools/hub/e2e && npm ci    # only needed once, if not already run by the test above
go run ./tools/hub/e2e/hubverify     # prints {"box_base":...,"hub_base":...,"token":...}, then blocks
node tools/hub/e2e/real_e2e.mjs <box_base> <hub_base> <token>   # in another shell
# or open hub_base in a real browser and "+ Add box" with box_base + token by hand
```
