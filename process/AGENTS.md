# Managed process instructions

These rules apply to `process/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths from that root. Read
`docs/design/managed-processes.md` before a lifecycle change.

## Manager lifecycle

The process manager is box-scoped and shared across sessions.

- Preserve the starting, ready, running, exited, and stopped states.
- Detect child exit asynchronously.
- Stop the Unix process group, not only its leader.
- Keep runtime declarations in memory. Do not write them into project config.
- Keep logs under the configured work directory.
- A restarted name is a new instance. `WaitExit` must return the terminal state
  of the instance that the caller observed.

## Status and engine integration

The engine exposes process state through a runtime-only `EngineContext` part.
Do not make the process package depend on `engine` or `message` to produce it.

## Tests

This package tests real subprocess machinery, so it can use the root
cross-process timing exception. Route polling through `internal/testpoll`.
Never add an inline sleep loop. Use in-process signals when a state is already
observable without crossing the OS process boundary.
