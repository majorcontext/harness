# Local tool instructions

These rules apply to `tools/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths and commands from that root.

The hub, monitor, and inspector are operator tools. They are not deployed
multi-user products. Keep each page build-free and dependency-free unless a
separate design changes that constraint.

## Shared browser rules

- Keep API data real. Do not invent telemetry.
- Treat run tokens as secrets.
- Keep state and route codecs in pure helpers.
- Test the exact committed HTML.
- Wait for conditions in Node tests. Do not use fixed sleeps.
- Keep CORS and CSP requirements explicit.
- Do not rename renderer-owned CSS classes during a styling-only change.

## Development hub

The hub is a stateless fleet control surface.

- Browser URL-fragment state owns the box registry and current selection.
- The browser calls each box directly.
- The Go server exposes only the embedded page and `POST /spawn`.
- Bind loopback by default.
- Verify browser `Origin` against `Host` before a spawn.
- Keep the page CSP strict while allowing connections to operator-added boxes.
- Treat a shared hub URL as a secret because its fragment contains run tokens.

### Spawn-command contract

The spawn command emits `TUNNEL_URL`, `RUN_TOKEN`, and optional
`PORT_URL_<port>` lines. Pass the selected box name through
`HARNESS_HUB_BOX_NAME`. Harness itself does not consume that variable.

Run:

```bash
node --test tools/hub/*_test.mjs
go test -race ./tools/hub/...
```

Read `tools/hub/e2e/README.md`, `docs/design/fleet-model.md`, and the
"Development hub" section in
`docs/development-interfaces.md` before a behavior change.

### Hub UI design language

The hub uses a dark tactical-telemetry style.

- Use the existing black, phosphor, hairline, square geometry.
- Reserve red for hazards and destructive actions.
- Reserve green for live or successful goal execution.
- Keep body text monospace and headings heavy uppercase.
- Do not add gradients, soft shadows, rounded corners, or decorative metadata.
- Do not add emoji or em dashes to hub UI strings.

## Session monitor

The monitor is a single-box live board.

- Keep the standalone `file://` and static-host path working.
- Keep `GET /monitor` as the same-origin embedded path.
- Store manual connection settings in the documented local-storage keys.
- Keep route state in explicit fragment parameters.
- Adopt a `#t=` token into storage, then scrub it from the visible URL.
- Do not connect an embedded page to a different origin under its same-origin
  CSP.
- Keep the testable helper region stable.

Run:

```bash
node --test tools/monitor/*_test.mjs
go test -race ./tools/monitor/...
```

Read `tools/monitor/e2e/README.md`,
`docs/development-interfaces.md`, and the approved monitor
mockup before a layout change.

### Monitor UI design language

The monitor uses the instrument-sheet design, not the hub design.

- Preserve the light-first OKLCH token system and its dark variant.
- Reserve green, amber, and red for state.
- Reserve the filled accent for the send action.
- Keep `docs/design/monitor-mockup.html` as the visual specification.

## Inspector

Follow the same build-free, pure-helper, and no-fixed-sleep rules. Do not apply
the hub theme unless a dedicated inspector design requests it.
