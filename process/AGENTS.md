# Managed process

Read the root AGENTS.md. Read `docs/design/managed-processes.md` before a lifecycle change.

- The manager is box-scoped and shared across sessions.
- Preserve the starting, ready, running, exited, and stopped states.
- Detect child exit asynchronously.
- Stop the Unix process group, not only its leader.
- Keep runtime declarations in memory. Never write them to project config.
- Keep logs under the configured work directory.
- A restarted name is a new instance. `WaitExit` returns the state of the observed instance.
- This package must not import `message`. The runtime renders status.
