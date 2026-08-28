# Development interfaces

This document describes the local hub and session monitor.

## Development hub

`harness hub` is a local, single-operator control surface over a FLEET of
`harness serve` boxes — a fleet dashboard for "what are my agents
doing right now" and for dispatching new goal-supervised sessions, not a
deployed product. It serves one embedded, single-file page
(`tools/hub/index.html`, `go:embed`) on
`localhost:7777` by default (`-addr` to change it).

- **No server-side state.** The hub keeps no registry and reads no config
  file: every box (name, base URL, run token) and the current selection
  live only in that browser tab's URL fragment, base64-encoded JSON
  (`#s=...`), kept in sync via `history.replaceState`. That makes a hub URL
  bookmarkable and shareable between local tabs with zero persistence code
  — and means **run tokens ride the URL by design**; treat a hub link like
  a secret.
- **The page talks to boxes directly** from the browser, over each box's
  normal HTTP+SSE API (`server/openapi.yaml`) — never proxied through the
  hub's own server. Every box must therefore be started with `-cors-origin`
  set to the hub's origin (or `*` for local hacking), e.g. `harness serve
  -cors-origin http://localhost:7777`; a box without it will look
  permanently unreachable from the hub.
- **The Go side is minimal on purpose**, exactly one API: `POST /spawn`.
  It execs the command given by `-spawn-command` (or `$HARNESS_HUB_SPAWN`)
  via `sh -c` and streams its combined stdout+stderr live to the page over
  SSE. The **spawn-command contract** — the only coupling between this repo
  and any deployment-specific provisioning tool — is plain lines anywhere
  in that output: `TUNNEL_URL=<url>` and `RUN_TOKEN=<token>` (required to
  add the box), and any number of `PORT_URL_<port>=<url>` lines (optional —
  one per exposed port's own tunnel/preview URL, collected into a
  `port_urls` map; see the process strip in `tools/hub/index.html`'s header
  comment). Once the command exits, the stream ends with a summary carrying
  those values (if found) and the exit code; the page adds the new box to
  its own URL state itself. Nothing box-provisioning-specific lives in this
  repo.
  - **Box name passthrough.** `POST /spawn`'s JSON body optionally carries
    `{"name": "..."}` — the page's generated (or, on a Respawn/ADOPT, reused)
    box name. The Go handler sets it as `HARNESS_HUB_BOX_NAME` in the spawn
    command's own environment (`tools/hub/spawn.go`'s `runSpawn`), exactly
    the deployment-environment contract `docs/design/fleet-model.md` §8
    specifies: deployment tooling invoked by `-spawn-command` reads this
    variable to derive per-name storage (typically setting
    `HARNESS_SESSION_DIR` from it before `harness serve` starts) — harness's
    own code never reads `HARNESS_HUB_BOX_NAME` at all. A request with no
    body, or no `name` field, spawns exactly as before (no env var set).
- The hub binds loopback-only by default (`resolveAddr` in `tools/hub/hub.go`).
- **Browser-security hardening** (both in `tools/hub/hub.go`, tested in
  `tools/hub/hub_test.go`). `POST /spawn` execs a real, costly provision
  command, so `handleSpawn` rejects a browser cross-origin request before any
  exec: if an `Origin` header is present it must match the request's `Host`
  (OWASP verify-origin). Loopback binding alone does not stop this — any page
  the operator visits can `fetch("http://localhost:7777/spawn",{method:
  "POST"})` as a no-preflight CORS simple request — but the page's own
  same-origin `fetch("/spawn")` (Origin == Host) and non-browser clients (no
  Origin, so not a CSRF vector) pass unchanged. The served page also carries
  a strict `Content-Security-Policy` (`default-src 'none'` + `'unsafe-inline'`
  script/style — the page is a single no-build `go:embed`'d file with no
  external resources and no per-response nonce hook — + `connect-src *`,
  required because it fetches/streams from arbitrary operator-added box
  origins the stateless hub cannot enumerate, + `frame-ancestors`/`base-uri`/
  `form-action` pinned to `'none'`): defense-in-depth for a page holding run
  tokens in its URL fragment.
- **Pure hub logic is unit-tested** by `tools/hub/hub_test.mjs` (run:
  `node --test tools/hub/*_test.mjs`). **End-to-end, against a real backend**
  is `tools/hub/e2e` (see its README): a `go test -race ./...` subtree that
  starts an actual `server.Server` + `hub.NewHandler` and drives the real,
  served `index.html` with Node + jsdom and an unmocked `fetch` — no manual
  setup step; it installs its own `npm` dependency on first run.

### UI design language

The hub is styled as **tactical telemetry** — a committed dark-only
brutalist archetype (derived from the public
[taste-skill](https://github.com/Leonxlnx/taste-skill) brutalist +
anti-slop skills). Any new hub UI — and future passes on the inspector,
which still wears the older soft theme — follows these rules:

- **One substrate, no theme toggle**: `#0a0a0a` background, `#eaeaea`
  phosphor foreground, `#2a2a2a` hairline borders. Never reintroduce a
  light mode here; pick-one-and-commit is the point.
- **Two semantic colors only.** Hazard red (`--accent`, `#ff2a2a`) means
  trouble or destructive action, nothing else. Terminal green (`--ok`,
  `#4af626`) is reserved for exactly one semantic: live or succeeded goal
  execution. Everything else is monochrome.
- **Monospace dominance**: body text is the `ui-monospace` stack;
  headers are heavy uppercase system-ui. Micro-labels are uppercase with
  `.06–.1em` tracking. No webfonts — the page is CSP-self-contained.
- **Geometry**: `border-radius: 0` absolutely everywhere; square status
  markers; 1px compartment borders; inverted-video hover
  (foreground/background swap). No gradients, soft shadows, or
  translucency. The scanline overlay is static — motion requires a
  stated purpose.
- **Copy discipline**: no emoji in UI strings, no em-dashes anywhere, and
  every piece of "telemetry" displayed must be real data (vcs revisions,
  seqs, PIDs, token counts) — never decorative or fabricated metadata.
- **Selectors are load-bearing**: the renderers create elements by class
  name (`.sess`, `.box-card`, `.dot`, `.goalnarr`, …). Restyle classes;
  never rename them in a styling pass.

## Session monitor

`tools/monitor` (`tools/monitor/index.html`) is the single-instance
counterpart to the hub above: where the hub answers "what are my boxes doing"
across a FLEET, the monitor answers "what is THIS `harness serve` instance
doing right now" — a live board of every session on that one box (phase,
current tool, staleness), a per-session detail view with a scrolling
transcript, and a composer to speak into a session (`prompt_async`). Like the
hub and the inspector, it is a build-free, dependency-free single HTML file
with no Go-side handler of its own.

- **How to run it**: open the file directly (`file://`) or serve it from any
  static host — nothing box-specific is baked in. The target box must be
  started with `-cors-origin` covering the monitor's origin (or `*` for local
  hacking), exactly like the hub's requirement above; a box without it looks
  permanently unreachable. The base URL and run token are entered in the
  page itself and persisted to `localStorage` in plaintext (same documented
  tradeoff as the inspector — a dev tool, not for a shared origin with a
  long-lived token) so a reload can reconnect automatically. Routing lives in
  the URL fragment as small explicit params, not the hub's base64 blob:
  `#b=<base>` (box base URL) and `#s=<session id>` (open detail view) — both
  bookmarkable, encoded/decoded by a tested pure helper.
- **Embedded serving, frictionless local**: every `harness serve` box also
  offers its own copy same-origin, at `GET /monitor` — by default
  `http://localhost:4096/monitor` (the port follows `-addr`, default
  `localhost:4096`; the exact URL, with any `#t=<token>` capability suffix, is
  printed to the terminal on startup — `monitorTerminalHint`). The bare root
  is a convenience redirect: `GET /` 302s to `/monitor` (via the `GET /{$}`
  route — `{$}`-anchored to the root path only, never a catch-all — registered
  under the same `MonitorPage` guard, so a pure-API box keeps `/` a clean 404).
  `tools/monitor`
  (package `monitor`, `embed.go`) `//go:embed`s the exact committed
  `index.html`; `cmd/harness`'s `serveCmd` wires it into
  `server.Options.MonitorPage`, which the server serves unauthenticated
  (like `/health` — the page itself carries no secrets) with a
  same-origin-scoped `Content-Security-Policy` (`connect-src 'self'`).
  `server` itself never imports `tools/monitor` (layering: `server` must
  not import `tools/*`); only `cmd/harness` does, the same pattern
  `harness hub` already uses. The `file://`/static-host path is unaffected
  — this is additive, and stays the only option for monitoring a box from a
  different origin (the embedded route's CSP deliberately does not permit
  that).
  - **Unauthenticated-on-loopback** (`server.Options.Unauthenticated`, an
    EXPLICIT opt-in never inferred from an empty `RunToken` on its own —
    `New` still fails closed otherwise): `serveCmd` classifies `-addr`
    (`isLoopbackAddr` — `localhost`, `127.0.0.1`/`::1`, any
    `net.IP.IsLoopback()` address; a bare `:port`, `0.0.0.0`, `::`, or any
    other routable address is NOT loopback). `HARNESS_RUN_TOKEN` unset +
    loopback bind runs the box fully unauthenticated (every route, not just
    `/health`/`/monitor`) and logs a clear warning; unset + non-loopback
    still hard-errors `HARNESS_RUN_TOKEN is required` exactly as before. The
    token guards network reachability, and loopback is a server-verifiable
    proxy for that (unlike, say, `Origin`, which a client controls).
  - **Unauthenticated on a non-loopback bind is also possible, but ONLY via
    a SECOND, separate, EXPLICIT opt-in** — the `-unauthenticated` serve
    flag, or `HARNESS_UNAUTHENTICATED=1` (`envUnauthenticated`, parsed with
    `strconv.ParseBool`; an unset or unparsable value is false, fail-closed
    on a malformed setting). `resolveUnauthenticated` (`cmd/harness/main.go`)
    is the single decision point both serve flags feed: a non-empty token
    always wins (token path unaffected, any bind); an empty token on a
    loopback bind is unchanged from above; an empty token on a non-loopback
    bind needs this opt-in or it still hard-errors exactly as before — the
    opt-in is never inferred from the empty token alone, only from the flag
    or env var. This is for a deployment where a trusted external gate
    already restricts reachability (e.g. a Cloudflare Access-gated tunnel,
    or a sandboxed network boundary) so the token is redundant with that
    gate — `server.Options.Unauthenticated` itself is bind-address-agnostic
    (see its own doc comment); the safety property lives entirely in
    `cmd/harness` deciding WHEN to set it. Landing this on a non-loopback
    bind logs a SEPARATE, distinctly worded loud warning ("serving
    unauthenticated on a non-loopback bind") from the loopback one above, so
    the two are distinguishable in a log search.
  - **Same-origin auto-connect** (`embeddedConnectPlan`, index.html): opening
    a box's own `/monitor` attempts the connection immediately against
    `location.origin`, using whatever token is available (`#t=` fragment,
    then a stored one, then none) — success lands straight on the board, no
    panel, nothing typed (covers both the unauthenticated-loopback case and
    a valid capability URL/returning operator with the SAME call). Only a
    failed attempt (a real token is required and none was available) falls
    back to a minimal token-only panel — host is already known, so the base
    field never reappears. A `#t=<token>` fragment (mirroring `tools/hub`'s
    "run tokens ride the URL by design" precedent, `extractFragmentToken`)
    is adopted into the SAME `localStorage` key manual entry uses and
    immediately scrubbed from the visible URL via `history.replaceState`; a
    fragment `#b=` naming a DIFFERENT origin than this box's own — which the
    embedded route's CSP would block outright if tried — surfaces a
    plain-text notice instead of attempting it, composing with (never
    blocking) the own-origin auto-connect. None of this weakens the auth
    model itself: a token is still required and still checked exactly as a
    hand-typed one would be, on every box that hasn't explicitly opted into
    `Unauthenticated`.
  - On a TTY, `serveCmd` also prints a click-ready line to stderr after
    "serve start" — `monitor: http://<addr>/monitor#t=<token>` (or, when
    running unauthenticated-loopback, the plain URL with no `#t=` at all,
    since there's no credential to carry) — gated on `stderrIsTerminal`
    (`os.ModeCharDevice`, stdlib-only) so a tokenized URL never lands in
    piped/production stderr, only an interactive operator's own terminal.
- **Test layers.** Pure helpers (SSE parser, activity reducer, transcript
  fold, route codec, formatters) live inside index.html's `/* TESTABLE-BEGIN
  */ … /* TESTABLE-END */` region and are covered by
  `tools/monitor/monitor_test.mjs` (run: `node --test tools/monitor/*_test.mjs`),
  using the same extraction-and-`vm`-evaluate pattern as the inspector's
  `inspector_test.mjs` (and now the hub's, above) — no build step, so the
  tests read the region straight out of the committed HTML. End-to-end,
  against a real backend, is `tools/monitor/e2e` (see its README): a `go
  test` subtree that starts a real `server.Server` plus a plain static file
  server for the actual committed `index.html`, and drives it with Node +
  jsdom and an unmocked `fetch` — mirroring `tools/hub/e2e`'s structure and
  conventions. A `window.__monitorTuning = {QUIET_MS, STALL_MS}` seam (set
  via jsdom's `beforeParse` before the page's own script runs, a no-op in
  production since nothing else ever sets it) lets both the unit and e2e
  suites shrink the staleness thresholds so `quiet`/`stalled` transitions are
  observable in test time instead of real minutes.
- **UI design language.** The monitor deliberately does NOT inherit the
  hub's committed dark brutalist archetype above. It carries its own
  "instrument sheet" language: light-first with a dark variant, both driven
  by one OKLCH token set (`--surface`, `--text-1..3`, `--separator`, etc.);
  semantic green/amber/red (`--ok`/`--warn`/`--critical`) are reserved
  strictly for session/staleness state, never decoration; a single accent
  color is owned by interaction (the composer's send affordance is the
  page's only filled-accent control). `docs/design/monitor-mockup.html` is
  the user-approved visual spec — its tokens, grid template, and markup
  shapes are binding; restyle within that spec rather than importing the
  hub's theme onto it.
