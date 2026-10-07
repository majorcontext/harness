# Fleet and serve diagnostics

This document describes box identity, task lineage, provider exhaustion, and
the serve diagnostics. The design is in [architecture.md](architecture.md).
`docs/design/fleet-model.md` holds the deployment rationale.

## Box identity

An operator chooses the name of a box. One volume or directory belongs to one
name, and `HARNESS_SESSION_DIR` points `harness serve` at it. Two servers that
are alive at the same time never share one directory. A box is ephemeral
compute that serves one name. The name and its volume are durable.

A new box over the same volume continues the history of the old one. At start,
`harness serve` calls `CatchUp` and opens each stored session that has work to
resume: a running or suspended turn, a queued input, an active goal, or an
unsettled child. A session whose log fails to replay is logged and skipped. It
does not stop `serve`.

The external orchestrator that spawns boxes gives the box name to the spawn
command as `HARNESS_HUB_BOX_NAME`, so a deployment script can derive per-name
storage from it. Harness never reads this variable. It is a contract between
the orchestrator and the deployment tooling. No component of this repository
implements the orchestrator side.

## Task lineage

Lineage is durable. A child session is a session of its own, so `GET
/sessions/{id}` reports its `parent_id` and its `agent`. The `child.spawned`
event is appended to the log of the parent. The lineage of a task survives a
restart.

## Failed children and provider exhaustion

The report of a failed child carries the cause, not only a class. The reason
is a fixed prefix that names the class of the error, then the error text:

| Class of the last turn | Reason prefix |
|---|---|
| Usage limit (`provider_exhausted`) | `provider capacity exhausted for this account` |
| Rate limit that outlasted the retries | `provider rate limit outlasted the retry budget for this account` |
| Permanent provider error | `turn failed with a permanent provider error and cannot succeed on retry` |
| Unrecovered error | `turn failed and did not recover` |
| Crash | `lost to restart: turn was in flight when the process last stopped` |

A prefix alone is not enough for a parent to act on, because one prefix
covers many causes. A permanent 400 can be a malformed request, a quota
rejection, or a policy refusal.

**Provider exhaustion is not a child failure.** A wall at the level of the
account (a usage limit, a quota, a credit balance, a spend cap) stops every
session on the same key at the same time. The child and its work are intact
and can run again when the provider recovers. A parent that treats the wall
as an ordinary failure starts a replacement into the same wall. Three layers
carry the fact:

- The adapter classifies it. `provider/anthropic` and `provider/openai`
  return `provider.Error{Kind: ErrKindProviderExhausted, RecoverHint}` for the
  message shapes of a spent supply. A per-minute throttle is not a spent
  supply. Matching on message text is allowed only inside the adapter.
- The session reads the typed kind, never text. A turn that fails with it
  ends with the cause `provider_exhausted`. A rate limit that outlasted the
  retry budget gets the same guidance in the report.
- The report tells the parent what to do. The line of the child in the
  `[tasks: ...]` segment ends with `provider exhausted, child preserved: do
  not spawn a replacement`, the instruction to resume the same child with
  `task send` on its `session_id`, and the recover-at hint when the provider
  gave one.

The `task` tool `log` action returns the last entries of a descendant, living
or dead. `tail` defaults to 20, a value above 100 is capped at 100, and a
negative value is an error. The entries fill newest first under a total size
budget, so the messages nearest a failure survive.

## Serve diagnostics

A caller that waits for seconds cannot tell from outside whether `harness
serve` was slow, the network in front of it was slow, or garbage collection
stopped the process. Each diagnostic is a threshold-gated log line. Nothing
runs always on.

- `slow store phase`: a store operation took over 1 s. `store phase in
  flight`: an operation is still running after 5 s, repeated every 5 s. A
  wedged volume hangs a file operation with no error, so a line at completion
  never comes. `cmd/harness/storelog.go` wraps the store.
- `long gc pause`: a stop-the-world pause stops every goroutine, so the
  process logs nothing while it lasts and looks like a wedged handler.
  `cmd/harness/gcwatch.go` samples the `/gc/pauses:seconds` histogram of
  `runtime/metrics` every 5 s and warns about a new pause of 200 ms or more.
  It never calls `runtime.ReadMemStats`, because that call stops the world.
  The first sample reports nothing, because the counts are cumulative for the
  life of the process.
- The config summary at start echoes `session_sync=volume` when it is set.

`harness serve` has no `-pprof` flag and serves no `/debug` route. Never
import `net/http/pprof` in this repository: its `init` registers
`/debug/pprof/*` on `http.DefaultServeMux` for every program that links it.

No metrics, no tracing, no always-on profiling. A new diagnostic in this area
is a threshold-gated log line or it does not land.
