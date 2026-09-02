# Development interfaces

This document describes the local development hub.

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
