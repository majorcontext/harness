# End-to-end test instructions

These rules apply to `e2e/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths from that root.

## Cross-process timing exception

End-to-end tests may observe out-of-process state with a deadline-bounded poll.
No in-process signal crosses that boundary.

Every poll must use `internal/testpoll`. Do not write an inline sleep loop.
The timeout is a failure bound. Return on the first successful check.

Do not use this exception for engine, manager, queue, or server state that has
an in-process notification seam.

## Test scope

Start a real subprocess only when the process boundary is the behavior under
test. Keep fixtures local and deterministic. Do not require live provider
credentials in the ordinary suite.

Run end-to-end tests with `-race`. Preserve useful subprocess output on
failure, and mask secret values.
