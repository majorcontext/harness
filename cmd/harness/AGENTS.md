# Harness command instructions

These rules apply to `cmd/harness/`. Harness does not merge ancestor files.
If root guidance is not active, locate the Git root and read
`<repo-root>/AGENTS.md`. Resolve repository paths from that root.

## Composition root

Keep the command package thin. It resolves config, constructs providers and
managers, and composes the engine, server, and embedded local tools.

Do not move engine behavior into command handlers. Do not make `server` import
`tools/*`; inject pages and dependencies through options.

## Startup budget

Keep `harness --version` on the millisecond startup path.

- Do not add network access at startup.
- Before first output, limit disk reads to the user and project config files.
- Do not start a long-lived plugin process before its first hook or tool call.
  A bounded manifest probe can run for a missing or stale cache entry.
- Do not start MCP servers or provider clients before first use.
- Do not scan skills or project instructions at `NewSession`.
- Do not add `init()` side effects.
- Keep plugin manifests and model metadata local and static on the hot path.
- Keep production dependencies pure Go.

Run the startup budget tests after a change to command initialization.

## Config and environment resolution

The command layer resolves environment variables. The engine must not read
operator environment variables directly.

Keep one decision point for each config precedence rule. Preserve explicit
zero, negative opt-out, and unset distinctions.

Validate adapter-only fields against the adapter that an entry builds, not the
provider map key alone. Fail loudly on an unreadable or unknown value.

Keep run and serve wiring in parity for shared engine settings.

## Provider construction

A provider-map key is the model-reference family. Pass that family into native
Responses clients so opaque data cannot cross endpoints.

Do not validate credentials during registry construction. The first provider
request owns credential validation.

## Monitor and hub composition

`cmd/harness` may import `tools/hub` and `tools/monitor`. The server may
not.

Only allow empty-token unauthenticated service when `resolveUnauthenticated`
proves loopback or receives the explicit non-loopback opt-in. Keep the two
warning messages distinct.

Print tokenized monitor URLs only to an interactive terminal. Do not write a
tokenized URL to piped production logs.

## GC and pprof diagnostics

Use `runtime/metrics` for GC pause observation. Do not use
`runtime.ReadMemStats` in the watcher.

Never import `net/http/pprof`. Keep the command-level default-mux regression
test because the binary import graph is wider than the server package.

## Tests

Test flag and environment precedence as tables. Cover malformed values and
explicit zero values. Do not use a live provider or a real deployment command
in ordinary command tests.
